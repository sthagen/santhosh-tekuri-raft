package raft

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

type member struct {
	id               int
	addr             string
	timeout          time.Duration
	heartbeatTimeout time.Duration

	connPoolMu sync.Mutex
	connPool   []*netConn
	maxConns   int

	nextIndex  uint64
	matchIndex uint64

	// owned exclusively by raft main goroutine
	// used to recalculateMatch
	matchedIndex uint64

	// leader notifies replicator when its lastLogIndex changes
	leaderLastIndexCh chan uint64

	// leader notifies replicator when its commitIndex changes
	leaderCommitIndexCh chan uint64
}

func (m *member) getConn() (*netConn, error) {
	m.connPoolMu.Lock()
	defer m.connPoolMu.Unlock()

	num := len(m.connPool)
	if num == 0 {
		return dial(m.addr, m.timeout)
	}
	var conn *netConn
	conn, m.connPool[num-1] = m.connPool[num-1], nil
	m.connPool = m.connPool[:num-1]
	return conn, nil
}

func (m *member) returnConn(conn *netConn) {
	m.connPoolMu.Lock()
	defer m.connPoolMu.Unlock()

	if len(m.connPool) < m.maxConns {
		m.connPool = append(m.connPool, conn)
	} else {
		conn.close()
	}
}

func (m *member) doRPC(typ rpcType, req, resp command) error {
	conn, err := m.getConn()
	if err != nil {
		return err
	}
	if err = conn.doRPC(typ, req, resp); err != nil {
		conn.close()
		return err
	}
	m.returnConn(conn)
	return nil
}

func (m *member) requestVote(req *requestVoteRequest) (*requestVoteResponse, error) {
	resp := new(requestVoteResponse)
	err := m.doRPC(rpcRequestVote, req, resp)
	return resp, err
}

func (m *member) appendEntries(req *appendEntriesRequest) (*appendEntriesResponse, error) {
	resp := new(appendEntriesResponse)
	err := m.doRPC(rpcAppendEntries, req, resp)
	return resp, err
}

func (m *member) retryAppendEntries(req *appendEntriesRequest, stopCh <-chan struct{}) (*appendEntriesResponse, error) {
	var failures uint64
	for {
		resp, err := m.appendEntries(req)
		if err != nil {
			failures++
			select {
			case <-time.After(backoff(failures)):
				continue
			case <-stopCh:
				return resp, err
			}
		}
		return resp, nil
	}
}

const maxAppendEntries = 64

func (m *member) replicate(storage *storage, heartbeat *appendEntriesRequest, matchUpdatedCh chan<- *member, stopCh <-chan struct{}) {
	// send initial empty AppendEntries RPCs (heartbeat) to each follower
	debug("heartbeat ->")
	m.retryAppendEntries(heartbeat, stopCh)

	req := &appendEntriesRequest{}
	*req = *heartbeat

	// non-blocking
	lastIndex := <-m.leaderLastIndexCh
	commitIndex := <-m.leaderCommitIndexCh

	// know which entries to replicate: fixes m.nextIndex and m.matchIndex
	// after loop: m.nextIndex == m.matchIndex + 1
	//
	// NOTE:
	//   we are not sending req.leaderCommitIndex, because
	//   we do not want follower to move its commit index, while we are
	//   trying to figure out which entries to replicate
	for lastIndex >= m.nextIndex {
		storage.fillEntries(req, m.nextIndex, m.nextIndex-1) // zero entries
		resp, err := m.retryAppendEntries(req, stopCh)
		if err != nil {
			return
		}
		if resp.success {
			m.setMatchIndex(req.prevLogIndex, matchUpdatedCh)
			break
		} else {
			m.nextIndex--
			continue
		}
	}

	for {
		req.leaderCommitIndex = commitIndex

		// if follower log is upto date, ask him to update its commitIndex
		if matchIndex := m.getMatchIndex(); matchIndex >= lastIndex {
			storage.fillEntries(req, matchIndex+1, matchIndex) // zero entries
			debug(heartbeat.leaderID, "asking", m.addr, "to set commitIndex =", commitIndex)
			if _, err := m.retryAppendEntries(req, stopCh); err != nil {
				return
			}
		}

		// replicate entries [m.nextIndex, lastIndex] to follower
		for m.getMatchIndex() < lastIndex {
			maxIndex := min(lastIndex, m.nextIndex+uint64(maxAppendEntries)-1)
			storage.fillEntries(req, m.nextIndex, maxIndex)
			debug(heartbeat.leaderID, "sending", len(req.entries), "entries to", m.addr)
			resp, err := m.retryAppendEntries(req, stopCh)
			if err != nil {
				return
			}
			if resp.success {
				m.nextIndex = maxIndex + 1
				m.setMatchIndex(maxIndex, matchUpdatedCh)
			} else {
				log.Println("[WARN] should not happend") // todo
			}
		}

		// send heartbeat during idle periods to
		// prevent election timeouts
	loop:
		for {
			select {
			case <-stopCh:
				return
			case lastIndex = <-m.leaderLastIndexCh:
				break loop // to replicate new entry
			case commitIndex = <-m.leaderCommitIndexCh:
				debug(m.addr, "got commitIndex update", commitIndex)
				break loop // to replicate new entry
			case <-afterRandomTimeout(m.heartbeatTimeout / 10):
				debug("heartbeat ->")
				m.retryAppendEntries(heartbeat, stopCh)
			}
		}
	}
}

func (m *member) getMatchIndex() uint64 {
	return atomic.LoadUint64(&m.matchIndex)
}

func (m *member) setMatchIndex(v uint64, updatedCh chan<- *member) {
	atomic.StoreUint64(&m.matchIndex, v)
	updatedCh <- m
}
