package raft

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

type state int

const (
	follower state = iota
	candidate
	leader
)

func (s state) String() string {
	switch s {
	case follower:
		return "follower"
	case candidate:
		return "candidate"
	case leader:
		return "leader"
	}
	return "unknown"
}

type Raft struct {
	addr    string
	members []*member
	wg      sync.WaitGroup

	stable   Stable
	term     uint64
	server   *server
	state    state
	leaderID string

	votedFor         string
	heartbeatTimeout time.Duration
	electionTimer    *time.Timer
	lastContact      time.Time // last time we had contact from the leader node

	log          logEntries
	lastLogIndex uint64
	lastLogTerm  uint64
	commitIndex  uint64
	lastApplied  uint64
}

func New(addrs []string, stable Stable, log Log) *Raft {
	heartbeatTimeout := 5000 * time.Millisecond // todo

	members := make([]*member, len(addrs))
	for i, addr := range addrs {
		members[i] = &member{
			addr:             addr,
			timeout:          10 * time.Second,
			heartbeatTimeout: heartbeatTimeout,
		}
	}

	return &Raft{
		addr:             addrs[0],
		stable:           stable,
		log:              logEntries{storage: log},
		members:          members,
		state:            follower,
		heartbeatTimeout: heartbeatTimeout,
	}
}

func (r *Raft) Start() error {
	var err error

	if r.term, r.votedFor, err = r.stable.Get(); err != nil {
		return err
	}

	if err = r.log.init(); err != nil {
		return err
	}

	last, err := r.log.lastEntry()
	if err != nil {
		return err
	}
	if last != nil {
		r.lastLogIndex, r.lastLogTerm = last.index, last.term
	}

	r.server, err = startServer(r.addr)
	if err != nil {
		return err
	}

	r.wg.Add(1)
	go r.run()

	return nil
}

func (r *Raft) run() {
	for {
		switch r.state {
		case follower:
			r.runFollower()
		case candidate:
			r.runCandidate()
		case leader:
			r.runLeader()
		}
	}
}

func (r *Raft) String() string {
	return fmt.Sprintf("%s %d %9s %s|", r.addr, r.term, r.state, r.votedFor)
}

func (r *Raft) quorumSize() int {
	return len(r.members)/2 + 1
}

func (r *Raft) setTerm(term uint64) {
	if r.term == term {
		return
	}
	if err := r.stable.Set(term, ""); err != nil {
		panic(fmt.Sprintf("stable.Set failed: %v", err))
	}
	r.term, r.votedFor = term, ""
}

func (r *Raft) setVotedFor(v string) {
	if err := r.stable.Set(r.term, v); err != nil {
		panic(fmt.Sprintf("save votedFor failed: %v", err))
	}
	r.votedFor = v
}

func randomTimeout(min time.Duration) time.Duration {
	return min + time.Duration(rand.Int63())%min
}

func randomTimer(min time.Duration) *time.Timer {
	return time.NewTimer(randomTimeout(min))
}

func afterRandomTimeout(min time.Duration) <-chan time.Time {
	return time.After(min + time.Duration(rand.Int63())%min)
}

const (
	maxFailureScale = 12
	failureWait     = 10 * time.Millisecond
)

// backoff is used to compute an exponential backoff
// duration. Base time is scaled by the current round,
// up to some maximum scale factor.
func backoff(round uint64) time.Duration {
	base, limit := failureWait, uint64(maxFailureScale)
	power := min(round, limit)
	for power > 2 {
		base *= 2
		power--
	}
	return base
}

func min(a, b uint64) uint64 {
	if a <= b {
		return a
	}
	return b
}
