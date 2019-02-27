package raft

import (
	"fmt"
	"io"
)

// todo: add ConfigChangeInProgress, ConfigCommitted

type Trace struct {
	StateChanged    func(info Info)
	ElectionStarted func(info Info)
	ElectionAborted func(info Info, reason string)
	ConfigChanged   func(info Info)
	ConfigCommitted func(info Info)
	ConfigReverted  func(info Info)
}

func NewTraceWriter(w io.Writer) Trace {
	return Trace{
		StateChanged: func(info Info) {
			if info.State() == Leader {
				_, _ = fmt.Fprintln(w, "[INFO] raft: cluster leadership acquired")
			}
		},
		ElectionAborted: func(info Info, reason string) {
			_, _ = fmt.Fprintf(w, "[INFO] raft: %s, aborting election\n", reason)
		},
		ConfigChanged: func(info Info) {
			_, _ = fmt.Fprintf(w, "[INFO] raft: config changed to %s\n", info.Configs().Latest)
		},
		ConfigCommitted: func(info Info) {
			_, _ = fmt.Fprintln(w, "[INFO] raft: config committed")
		},
		ConfigReverted: func(info Info) {
			_, _ = fmt.Fprintf(w, "[INFO] raft: config reverted to %s\n", info.Configs().Latest)
		},
	}
}

func (r *Raft) liveInfo() Info {
	return liveInfo{r: r, ldr: r.ldr}
}

func (r *Raft) stateChanged() {
	if r.trace.StateChanged != nil {
		r.trace.StateChanged(r.liveInfo())
	}
}

func (r *Raft) electionAborted(reason string) {
	if r.trace.ElectionAborted != nil {
		r.trace.ElectionAborted(r.liveInfo(), reason)
	}
}
