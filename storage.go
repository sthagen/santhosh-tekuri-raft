package raft

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/santhosh-tekuri/raft/log"
)

type StorageOptions struct {
	DirMode        os.FileMode
	FileMode       os.FileMode
	LogSegmentSize int
}

func DefaultStorageOptions() StorageOptions {
	return StorageOptions{
		DirMode:        0700,
		FileMode:       0600,
		LogSegmentSize: 16 * 1024 * 1024,
	}
}

// Storage contains all persistent state.
type storage struct {
	idVal *value
	cid   uint64
	nid   uint64

	termVal  *value
	term     uint64
	votedFor uint64

	log          *log.Log
	lastLogIndex uint64
	lastLogTerm  uint64

	snaps   *snapshots
	configs Configs
}

func OpenStorage(dir string, opt StorageOptions) (*storage, error) {
	if err := os.MkdirAll(dir, opt.DirMode); err != nil {
		return nil, err
	}
	s, err := &storage{}, error(nil)
	defer func() {
		if err != nil {
			if s.log != nil {
				_ = s.log.Close()
			}
		}
	}()

	// open identity value ----------------
	if s.idVal, err = openValue(dir, ".id", opt.FileMode); err != nil {
		return nil, err
	}
	s.cid, s.nid = s.idVal.get()

	// open term value ----------------
	if s.termVal, err = openValue(dir, ".term", opt.FileMode); err != nil {
		return nil, err
	}
	s.term, s.votedFor = s.termVal.get()

	// open snapshots ----------------
	if s.snaps, err = openSnapshots(filepath.Join(dir, "snapshots")); err != nil {
		return nil, err
	}
	s.lastLogIndex, s.lastLogTerm = s.snaps.index, s.snaps.term
	meta, err := s.snaps.meta()
	if err != nil {
		return nil, err
	}

	// open log ----------------
	logOpt := log.Options{
		FileMode:    opt.FileMode,
		SegmentSize: opt.LogSegmentSize,
	}
	if s.log, err = log.Open(filepath.Join(dir, "log"), opt.DirMode, logOpt); err != nil {
		return nil, err
	}
	if s.log.Count() > 0 {
		data, err := s.log.Get(s.log.LastIndex())
		if err != nil {
			return nil, opError(err, "Log.Get(%d)", s.log.LastIndex())
		}
		e := &entry{}
		if err := e.decode(bytes.NewReader(data)); err != nil {
			return nil, opError(err, "Log.Get(%d).decode", s.log.LastIndex())
		}
		if e.index != s.log.LastIndex() {
			panic("BUG")
		}
		s.lastLogIndex, s.lastLogTerm = e.index, e.term
	}

	// load configs ----------------
	need := 2
	for i := s.lastLogIndex; i > s.snaps.index; i-- {
		e := &entry{}
		if err = s.getEntry(i, e); err != nil {
			return nil, err
		}
		if e.typ == entryConfig {
			if need == 2 {
				err = s.configs.Latest.decode(e)
			} else {
				err = s.configs.Committed.decode(e)
			}
			if err != nil {
				return nil, err
			}
			need--
			if need == 0 {
				break
			}
		}
	}
	if need == 2 {
		s.configs.Latest = meta.Config
		need--
	}
	if need == 1 {
		s.configs.Committed = meta.Config
	}

	return s, nil
}

// GetIdentity returns the server identity.
//
// The identity includes clusterID and nodeID. Zero values
// mean identity is not set yet.
func (s *storage) GetIdentity() (cid, nid uint64) {
	return s.cid, s.nid
}

// SetIdentity sets the server identity.
//
// If identity is already set and you are trying
// to override it with different identity, it returns error.
func (s *storage) SetIdentity(cid, nid uint64) error {
	if cid == 0 {
		return errors.New("raft: cid is zero")
	}
	if nid == 0 {
		return errors.New("raft: nid is zero")
	}
	if cid == s.cid && nid == s.nid {
		return nil
	}
	if s.cid != 0 || s.nid != 0 {
		return ErrIdentityAlreadySet
	}
	if err := s.idVal.set(cid, nid); err != nil {
		return err
	}
	s.cid, s.nid = s.idVal.get()
	return nil
}

func (s *storage) setTerm(term uint64) {
	if s.term != term {
		if term < s.term {
			panic(fmt.Sprintf("term cannot be changed from %d to %d", s.term, term))
		}
		if err := s.termVal.set(s.term, 0); err != nil {
			panic(opError(err, "Vars.SetVote(%d, %d)", term, 0))
		}
		s.term, s.votedFor = term, 0
	}
}

var grantingVote = func(s *storage, term, candidate uint64) error { return nil }

func (s *storage) setVotedFor(term, candidate uint64) {
	if term < s.term {
		panic(fmt.Sprintf("term cannot be changed from %d to %d", s.term, term))
	}
	err := grantingVote(s, term, candidate)
	if err == nil {
		err = s.termVal.set(s.term, candidate)
	}
	if err != nil {
		panic(opError(err, "Vars.SetVote(%d, %d)", term, candidate))
	}
	s.term, s.votedFor = term, candidate
}

// NOTE: this should not be called with snapIndex
func (s *storage) getEntryTerm(index uint64) (uint64, error) {
	e := &entry{}
	err := s.getEntry(index, e)
	return e.term, err
}

// called by raft.runLoop and m.replicate. append call can be called during this
// never called with invalid index
func (s *storage) getEntry(index uint64, e *entry) error {
	b, err := s.log.Get(index)
	if err == errNoEntryFound {
		return err
	} else if err != nil {
		panic(opError(err, "Log.Get(%d)", index))
	}
	if err = e.decode(bytes.NewReader(b)); err != nil {
		panic(opError(err, "log.Get(%d).decode()", index))
	}
	if e.index != index {
		panic(opError(fmt.Errorf("got %d, want %d", e.index, index), "log.Get(%d).index: ", index))
	}
	return nil
}

func (s *storage) mustGetEntry(index uint64, e *entry) {
	if err := s.getEntry(index, e); err != nil {
		panic(bug(2, "storage.MustGetEntry(%d): %v", index, err))
	}
}

// called by raft.runLoop. getEntry call can be called during this
func (s *storage) appendEntry(e *entry) {
	if s.lastLogIndex != s.log.LastIndex() {
		panic("BUG")
	}
	if e.index != s.lastLogIndex+1 {
		panic(bug(2, "storage.appendEntry.index: got %d, want %d", e.index, s.lastLogIndex+1))
	}
	w := new(bytes.Buffer)
	if err := e.encode(w); err != nil {
		panic(bug(2, "entry.encode(%d): %v", e.index, err))
	}
	if err := s.log.Append(w.Bytes()); err != nil {
		panic(opError(err, "Log.Append"))
	}
	s.lastLogIndex, s.lastLogTerm = e.index, e.term
	if s.lastLogIndex != s.log.LastIndex() {
		panic("BUG")
	}
}

func (s *storage) syncLog() {
	if err := s.log.Sync(); err != nil {
		panic(opError(err, "Log.Sync"))
	}
}

// never called with invalid index
func (s *storage) removeLTE(index uint64) error {
	debug("removeLTE index:", index, "prevLogIndex:", s.log.PrevIndex(), "lastLogIndex:", s.lastLogIndex)
	// todo: trace log compaction
	if err := s.log.RemoveLTE(index); err != nil {
		return opError(err, "Log.RemoveLTE(%d)", index)
	}
	return nil
}

func (r *Raft) compactLog(lte uint64) {
	if err := r.storage.removeLTE(lte); err != nil {
		if r.trace.Error != nil {
			r.trace.Error(err)
		}
	} else if r.trace.LogCompacted != nil {
		r.trace.LogCompacted(r.liveInfo())
	}
}

// no flr.replicate is going on when this called
// todo: are you sure about this ???
func (s *storage) clearLog() error {
	if err := s.log.Reset(s.snaps.index); err != nil {
		return opError(err, "Log.Reset(%d)", s.snaps.index)
	}
	if s.log.LastIndex() != s.snaps.index {
		panic("BUG")
	}
	if s.log.PrevIndex() != s.snaps.index {
		panic("BUG")
	}
	s.lastLogIndex, s.lastLogTerm = s.snaps.index, s.snaps.term
	return nil
}

// called by raft.runLoop. no other calls made during this
// never called with invalid index
func (s *storage) removeGTE(index, prevTerm uint64) {
	if err := s.log.RemoveGTE(index); err != nil {
		panic(opError(err, "Log.RemoveGTE(%d)", index))
	}
	if s.log.LastIndex() != index-1 {
		panic("BUG")
	}
	s.lastLogIndex, s.lastLogTerm = index-1, prevTerm
}

func (s *storage) bootstrap(config Config) (err error) {
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(runtime.Error); ok {
				panic(v)
			}
			err = toErr(v)
		}
	}()
	s.appendEntry(config.encode())
	s.syncLog()
	s.setTerm(1)
	s.lastLogIndex, s.lastLogTerm = config.Index, config.Term
	s.configs.Committed, s.configs.Latest = config, config
	return nil
}
