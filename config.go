package raft

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

type ConfigAction uint8

const (
	None ConfigAction = iota
	Promote
	Demote
	Remove
)

func (a ConfigAction) String() string {
	switch a {
	case None:
		return "None"
	case Promote:
		return "Promote"
	case Demote:
		return "Demote"
	case Remove:
		return "Remove"
	}
	return fmt.Sprintf("Action(%d)", a)
}

// Node represents a single node in raft configuration.
type Node struct {
	// ID uniquely identifies this node in raft cluster.
	ID uint64 `json:"-"`

	// Addr is network address that other nodes can contact.
	Addr string `json:"addr"`

	// Voter can participate in elections and its matchIndex
	// is used in advancing leader's commitIndex.
	Voter bool `json:"voter"`

	// Action tells the action to be taken by leader, when appropriate.
	Action ConfigAction `json:"action,omitempty"`
}

func (n Node) IsStable() bool {
	return n.Action == None
}

func (n Node) promote() bool {
	return !n.Voter && n.Action == Promote
}

func (n Node) demote() bool {
	return n.Voter && (n.Action == Demote || n.Action == Remove)
}

func (n Node) remove() bool {
	return !n.Voter && n.Action == Remove
}

func (n Node) encode(w io.Writer) error {
	if err := writeUint64(w, n.ID); err != nil {
		return err
	}
	if err := writeString(w, n.Addr); err != nil {
		return err
	}
	if err := writeBool(w, n.Voter); err != nil {
		return err
	}
	return writeUint8(w, uint8(n.Action))
}

func (n *Node) decode(r io.Reader) error {
	var err error
	if n.ID, err = readUint64(r); err != nil {
		return err
	}
	if n.Addr, err = readString(r); err != nil {
		return err
	}
	if n.Voter, err = readBool(r); err != nil {
		return err
	}
	if action, err := readUint8(r); err != nil {
		return err
	} else {
		n.Action = ConfigAction(action)
	}
	return nil
}

func (n Node) validate() error {
	if n.ID == 0 {
		return errors.New("id must be greater than zero")
	}
	if n.Addr == "" {
		return errors.New("empty address")
	}
	_, sport, err := net.SplitHostPort(n.Addr)
	if err != nil {
		return fmt.Errorf("invalid address %s: %v", n.Addr, err)
	}
	port, err := strconv.Atoi(sport)
	if err != nil {
		return errors.New("port must be specified in address")
	}
	if port <= 0 {
		return errors.New("invalid port")
	}
	if n.Action == Promote && n.Voter {
		return errors.New("voter can't be promoted")
	}
	if n.Action == Demote && !n.Voter {
		return errors.New("nonvoter can't be demoted")
	}
	return nil
}

// -------------------------------------------------

type Config struct {
	Nodes map[uint64]Node `json:"nodes"`
	Index uint64          `json:"index"`
	Term  uint64          `json:"term"`
}

func (c Config) IsBootstrap() bool {
	return c.Index == 0
}

func (c Config) IsStable() bool {
	for _, n := range c.Nodes {
		if !n.IsStable() {
			return false
		}
	}
	return true
}

func (c Config) nodeForAddr(addr string) (Node, bool) {
	for _, n := range c.Nodes {
		if n.Addr == addr {
			return n, true
		}
	}
	return Node{}, false
}

func (c Config) isVoter(id uint64) bool {
	n, ok := c.Nodes[id]
	return ok && n.Voter
}

func (c Config) numVoters() int {
	voters := 0
	for _, n := range c.Nodes {
		if n.Voter {
			voters++
		}
	}
	return voters
}

func (c Config) quorum() int {
	return c.numVoters()/2 + 1
}

func (c Config) clone() Config {
	nodes := make(map[uint64]Node)
	for id, n := range c.Nodes {
		nodes[id] = n
	}
	c.Nodes = nodes
	return c
}

func (c Config) encode() *entry {
	w := new(bytes.Buffer)
	if err := writeUint32(w, uint32(len(c.Nodes))); err != nil {
		panic(err)
	}
	for _, n := range c.Nodes {
		if err := n.encode(w); err != nil {
			panic(err)
		}
	}
	return &entry{
		typ:   entryConfig,
		index: c.Index,
		term:  c.Term,
		data:  w.Bytes(),
	}
}

func (c *Config) decode(e *entry) error {
	if e.typ != entryConfig {
		return fmt.Errorf("raft: expected entryConfig in Config.decode")
	}
	c.Index, c.Term = e.index, e.term
	r := bytes.NewBuffer(e.data)
	size, err := readUint32(r)
	if err != nil {
		return err
	}
	c.Nodes = make(map[uint64]Node)
	for ; size > 0; size-- {
		n := Node{}
		if err := n.decode(r); err != nil {
			return err
		}
		c.Nodes[n.ID] = n
	}
	c.Index, c.Term = e.index, e.term
	return nil
}

func (c Config) validate() error {
	addrs := make(map[string]bool)
	for id, n := range c.Nodes {
		if err := n.validate(); err != nil {
			return err
		}
		if id != n.ID {
			return fmt.Errorf("id mismatch for node %d", n.ID)
		}
		if addrs[n.Addr] {
			return fmt.Errorf("duplicate address %s", n.Addr)
		}
		addrs[n.Addr] = true
	}
	if c.numVoters() == 0 {
		return errors.New("zero voters")
	}
	return nil
}

func (c Config) String() string {
	var voters, nonvoters []string
	for _, n := range c.Nodes {
		s := fmt.Sprintf("%d,%s", n.ID, n.Addr)
		if n.Action != None {
			s = fmt.Sprintf("%s,%s", s, n.Action)
		}
		if n.Voter {
			voters = append(voters, s)
		} else {
			nonvoters = append(nonvoters, s)
		}
	}
	return fmt.Sprintf("index: %d, voters: %v, nonvoters: %v", c.Index, voters, nonvoters)
}

// ---------------------------------------------------------

type Configs struct {
	Committed Config `json:"committed"`
	Latest    Config `json:"latest"`
}

func (c Configs) clone() Configs {
	c.Committed = c.Committed.clone()
	c.Latest = c.Latest.clone()
	return c
}

func (c Configs) IsBootstrap() bool {
	return c.Latest.IsBootstrap()
}

func (c Configs) IsCommitted() bool {
	return c.Latest.Index == c.Committed.Index
}

func (c Configs) IsStable() bool {
	return c.IsCommitted() && c.Latest.IsStable()
}

// ---------------------------------------------------------

func (r *Raft) bootstrap(t changeConfig) {
	if !r.configs.IsBootstrap() {
		t.reply(NotLeaderError{r.leader, r.leaderAddr(), false})
		return
	}
	if err := t.newConf.validate(); err != nil {
		t.reply(fmt.Errorf("raft.bootstrap: invalid config: %v", err))
		return
	}
	self, ok := t.newConf.Nodes[r.nid]
	if !ok {
		t.reply(fmt.Errorf("raft.bootstrap: invalid config: self %d does not exist", r.nid))
		return
	}
	if !self.Voter {
		t.reply(fmt.Errorf("raft.bootstrap: invalid config: self %d must be voter", r.nid))
		return
	}
	if !t.newConf.IsStable() {
		t.reply(fmt.Errorf("raft.bootstrap: non-stable config"))
		return
	}

	t.newConf.Index, t.newConf.Term = 1, 1
	debug(r, "bootstrapping", t.newConf)
	if err := r.storage.bootstrap(t.newConf); err != nil {
		t.reply(err)
		return
	}
	r.changeConfig(t.newConf)
	t.reply(nil)
	r.setState(Candidate)
}

func (l *ldrShip) onChangeConfig(t changeConfig) {
	if !l.configs.IsCommitted() {
		t.reply(InProgressError("configChange"))
		return
	}
	if l.commitIndex < l.startIndex {
		t.reply(ErrNotCommitReady)
		return
	}
	if t.newConf.Index != l.configs.Latest.Index {
		t.reply(ErrConfigChanged)
		return
	}
	if err := t.newConf.validate(); err != nil {
		t.reply(fmt.Errorf("raft.changeConfig: %v", err))
		return
	}

	for id, n := range l.configs.Latest.Nodes {
		nn, ok := t.newConf.Nodes[id]
		if !ok {
			t.reply(fmt.Errorf("raft.changeConfig: node %d is removed", id))
			return
		}
		if n.Voter != nn.Voter {
			t.reply(fmt.Errorf("raft.changeConfig: node %d voting right changed", id))
			return
		}
	}
	for id, n := range t.newConf.Nodes {
		if _, ok := l.configs.Latest.Nodes[id]; !ok {
			if n.Voter {
				t.reply(fmt.Errorf("raft.changeConfig: new node %d must be nonvoter", id))
				return
			}
		}
	}

	var voter uint64
	for id, n := range t.newConf.Nodes {
		if n.Voter && n.Action == None {
			voter = id
		}
	}
	if voter == 0 {
		t.reply(fmt.Errorf("raft.changeConfig: at least one voter must remain in cluster"))
		return
	}
	l.doChangeConfig(t.task, t.newConf)
}

func (l *ldrShip) doChangeConfig(t *task, config Config) {
	l.storeEntry(&newEntry{
		entry: config.encode(),
		task:  t,
	})
}

func (l *ldrShip) onWaitForStableConfig(t waitForStableConfig) {
	if l.configs.IsStable() {
		t.reply(l.configs.Latest)
		return
	}
	l.waitStable = append(l.waitStable, t)
}

// ---------------------------------------------------------

func (l *ldrShip) setCommitIndex(index uint64) {
	configCommitted := l.Raft.setCommitIndex(index)
	if configCommitted {
		l.checkActions()
		if l.configs.IsStable() {
			for _, t := range l.waitStable {
				t.reply(l.configs.Latest)
			}
			l.waitStable = nil
		}
	}
}

func (r *Raft) setCommitIndex(index uint64) (configCommitted bool) {
	r.commitIndex = index
	debug(r, "commitIndex", r.commitIndex)
	if !r.configs.IsCommitted() && r.configs.Latest.Index <= r.commitIndex {
		r.commitConfig()
		configCommitted = true
		if r.state == Leader && !r.configs.Latest.isVoter(r.nid) {
			// if we are no longer voter after this config is committed,
			// then what is the point of accepting fsm entries from user ????
			debug(r, "leader -> follower notVoter")
			r.setState(Follower)
			r.setLeader(0)
		}
		if r.shutdownOnRemove {
			if _, ok := r.configs.Latest.Nodes[r.nid]; !ok {
				r.doClose(ErrNodeRemoved)
			}
		}
	}
	return
}

func (l *ldrShip) changeConfig(config Config) {
	l.voter = config.isVoter(l.nid)
	l.Raft.changeConfig(config)

	// remove flrs
	for id, f := range l.flrs {
		if _, ok := config.Nodes[id]; !ok {
			close(f.stopCh)
			delete(l.flrs, id)
		}
	}

	// add new flrs
	for id, n := range config.Nodes {
		if id != l.nid {
			if _, ok := l.flrs[id]; !ok {
				l.addFlr(n)
			}
		}
	}
	l.onActionChange()
}

func (r *Raft) changeConfig(config Config) {
	debug(r, "changeConfig", config)
	r.configs.Committed = r.configs.Latest
	r.setLatest(config)
	if r.trace.ConfigChanged != nil {
		r.trace.ConfigChanged(r.liveInfo())
	}
}

func (r *Raft) commitConfig() {
	debug(r, "commitConfig", r.configs.Latest)
	r.configs.Committed = r.configs.Latest
	if r.trace.ConfigCommitted != nil {
		r.trace.ConfigCommitted(r.liveInfo())
	}
}

func (r *Raft) revertConfig() {
	debug(r, "revertConfig", r.configs.Committed)
	r.setLatest(r.configs.Committed)
	if r.trace.ConfigReverted != nil {
		r.trace.ConfigReverted(r.liveInfo())
	}
}

func (r *Raft) setLatest(config Config) {
	r.configs.Latest = config
	r.resolver.update(config)
}
