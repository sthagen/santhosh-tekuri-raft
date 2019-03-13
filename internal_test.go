package raft

import "time"

// export access to raft internals for tests

func Debug(args ...interface{}) {
	debug(args...)
}

func RequestVote(from, to *Raft) (granted bool, err error) {
	fn := func(r *Raft) {
		req := &voteReq{
			term:         r.term,
			lastLogIndex: r.lastLogIndex,
			lastLogTerm:  r.lastLogTerm,
			candidate:    r.id,
		}
		pool := from.getConnPool(to.id)
		cand := candShip{Raft: from}
		resp, errr := cand.requestVote(pool, req, time.Time{})
		granted, err = resp.result == success, errr
	}
	if from.isClosed() {
		fn(from)
	} else {
		ierr := from.inspect(fn)
		if err == nil {
			err = ierr
		}
	}
	return
}

func BootstrapStorage(storage Storage, nodes map[uint64]Node) error {
	store := newStorage(storage)
	if err := store.init(); err != nil {
		return err
	}
	return store.bootstrap(Config{Nodes: nodes, Index: 1, Term: 1})
}
