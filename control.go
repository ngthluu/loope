package main

import (
	"context"
	"sync"
)

// runRegistry tracks the cancel func of every pipeline running in this process,
// keyed by issue number, so a stop request can halt one immediately. It is the
// in-memory half of the stop mechanism; the on-disk stop marker is the half
// that crosses process boundaries and restarts.
type runRegistry struct {
	mu   sync.Mutex
	live map[int]context.CancelFunc
}

// register claims issue n for a pipeline in this process. It returns false when
// the issue is already registered, which is what stops a continue from starting
// a second Claude session in a worktree one is already running in.
func (r *runRegistry) register(n int, cancel context.CancelFunc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		r.live = map[int]context.CancelFunc{}
	}
	if _, ok := r.live[n]; ok {
		return false
	}
	r.live[n] = cancel
	return true
}

// deregister releases issue n. Always called via defer by the pipeline that
// registered it, so a panicking run still frees its slot.
func (r *runRegistry) deregister(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, n)
}

// cancel halts issue n's pipeline if it is running in this process, reporting
// whether one was found. The entry is left in place: the pipeline goroutine
// deregisters as it unwinds.
func (r *runRegistry) cancel(n int) bool {
	r.mu.Lock()
	fn, ok := r.live[n]
	r.mu.Unlock()
	if !ok {
		return false
	}
	fn()
	return true
}

// running reports whether issue n has a pipeline live in this process.
func (r *runRegistry) running(n int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.live[n]
	return ok
}

// numbers returns the issue numbers currently registered. watchStops iterates
// this, so a quiet daemon does one os.Stat per live pipeline per tick and
// nothing else.
func (r *runRegistry) numbers() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(r.live))
	for n := range r.live {
		out = append(out, n)
	}
	return out
}

// finishStopped parks issue n in the operator-held stopped state, preserving
// every artifact. fromLabel is the state label the issue currently carries, or
// "" when it carries none (a queued ticket).
func (o *Orchestrator) finishStopped(ctx context.Context, n int, fromLabel string) error {
	cctx := context.WithoutCancel(ctx)
	if fromLabel == "" {
		_ = o.gh.AddLabel(cctx, n, o.cfg.StateLabels.Stopped)
	} else {
		_ = o.gh.SwapLabels(cctx, n, fromLabel, o.cfg.StateLabels.Stopped)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Stopped)
	clearParkCause(o.issueLogDir(n))
	return nil
}
