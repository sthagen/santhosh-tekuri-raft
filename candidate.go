package raft

import "time"

func (r *Raft) runCandidate() {
	assert(r.leader == "", "%s r.leader: got %s, want ", r, r.leader)
	var (
		timeoutCh   <-chan time.Time
		voteCh      <-chan voteResult
		votesNeeded int
	)

	startElection := true
	for r.state == Candidate {
		if startElection {
			startElection = false
			timeoutCh = afterRandomTimeout(r.hbTimeout)
			voteCh = r.startElection()
			votesNeeded = r.configs.Latest.quorum()
		}
		select {
		case <-r.shutdownCh:
			return

		case rpc := <-r.server.rpcCh:
			r.replyRPC(rpc)

		case vote := <-voteCh:
			// todo: if quorum unreachable raise alert
			if vote.from != r.id {
				debug(r, "<< voteResp", vote.from, vote.granted, vote.term, vote.err)
			}

			if vote.err != nil {
				continue
			}
			// if response contains term T > currentTerm:
			// set currentTerm = T, convert to follower
			if vote.term > r.term {
				debug(r, "candidate -> follower")
				r.state = Follower
				r.setTerm(vote.term)
				r.stateChanged()
				return
			}

			// if votes received from majority of servers: become leader
			if vote.granted {
				votesNeeded--
				if votesNeeded == 0 {
					debug(r, "candidate -> leader")
					r.state = Leader
					r.leader = r.id
					r.stateChanged()
					return
				}
			}
		case <-timeoutCh:
			startElection = true

		case ne := <-r.newEntryCh:
			ne.reply(NotLeaderError{r.leaderAddr(), false})

		case t := <-r.taskCh:
			r.executeTask(t)
		}
	}
}

type voteResult struct {
	*voteResp
	from ID
	err  error
}

func (r *Raft) startElection() <-chan voteResult {
	resultsCh := make(chan voteResult, len(r.configs.Latest.Nodes))

	// increment currentTerm
	r.setTerm(r.term + 1)

	debug(r, "startElection")
	if r.trace.ElectionStarted != nil {
		r.trace.ElectionStarted(r.liveInfo())
	}

	// send RequestVote RPCs to all other servers
	req := &voteReq{
		term:         r.term,
		candidate:    r.id,
		lastLogIndex: r.lastLogIndex,
		lastLogTerm:  r.lastLogTerm,
	}
	for _, n := range r.configs.Latest.Nodes {
		if !n.Voter {
			continue
		}
		if n.ID == r.id {
			// vote for self
			r.setVotedFor(r.id)
			resultsCh <- voteResult{
				voteResp: &voteResp{
					term:    r.term,
					granted: true,
				},
				from: r.id,
			}
			continue
		}
		connPool := r.getConnPool(n.ID)
		go func() {
			result := voteResult{
				voteResp: &voteResp{
					term:    req.term,
					granted: false,
				},
				from: connPool.id,
			}
			defer func() {
				resultsCh <- result
			}()
			resp, err := r.requestVote(connPool, req)
			if err != nil {
				result.err = err
				return
			}
			result.voteResp = resp
		}()
	}
	return resultsCh
}

func (r *Raft) requestVote(pool *connPool, req *voteReq) (*voteResp, error) {
	debug(r.id, ">> requestVote", pool.id)
	conn, err := pool.getConn()
	if err != nil {
		return nil, err
	}
	resp := new(voteResp)
	if r.trace.sending != nil {
		r.trace.sending(r.id, pool.id, req)
	}
	if err = conn.doRPC(req, resp); err != nil {
		_ = conn.close()
		return nil, err
	}
	pool.returnConn(conn)
	if r.trace.received != nil {
		r.trace.received(r.id, pool.id, resp)
	}
	return resp, nil
}
