package raft

import (
	"container/list"
	"fmt"
	"sort"
	"sync"
	"time"
)

const minCheckInterval = 10 * time.Millisecond

type ldrShip struct {
	*Raft

	// if quorum of nodes are not reachable for this duration
	// leader steps down to follower
	leaseTimer *time.Timer

	// leader term starts from this index.
	// this index refers to noop entry
	startIndex uint64

	// queue in which user submitted entries are enqueued
	// committed entries are dequeued and handed over to fsm go-routine
	newEntries *list.List

	// holds running replications, key is addr
	repls map[ID]*replication
	wg    sync.WaitGroup

	// to receive updates from replicators
	fromReplsCh chan interface{}
}

func (l *ldrShip) init() {
	assert(l.leader == l.id, "%s ldr.leader: got %s, want %s", l, l.leader, l.id)

	l.leaseTimer.Stop() // we start it on detecting failures
	l.startIndex = l.lastLogIndex + 1
	l.fromReplsCh = make(chan interface{}, len(l.configs.Latest.Nodes))

	// add a blank no-op entry into log at the start of its term
	l.storeEntry(NewEntry{
		entry: &entry{
			typ: entryNop,
		},
	})

	// start replication routine for each follower
	for _, node := range l.configs.Latest.Nodes {
		l.startReplication(node)
	}
}

func (l *ldrShip) release() {
	if !l.leaseTimer.Stop() {
		select {
		case <-l.leaseTimer.C:
		default:
		}
	}

	for id, repl := range l.repls {
		close(repl.stopCh)
		delete(l.repls, id)
	}

	if l.leader == l.id {
		l.leader = ""
	}

	// respond to any pending user entries
	var err error
	if l.isClosed() {
		err = ErrServerClosed
	} else {
		err = NotLeaderError{l.leaderAddr(), true}
	}
	for l.newEntries.Len() > 0 {
		ne := l.newEntries.Remove(l.newEntries.Front()).(NewEntry)
		ne.reply(err)
	}

	// wait for replicators to finish
	l.wg.Wait()
	l.fromReplsCh = nil
}

func (l *ldrShip) storeEntry(ne NewEntry) {
	ne.entry.index, ne.entry.term = l.lastLogIndex+1, l.term

	// append entry to local log
	debug(l, "log.append", ne.typ, ne.index)
	if ne.typ != entryQuery && ne.typ != entryBarrier {
		l.storage.appendEntry(ne.entry)
	}
	l.newEntries.PushBack(ne)

	// we updated lastLogIndex, so notify replicators
	if ne.typ == entryQuery || ne.typ == entryBarrier {
		l.applyCommitted()
	} else {
		l.notifyReplicators()
	}
}

func (l *ldrShip) startReplication(node Node) {
	repl := &replication{
		rtime:         newRandTime(),
		status:        replStatus{id: node.ID},
		ldrStartIndex: l.startIndex,
		connPool:      l.getConnPool(node.ID),
		hbTimeout:     l.hbTimeout,
		storage:       l.storage,
		stopCh:        make(chan struct{}),
		toLeaderCh:    l.fromReplsCh,
		fromLeaderCh:  make(chan leaderUpdate, 1),
		trace:         &l.trace,
		str:           fmt.Sprintf("%v %s", l, string(node.ID)),
	}
	l.repls[node.ID] = repl

	// send initial empty AppendEntries RPCs (heartbeat) to each follower
	req := &appendEntriesReq{
		term:           l.term,
		leader:         l.id,
		ldrCommitIndex: l.commitIndex,
		prevLogIndex:   l.lastLogIndex,
		prevLogTerm:    l.lastLogTerm,
	}

	l.wg.Add(1)
	if node.ID == l.id {
		go func() {
			// self replication: when leaderUpdate comes
			// just notify that it is replicated
			// we are doing this, so that the it is easier
			// to handle the case of single node cluster
			// todo: is this really needed? we can optimize it
			//       by avoiding this extra goroutine
			defer l.wg.Done()
			repl.notifyLdr(matchIndex{&repl.status, req.prevLogIndex})
			for {
				select {
				case <-repl.stopCh:
					return
				case update := <-repl.fromLeaderCh:
					repl.notifyLdr(matchIndex{&repl.status, update.lastIndex})
				}
			}
		}()
	} else {
		// don't retry on failure. so that we can respond to apply/inspect
		debug(repl, ">> firstHeartbeat")
		_ = repl.doRPC(req, &appendEntriesResp{})
		go func() {
			defer l.wg.Done()
			repl.runLoop(req)
			debug(repl, "repl.end")
		}()
	}
}

func (l *ldrShip) checkReplUpdates(update interface{}) {
	matchUpdated, noContactUpdated := false, false
	for {
		switch update := update.(type) {
		case matchIndex:
			matchUpdated = true
			update.status.matchIndex = update.val
		case noContact:
			noContactUpdated = true
			update.status.noContact = update.time
			if l.trace.Unreachable != nil {
				l.trace.Unreachable(l.liveInfo(), update.status.id, update.time)
			}
		case newTerm:
			// if response contains term T > currentTerm:
			// set currentTerm = T, convert to follower
			debug(l, "leader -> follower")
			l.state = Follower
			l.setTerm(update.val)
			l.leader = ""
			return
		}

		// get any waiting update
		select {
		case <-l.shutdownCh:
			return
		case update = <-l.fromReplsCh:
			continue
		default:
		}
		break
	}
	if matchUpdated {
		l.onMajorityCommit()
	}
	if noContactUpdated {
		l.checkLeaderLease()
	}
}

func (l *ldrShip) checkLeaderLease() {
	voters, reachable := 0, 0
	now, firstFailure := time.Now(), time.Time{}
	for _, node := range l.configs.Latest.Nodes {
		if node.Voter {
			voters++
			repl := l.repls[node.ID]
			noContact := repl.status.noContact
			if noContact.IsZero() {
				reachable++
			} else if now.Sub(noContact) <= l.ldrLeaseTimeout {
				reachable++
				if firstFailure.IsZero() || noContact.Before(firstFailure) {
					firstFailure = noContact
				}
			}
		}
	}

	// todo: if quorum unreachable raise alert
	if reachable < voters/2+1 {
		debug(l, "leader -> follower quorumUnreachable")
		l.state = Follower
		l.leader = ""
		return
	}

	if !l.leaseTimer.Stop() {
		select {
		case <-l.leaseTimer.C:
		default:
		}
	}

	if !firstFailure.IsZero() {
		d := l.ldrLeaseTimeout - now.Sub(firstFailure)
		if d < minCheckInterval {
			d = minCheckInterval
		}
		l.leaseTimer.Reset(d)
	}
}

// computes N such that, a majority of matchIndex[i] ≥ N
func (l *ldrShip) majorityMatchIndex() uint64 {
	numVoters := l.configs.Latest.numVoters()
	if numVoters == 1 {
		for _, node := range l.configs.Latest.Nodes {
			if node.Voter {
				return l.repls[node.ID].status.matchIndex
			}
		}
	}

	matched := make(decrUint64Slice, numVoters)
	i := 0
	for _, node := range l.configs.Latest.Nodes {
		if node.Voter {
			matched[i] = l.repls[node.ID].status.matchIndex
			i++
		}
	}
	// sort in decrease order
	sort.Sort(matched)
	quorum := numVoters/2 + 1
	return matched[quorum-1]
}

// If majorityMatchIndex(N) > commitIndex,
// and log[N].term == currentTerm: set commitIndex = N
func (l *ldrShip) onMajorityCommit() {
	majorityMatchIndex := l.majorityMatchIndex()

	// note: if majorityMatchIndex >= ldr.startIndex, it also mean
	// majorityMatchIndex.term == currentTerm
	if majorityMatchIndex > l.commitIndex && majorityMatchIndex >= l.startIndex {
		l.setCommitIndex(majorityMatchIndex)
		l.applyCommitted()
		l.notifyReplicators() // we updated commit index
	}
}

// if commitIndex > lastApplied: increment lastApplied, apply
// log[lastApplied] to state machine
func (l *ldrShip) applyCommitted() {
	for {
		// send query/barrier entries if any to fsm
		for l.newEntries.Len() > 0 {
			elem := l.newEntries.Front()
			ne := elem.Value.(NewEntry)
			if ne.index == l.lastApplied+1 && (ne.typ == entryQuery || ne.typ == entryBarrier) {
				l.newEntries.Remove(elem)
				debug(l, "fms <- {", ne.typ, ne.index, "}")
				select {
				case <-l.shutdownCh:
					ne.reply(ErrServerClosed)
					return
				case l.fsmTaskCh <- ne:
				}
			} else {
				break
			}
		}

		if l.lastApplied+1 > l.commitIndex {
			return
		}

		// get lastApplied+1 entry
		var ne NewEntry
		if l.newEntries.Len() > 0 {
			elem := l.newEntries.Front()
			if elem.Value.(NewEntry).index == l.lastApplied+1 {
				ne = l.newEntries.Remove(elem).(NewEntry)
			}
		}
		if ne.entry == nil {
			ne.entry = &entry{}
			l.storage.getEntry(l.lastApplied+1, ne.entry)
		}

		l.applyEntry(ne)
		l.lastApplied++
		debug(l, "lastApplied", l.lastApplied)
	}
}

func (l *ldrShip) notifyReplicators() {
	update := leaderUpdate{
		lastIndex:   l.lastLogIndex,
		commitIndex: l.commitIndex,
	}
	for _, repl := range l.repls {
		select {
		case repl.fromLeaderCh <- update:
		case <-repl.fromLeaderCh:
			repl.fromLeaderCh <- update
		}
	}
}

// -------------------------------------------------------

type decrUint64Slice []uint64

func (s decrUint64Slice) Len() int           { return len(s) }
func (s decrUint64Slice) Less(i, j int) bool { return s[i] > s[j] }
func (s decrUint64Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }
