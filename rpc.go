package raft

// If election timeout elapses without receiving AppendEntries
// RPC from current leader or granting vote to candidate:
// convert to candidate.
//
// replyRPC return true if it is appendEntries request
// or vote granted
func (r *Raft) replyRPC(rpc rpc) bool {
	var resp message
	var resetElectionTimer bool
	switch req := rpc.req.(type) {
	case *voteRequest:
		reply := r.onVoteRequest(req)
		resp, resetElectionTimer = reply, reply.granted
	case *appendEntriesRequest:
		reply := r.onAppendEntriesRequest(req)
		resp, resetElectionTimer = reply, true
		resp, resetElectionTimer = reply, true
	default:
		// todo
	}
	rpc.respCh <- resp
	return resetElectionTimer
}

func (r *Raft) onVoteRequest(req *voteRequest) *voteResponse {
	debug(r, "onVoteRequest", req.term, req.candidate, req.lastLogIndex, req.lastLogTerm)
	resp := &voteResponse{
		term:    r.term,
		granted: false,
	}

	switch {
	case req.term < r.term: // reject: older term
		debug(r, "rejectVoteTo", req.candidate, "oldTerm")
		return resp
	case req.term > r.term: // convert to follower
		debug(r, "stateChange", req.term, Follower)
		r.state = Follower
		r.setTerm(req.term)
		r.stateChanged()
	}

	// if we have leader, we only vote for him
	if r.leader != "" {
		resp.granted = req.candidate == r.leader
		return resp
	}

	if r.votedFor != "" { // we already voted in this election before
		if r.votedFor == req.candidate { // same candidate we votedFor
			resp.granted = true
			debug(r, "grantVoteTo", req.candidate)
		} else {
			debug(r, "rejectVoteTo", req.candidate, "alreadyVotedTo", r.votedFor)
		}
		return resp
	}

	// reject if candidate’s log is not at least as up-to-date as ours
	if r.log.lastTerm > req.lastLogTerm || (r.log.lastTerm == req.lastLogTerm && r.log.lastIndex > req.lastLogIndex) {
		debug(r, "rejectVoteTo", req.candidate, "logNotUptoDate", r.log.lastIndex, r.log.lastTerm, req.lastLogIndex, req.lastLogTerm)
		return resp
	}

	debug(r, "grantVoteTo", req.candidate)
	resp.granted = true
	r.setVotedFor(req.candidate)

	return resp
}

func (r *Raft) onAppendEntriesRequest(req *appendEntriesRequest) *appendEntriesResponse {
	resp := &appendEntriesResponse{
		term:         r.term,
		success:      false,
		lastLogIndex: r.log.lastIndex,
	}

	// reply false if older term
	if req.term < r.term {
		return resp
	}

	// if newer term, convert to follower
	if req.term > r.term || r.state != Follower {
		debug(r, "stateChange", req.term, Follower)
		r.state = Follower
		r.setTerm(req.term)
		r.stateChanged()
	}

	r.leader = req.leader

	// reply false if log at req.prevLogIndex does not match
	if req.prevLogIndex > 0 {
		if req.prevLogIndex > r.log.lastIndex {
			// no log found
			return resp
		}

		var prevLogTerm uint64
		if req.prevLogIndex == r.log.lastIndex {
			prevLogTerm = r.log.lastTerm
		} else {
			prevEntry := &entry{}
			r.log.getEntry(req.prevLogIndex, prevEntry)
			prevLogTerm = prevEntry.term
		}

		if req.prevLogTerm != prevLogTerm {
			// term did not match
			return resp
		}
	}

	// we came here, it means we got valid request
	if len(req.entries) > 0 {
		var newEntries []*entry

		// if new entry conflicts, delete it and all that follow it
		for i, ne := range req.entries {
			if ne.index > r.log.lastIndex {
				newEntries = req.entries[i:]
				break
			}
			e := &entry{}
			r.log.getEntry(ne.index, e)
			if e.term != ne.term { // conflicts
				debug(r, "log.deleteGTE", ne.index)
				r.log.deleteGTE(ne.index)
				if ne.index <= r.configs.Latest.Index {
					r.revertConfig()
				}
				newEntries = req.entries[i:]
				break
			}
		}

		// append new entries not already in the log
		if len(newEntries) > 0 {
			debug(r, "log.appendN", "from:", newEntries[0].index, "n:", len(newEntries))
			for _, e := range newEntries {
				r.log.append(e)
				if e.typ == entryConfig {
					var newConfig Config
					if err := newConfig.decode(e); err != nil {
						panic(err)
					}
					r.changeConfig(newConfig)
				}
			}
		}

		resp.lastLogIndex = r.log.lastIndex
	}

	// If leaderCommit > commitIndex, set commitIndex =
	// min(leaderCommit, index of last new entry)
	// note: req.ldrCommitIndex==0 for heartbeat requests
	lastIndex, lastTerm := r.lastLog(req)
	if lastTerm == req.term && req.ldrCommitIndex > r.commitIndex {
		r.commitIndex = min(req.ldrCommitIndex, lastIndex)
		r.applyCommitted(nil) // apply newly committed logs
	}

	resp.success = true
	return resp
}

func (r *Raft) lastLog(req *appendEntriesRequest) (index uint64, term uint64) {
	switch n := len(req.entries); {
	case n == 0:
		return req.prevLogIndex, req.prevLogTerm
	default:
		last := req.entries[n-1]
		return last.index, last.term
	}
}
