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

		case rpc := <-r.rpcCh:
			r.replyRPC(rpc)

		case vote := <-voteCh:
			// todo: if quorum unreachable raise alert
			if vote.voterID != r.addr() {
				debug(r, "<< voteResponse", vote.voterID, vote.granted, vote.term, vote.err)
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
			ne.reply(NotLeaderError{r.leaderAddr()})

		case t := <-r.taskCh:
			r.executeTask(t)
		}
	}
}

type voteResult struct {
	*voteResponse
	voterID string // todo: should we rename it to voterAddr
	err     error
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
	req := &voteRequest{
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
				voteResponse: &voteResponse{
					term:    r.term,
					granted: true,
				},
				voterID: r.addr(),
			}
			continue
		}
		connPool := r.getConnPool(n.Addr)
		go func() {
			result := voteResult{
				voteResponse: &voteResponse{
					term:    req.term,
					granted: false,
				},
				voterID: connPool.addr,
			}
			defer func() {
				resultsCh <- result
			}()
			resp, err := r.requestVote(connPool, req)
			if err != nil {
				result.err = err
				return
			}
			result.voteResponse = resp
		}()
	}
	return resultsCh
}

func (r *Raft) requestVote(pool *connPool, req *voteRequest) (*voteResponse, error) {
	debug(r.id, ">> requestVote", pool.addr)
	conn, err := pool.getConn()
	if err != nil {
		return nil, err
	}
	resp := new(voteResponse)
	if err = conn.doRPC(rpcVote, req, resp); err != nil {
		_ = conn.close()
		return resp, err
	}
	pool.returnConn(conn)
	return resp, nil
}
