package raft

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fortytw2/leaktest"
	"github.com/santhosh-tekuri/fnet"
	"github.com/santhosh-tekuri/raft/inmem"
)

func TestRaft_Voting(t *testing.T) {
	debug("\nTestRaft_Voting --------------------------")
	defer leaktest.Check(t)()
	c := newCluster(t)
	c.launch(3)
	defer c.shutdown()
	ldr := c.ensureHealthy()

	req := &voteRequest{}
	ldr.inspect(func(r *Raft) {
		req.term = r.term
		req.lastLogIndex = r.lastLogIndex
		req.lastLogTerm = r.lastLogTerm
	})

	var followers []*member
	for _, m := range ldr.config.members() {
		if m.addr != ldr.addr {
			followers = append(followers, m)
		}
	}

	// a follower that thinks there's a leader should vote for that leader
	req.candidateID = ldr.addr
	resp, err := followers[0].requestVote(req)
	if err != nil {
		t.Fatalf("requestVote failed: %v", err)
	}
	if !resp.granted {
		t.Fatalf("voteGranted: got %t, want true", resp.granted)
	}

	// a follower that thinks there's a leader shouldn't vote for a different candidate
	req.candidateID = followers[0].addr
	resp, err = followers[1].requestVote(req)
	if err != nil {
		t.Fatalf("requestVote failed: %v", err)
	}
	if resp.granted {
		t.Fatalf("voteGranted: got %t, want false", resp.granted)
	}
}

func TestRaft_SingleNode(t *testing.T) {
	debug("\nTestRaft_SingleNode --------------------------")
	defer leaktest.Check(t)()
	c := newCluster(t)
	c.launch(1)
	defer c.shutdown()
	ldr := c.ensureHealthy()

	// should be able to apply
	resp, err := ldr.waitApply("test", c.heartbeatTimeout)
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}

	// check response
	if resp.msg != "test" {
		t.Fatalf("apply response mismatch. got %s, want test", resp.msg)
	}

	// check index
	if resp.index != 1 {
		t.Fatalf("fsmReplyIndex: got %d want 1", resp.index)
	}
	if idx := ldr.fsmMock().len(); idx != 1 {
		t.Fatalf("fsm.len: got %d want 1", idx)
	}

	// check that it is applied to the FSM
	if cmd := ldr.fsmMock().lastCommand(); cmd != "test" {
		t.Fatalf("fsm.lastCommand: got %s want test", cmd)
	}
}

func TestRaft_TripleNode(t *testing.T) {
	debug("\nTestRaft_TripleNode --------------------------")
	defer leaktest.Check(t)()
	c := newCluster(t)
	c.launch(3)
	defer c.shutdown()
	ldr := c.ensureHealthy()

	// should agree on leader
	c.ensureLeader(ldr.addr)

	// should be able to apply
	resp, err := ldr.waitApply("test", c.heartbeatTimeout)
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if resp.msg != "test" {
		t.Fatalf("apply response mismatch. got %s, want test", resp.msg)
	}
	if resp.index != 1 {
		t.Fatalf("fsmReplyIndex: got %d want 1", resp.index)
	}
	c.ensureFSMReplicated(1)
}

func TestRaft_LeaderFail(t *testing.T) {
	debug("\nTestRaft_LeaderFail --------------------------")
	defer leaktest.Check(t)()
	c := newCluster(t)
	c.launch(3)
	defer c.shutdown()
	ldr := c.ensureHealthy()

	// should agree on leader
	c.ensureLeader(ldr.addr)

	// should be able to apply
	resp, err := ldr.waitApply("test", c.heartbeatTimeout)
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if resp.msg != "test" {
		t.Fatalf("apply response mismatch. got %s, want test", resp.msg)
	}
	if resp.index != 1 {
		t.Fatalf("fsmReplyIndex: got %d want 1", resp.index)
	}
	c.ensureFSMReplicated(1)

	// disconnect leader
	ldrTerm := ldr.getTerm()
	c.disconnect(ldr)

	// leader should stepDown
	if !ldr.waitForState(c.longTimeout, follower, candidate) {
		t.Fatal("leader should stepDown")
	}

	// wait for new leader
	c.ensureStability()
	newLdr := c.leader()
	if newLdr == ldr {
		t.Fatalf("newLeader: got %s, want !=%s", newLdr.addr, ldr.addr)
	}

	// ensure leader term is greater
	if newLdrTerm := newLdr.getTerm(); newLdrTerm <= ldrTerm {
		t.Fatalf("expected new leader term: newLdrTerm=%d, ldrTerm=%d", newLdrTerm, ldrTerm)
	}

	// apply should work not work on old leader
	_, err = ldr.waitApply("reject", c.heartbeatTimeout)
	if err, ok := err.(NotLeaderError); !ok {
		t.Fatalf("got %v, want NotLeaderError", err)
	} else if err.Leader != "" {
		t.Fatalf("got %s, want ", err.Leader)
	}

	// apply should work on new leader
	if _, err = newLdr.waitApply("accept", c.heartbeatTimeout); err != nil {
		t.Fatalf("got %v, want nil", err)
	}

	// reconnect the networks
	c.connect()
	c.ensureHealthy()

	// wait for log replication
	c.ensureFSMReplicated(2)

	// Check two entries are applied to the FSM
	c.ensureFSMSame([]string{"test", "accept"})
}

func TestRaft_BehindFollower(t *testing.T) {
	debug("\nTestRaft_BehindFollower --------------------------")
	defer leaktest.Check(t)()
	c := newCluster(t)
	c.launch(3)
	defer c.shutdown()
	ldr := c.ensureHealthy()

	// should agree on leader
	c.ensureLeader(ldr.addr)

	// disconnect one follower
	behind := c.followers()[0]
	c.disconnect(behind)

	// commit a lot of things
	for i := 0; i < 100; i++ {
		ldr.TasksCh <- ApplyEntry([]byte(fmt.Sprintf("test%d", i)))
	}
	if _, err := ldr.waitApply("test100", c.longTimeout); err != nil {
		t.Fatal(err)
	}

	// reconnect the behind node
	c.connect()
	c.ensureHealthy()

	// ensure all the logs are the same
	c.ensureFSMReplicated(101)
	c.ensureFSMSame(nil)

	// Ensure one leader
	c.ensureLeader(c.leader().addr)
}

func TestRaft_ApplyNonLeader(t *testing.T) {
	debug("\nTestRaft_ApplyNonLeader --------------------------")
	defer leaktest.Check(t)()
	c := newCluster(t)
	c.launch(3)
	defer c.shutdown()
	ldr := c.ensureHealthy()

	// should agree on leader
	c.ensureLeader(ldr.addr)

	// apply should work not work on non-leader
	for _, r := range c.rr {
		if r != ldr {
			_, err := r.waitApply("reject", c.commitTimeout)
			if err, ok := err.(NotLeaderError); !ok {
				t.Fatalf("got %v, want NotLeaderError", err)
			} else if err.Leader != ldr.addr {
				t.Fatalf("got %s, want %s", err.Leader, ldr.addr)
			}
		}
	}
}

func TestRaft_ApplyConcurrent(t *testing.T) {
	debug("\nTestRaft_ApplyConcurrent --------------------------")
	defer leaktest.Check(t)()
	c := newCluster(t)
	c.launch(3)
	defer c.shutdown()
	ldr := c.ensureHealthy()

	// should agree on leader
	c.ensureLeader(ldr.addr)

	// concurrently apply
	wg := sync.WaitGroup{}
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := ldr.waitApply(fmt.Sprintf("test%d", i), 0); err != nil {
				debug("FAIL got", err, "want nil")
				t.Fail() // note: t.Fatal should note be called from non-test goroutine
			}
		}(i)
	}

	// wait to finish
	doneCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		break
	case <-time.After(c.longTimeout):
		t.Fatal("timeout")
	}

	// check If anything failed
	if t.Failed() {
		t.Fatal("one or more of the apply operations failed")
	}

	// check the FSMs
	c.ensureFSMReplicated(100)
	c.ensureFSMSame(nil)
}

func TestMain(m *testing.M) {
	code := m.Run()

	// wait until all pending debug messages are printed to stdout
	debug("barrier")

	os.Exit(code)
}

// ---------------------------------------------

type cluster struct {
	*testing.T
	rr               []*Raft
	network          *fnet.Network
	heartbeatTimeout time.Duration
	longTimeout      time.Duration
	commitTimeout    time.Duration
}

func newCluster(t *testing.T) *cluster {
	return &cluster{
		T:                t,
		heartbeatTimeout: 50 * time.Millisecond,
		longTimeout:      5 * time.Second,
		commitTimeout:    5 * time.Millisecond,
	}
}

func (c *cluster) launch(n int) {
	c.Helper()
	c.network = fnet.New()
	addrs := make([]string, n)
	for i := range addrs {
		host := string('A' + i)
		addrs[i] = host + ":8888"
	}

	c.rr = make([]*Raft, n)
	for i := range c.rr {
		storage := new(inmem.Storage)
		r := New(addrs[i], &fsmMock{}, storage, storage)
		_, err := r.storage.bootstrap(addrs, r.dialFn, 10*time.Second) // todo: timeout
		if err != nil {
			c.Fatalf("storage.bootstrap failed: %v", err)
		}

		r.heartbeatTimeout = c.heartbeatTimeout
		c.rr[i] = r

		// switch to fake transport
		host := c.network.Host(string('A' + i))
		r.listenFn, r.dialFn = host.Listen, host.DialTimeout
		for _, m := range r.config.members() {
			m.dialFn = host.DialTimeout
		}

		if err := r.Listen(); err != nil {
			c.Fatalf("raft.listen failed: %v", err)
		}
		go func() { _ = r.Serve() }()
	}
}

func (c *cluster) ensureStability() {
	c.Helper()
	limitTimer := time.NewTimer(c.longTimeout)
	startupTimeout := c.heartbeatTimeout
	electionTimer := time.NewTimer(startupTimeout + 2*c.heartbeatTimeout)

	stateChanged := make(chan struct{}, 10)
	onStateChange(func(r *Raft) {
		select {
		case stateChanged <- struct{}{}:
		default:
		}
	})
	defer onStateChange(nil)

	onVoteRequest(func(r *Raft, req *voteRequest) {
		select {
		case stateChanged <- struct{}{}:
		default:
		}
	})
	defer onStateChange(nil)

	for {
		select {
		case <-limitTimer.C:
			c.Fatal("cluster is not stable")
		case <-stateChanged:
			electionTimer.Reset(2 * c.heartbeatTimeout)
		case <-electionTimer.C:
			return
		}
	}
}

func (c *cluster) getInState(state state) []*Raft {
	var rr []*Raft
	for _, r := range c.rr {
		if r.getState() == state {
			rr = append(rr, r)
		}
	}
	return rr
}

func (c *cluster) leader() *Raft {
	c.Helper()
	leaders := c.getInState(leader)
	if len(leaders) != 1 {
		c.Fatalf("got %d leaders, want 1 leader", len(leaders))
	}
	return leaders[0]
}

func (c *cluster) followers() []*Raft {
	c.Helper()
	followers := c.getInState(follower)
	if len(followers) != len(c.rr)-1 {
		c.Fatalf("got %d followers, want %d followers", len(followers), len(c.rr)-1)
	}
	return followers
}

func (c *cluster) ensureHealthy() *Raft {
	c.Helper()
	c.ensureStability()
	ldr := c.leader()
	c.followers()
	return ldr
}

func (c *cluster) ensureLeader(leaderID string) {
	c.Helper()
	for _, r := range c.rr {
		if got := r.getLeaderID(); got != leaderID {
			c.Fatalf("leaderID of %s: got %s, want %s", r.addr, got, leaderID)
		}
	}
}

func (c *cluster) waitForLeader(timeout time.Duration) (*Raft, error) {
	c.Helper()
	leaderCh := make(chan *Raft, 1)
	onStateChange(func(r *Raft) {
		if r.state == leader {
			select {
			case leaderCh <- r:
			default:
			}
		}
	})
	defer onStateChange(nil)

	// check if leader already chosen
	for _, r := range c.rr {
		if r.getState() == leader {
			return r, nil
		}
	}

	// wait until leader chosen
	select {
	case r := <-leaderCh:
		return r, nil
	case <-time.After(timeout):
		return nil, errors.New("waitForLeader: timedout")
	}
}

func (c *cluster) ensureFSMReplicated(len uint64) {
	c.Helper()
	limitCh := time.After(c.longTimeout)

	eventCh := make(chan struct{}, 1)
	eventCh <- struct{}{}
	onFSMApplied(func(*Raft, uint64) {
		select {
		case eventCh <- struct{}{}:
		default:
		}
	})
	defer onFSMApplied(nil)

loop:
	for {
		select {
		case <-eventCh:
			for _, r := range c.rr {
				if r.fsmMock().len() != len {
					continue loop
				}
			}
			return
		case <-limitCh:
			c.Fatal("timeout waiting for replication")
		}
	}
}

// if want==nil, we ensure all fsm are same
// if want!=nil, we ensure all fms has want
func (c *cluster) ensureFSMSame(want []string) {
	c.Helper()
	if want == nil {
		want = c.rr[0].fsmMock().commands()
	}
	for _, r := range c.rr {
		if got := r.fsmMock().commands(); !reflect.DeepEqual(got, want) {
			c.Fatalf("got %v, want %v", got, want)
		}
	}
}

func (c *cluster) disconnect(r *Raft) {
	host, _, _ := net.SplitHostPort(r.addr)
	debug("disconnecting", host)
	c.network.SetFirewall(fnet.Split([]string{host}, fnet.AllowAll))
}

func (c *cluster) connect() {
	debug("reconnecting")
	c.network.SetFirewall(fnet.AllowAll)
}

func (c *cluster) shutdown() {
	for _, r := range c.rr {
		r.Shutdown()
	}
}

// ---------------------------------------------

func (r *Raft) getState() state {
	var s state
	r.inspect(func(r *Raft) {
		s = r.state
	})
	return s
}

func (r *Raft) getTerm() uint64 {
	var term uint64
	r.inspect(func(r *Raft) {
		term = r.term
	})
	return term
}

func (r *Raft) getLeaderID() string {
	var leaderID string
	r.inspect(func(r *Raft) {
		leaderID = r.leaderID
	})
	return leaderID
}

// wait until state is one of given states
func (r *Raft) waitForState(timeout time.Duration, states ...state) bool {
	matchesState := func(curState state) bool {
		for _, s := range states {
			if curState == s {
				return true
			}
		}
		return false
	}

	ch := make(chan struct{}, 1)
	onStateChange(func(raft *Raft) {
		if raft == r && matchesState(r.state) {
			close(ch)
			onStateChange(nil)
		}
	})
	defer onStateChange(nil)
	if !matchesState(r.getState()) {
		select {
		case <-ch:
			break
		case <-time.After(timeout):
			return false
		}
	}
	return true
}

func (r *Raft) fsmMock() *fsmMock {
	return r.fsm.(*fsmMock)
}

func (r *Raft) waitTask(t Task, timeout time.Duration) (interface{}, error) {
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}
	select {
	case r.TasksCh <- t:
		break
	case <-timer:
		return nil, fmt.Errorf("waitApply(%v): submit timedout", t)
	}
	select {
	case <-t.Done():
		return t.Result(), t.Err()
	case <-timer:
		return nil, fmt.Errorf("waitApply(%v): result timedout", t)
	}
}

// use zero timeout, to wait till reply received
func (r *Raft) waitApply(cmd string, timeout time.Duration) (fsmReply, error) {
	result, err := r.waitTask(ApplyEntry([]byte(cmd)), timeout)
	if err != nil {
		return fsmReply{}, err
	}
	return result.(fsmReply), nil
}

func (r *Raft) inspect(fn func(*Raft)) {
	t := &task{
		fn: func(t Task, r *Raft) {
			fn(r)
			t.reply(nil)
		},
		done: make(chan struct{}),
	}
	_, _ = r.waitTask(t, 0)
}

// events ----------------------------------------------------------------------

var mu sync.RWMutex
var stateChangedFn func(*Raft)
var fsmAppliedFn func(*Raft, uint64)
var voteRequestFn func(*Raft, *voteRequest)

func init() {
	stateChanged = func(r *Raft) {
		mu.RLock()
		f := stateChangedFn
		mu.RUnlock()
		if f != nil {
			f(r)
		}
	}

	fsmApplied = func(r *Raft, index uint64) {
		mu.RLock()
		f := fsmAppliedFn
		mu.RUnlock()
		if f != nil {
			f(r, index)
		}
	}

	gotVoteRequest = func(r *Raft, req *voteRequest) {
		mu.RLock()
		f := voteRequestFn
		mu.RUnlock()
		if f != nil {
			f(r, req)
		}
	}
}

func onStateChange(f func(*Raft)) {
	mu.Lock()
	stateChangedFn = f
	mu.Unlock()
}

func onFSMApplied(f func(*Raft, uint64)) {
	mu.Lock()
	fsmAppliedFn = f
	mu.Unlock()
}

func onVoteRequest(f func(*Raft, *voteRequest)) {
	mu.Lock()
	voteRequestFn = f
	mu.Unlock()
}

// ---------------------------------------------

type fsmMock struct {
	mu   sync.RWMutex
	cmds []string
}

type fsmReply struct {
	msg   string
	index int
}

func (fsm *fsmMock) Apply(cmd []byte) interface{} {
	fsm.mu.Lock()
	defer fsm.mu.Unlock()
	fsm.cmds = append(fsm.cmds, string(cmd))
	return fsmReply{string(cmd), len(fsm.cmds)}
}

func (fsm *fsmMock) len() uint64 {
	fsm.mu.RLock()
	defer fsm.mu.RUnlock()
	return uint64(len(fsm.cmds))
}

func (fsm *fsmMock) lastCommand() string {
	fsm.mu.RLock()
	defer fsm.mu.RUnlock()
	if len(fsm.cmds) == 0 {
		return ""
	}
	return fsm.cmds[len(fsm.cmds)-1]
}

func (fsm *fsmMock) commands() []string {
	fsm.mu.RLock()
	defer fsm.mu.RUnlock()
	return append([]string(nil), fsm.cmds...)
}
