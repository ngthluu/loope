package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
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

// currentStateLabel returns the state label issue n currently carries on
// GitHub, or "" when it carries none (a queued ticket).
func (o *Orchestrator) currentStateLabel(ctx context.Context, n int) (string, error) {
	labels, err := o.gh.IssueLabels(ctx, n)
	if err != nil {
		return "", err
	}
	sl := o.cfg.StateLabels
	for _, want := range []string{sl.WIP, sl.Stopped, sl.Rework, sl.Done, sl.NeedsInfo, sl.Failed} {
		if want != "" && hasLabel(labels, want) {
			return want, nil
		}
	}
	return "", nil
}

// Stop halts work on issue n and parks it in the operator-held stopped state,
// preserving every artifact. The stop marker is written FIRST, so the request is
// durable before anything else can fail — that is what lets `loope -stop <N>` in
// a second shell halt a run a daemon in another process owns, and what makes the
// stop survive a daemon restart.
//
// Then, by what is actually running: a pipeline live in THIS process is
// cancelled and does its own labeling as it unwinds; a WIP issue owned by a live
// daemon elsewhere is left to that daemon's watcher (~2s); anything else
// (queued, parked, or WIP with no daemon alive) is labeled here and now.
//
// Stopping a stopped issue is a no-op success. Stopping a done or needs-info
// issue is an error: there is nothing to stop.
func (o *Orchestrator) Stop(ctx context.Context, n int) error {
	state, err := o.currentStateLabel(ctx, n)
	if err != nil {
		return err
	}
	switch state {
	case o.cfg.StateLabels.Stopped:
		log.Printf("stopped #%d", n)
		return nil
	case o.cfg.StateLabels.Done, o.cfg.StateLabels.NeedsInfo:
		return fmt.Errorf("#%d is %s — there is nothing to stop", n, state)
	}

	recordStopRequest(o.issueLogDir(n))

	if o.registry.cancel(n) {
		log.Printf("stopping #%d (halting the running session)", n)
		return nil
	}
	if state == o.cfg.StateLabels.WIP && lockOwnerAlive(o.cfg.WorkDir) {
		log.Printf("stop requested for #%d — the running daemon will halt it shortly", n)
		return nil
	}
	if err := o.finishStopped(ctx, n, state); err != nil {
		return err
	}
	log.Printf("stopped #%d", n)
	return nil
}

// finishStopped moves issue n into the stopped state, preserving the worktree,
// branch, logs, and session file — continue builds on all of it. fromLabel is
// the state label the issue carries, or "" for a queued ticket that has none.
//
// It uses a cancellation-proof context because the pipeline path calls it with
// an already-cancelled one, clears the park cause so ResumeParked can never see
// the issue as resumable, and deliberately LEAVES the stop marker: the marker is
// cleared by continue, not by the stop completing.
func (o *Orchestrator) finishStopped(ctx context.Context, n int, fromLabel string) error {
	cctx := context.WithoutCancel(ctx)
	_ = o.gh.Comment(cctx, n, fmt.Sprintf(
		"🤖 Stopped by request. Progress is preserved — continue with `loope -continue %d` or the dashboard.", n))
	if fromLabel == "" {
		if err := o.gh.AddLabel(cctx, n, o.cfg.StateLabels.Stopped); err != nil {
			return fmt.Errorf("issue #%d: marking stopped failed: %w", n, err)
		}
	} else if err := o.gh.SwapLabels(cctx, n, fromLabel, o.cfg.StateLabels.Stopped); err != nil {
		return fmt.Errorf("issue #%d: marking stopped failed: %w", n, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Stopped)
	clearParkCause(o.issueLogDir(n))
	return nil
}

// watchStops cancels any locally running pipeline whose stop marker has
// appeared. It is what lets `loope -stop <N>` in another shell halt a run this
// daemon owns: that process can only write the marker file, not reach into this
// process's goroutines.
//
// It iterates only over registered issue numbers, so a quiet daemon does one
// os.Stat per live pipeline per tick and nothing else. Returns when ctx is done.
func (o *Orchestrator) watchStops(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, n := range o.registry.numbers() {
				if stopRequested(o.issueLogDir(n)) {
					if o.registry.cancel(n) {
						log.Printf("issue #%d: stop requested — halting the running session", n)
					}
				}
			}
		}
	}
}

// prepareContinue validates a continue and performs everything that must happen
// synchronously — the caller can therefore report a real error — then returns
// the resume closure to run, or nil when there is nothing to resume.
//
// Case 1, a preserved worktree and a saved session: swap stopped -> WIP (the
// ticket is genuinely working again, so the dashboard shows it live and
// SweepOrphans can recover it if the daemon dies mid-continue) and return the
// resume. Case 2, neither survived — the ticket was stopped while queued — so
// continue means re-queue: drop the stopped label and the local state, and the
// next poll cycle picks the issue up from scratch through triage.
func (o *Orchestrator) prepareContinue(ctx context.Context, n int) (func(context.Context) error, error) {
	state, err := o.currentStateLabel(ctx, n)
	if err != nil {
		return nil, err
	}
	if state != o.cfg.StateLabels.Stopped {
		return nil, fmt.Errorf("#%d is not stopped", n)
	}
	if o.registry.running(n) {
		return nil, fmt.Errorf("#%d is already running", n)
	}
	logDir := o.issueLogDir(n)

	resumable := false
	if _, err := os.Stat(worktreePath(o.cfg.WorkDir, n)); err == nil {
		if si, serr := readSession(logDir); serr == nil && si.SessionID != "" {
			resumable = true
		}
	}

	clearStopRequest(logDir)
	if !resumable {
		if err := o.gh.RemoveLabel(ctx, n, o.cfg.StateLabels.Stopped); err != nil {
			return nil, fmt.Errorf("issue #%d: re-queueing failed: %w", n, err)
		}
		clearState(logDir)
		log.Printf("issue #%d: nothing to resume — re-queued for a fresh run", n)
		return nil, nil
	}
	if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.Stopped, o.cfg.StateLabels.WIP); err != nil {
		return nil, fmt.Errorf("issue #%d: marking wip failed: %w", n, err)
	}
	recordState(logDir, o.cfg.StateLabels.WIP)
	return func(rctx context.Context) error {
		return o.resume(rctx, n, o.cfg.StateLabels.WIP)
	}, nil
}

// Continue takes stopped issue n out of the operator hold and drives it to a PR,
// synchronously: it resumes the persisted Claude session in the preserved
// worktree and then ships (WIP -> Done) or parks (WIP -> Rework) exactly as a
// rework does. A ticket stopped before any work started is simply re-queued.
func (o *Orchestrator) Continue(ctx context.Context, n int) error {
	run, err := o.prepareContinue(ctx, n)
	if err != nil || run == nil {
		return err
	}
	return run(ctx)
}

// orchestratorController adapts Orchestrator to the dashboard's Controller.
// The dashboard's requests are short-lived, so the adapter runs work on the
// daemon's context instead: a continue must survive its HTTP response and die
// with the daemon, not with the request.
type orchestratorController struct{ o *Orchestrator }

// controller returns the dashboard-facing mutating surface for this daemon.
func (o *Orchestrator) controller() Controller { return orchestratorController{o: o} }

// Stop is fast (it writes a marker and cancels or labels), so it runs inline and
// the UI gets a real error.
func (c orchestratorController) Stop(n int) error {
	return c.o.Stop(c.o.base(), n)
}

// Continue validates and performs the label transition synchronously — so the
// UI can report "#N is not stopped" or "#N is already running" — then runs the
// multi-minute resume in the background on the daemon's context.
func (c orchestratorController) Continue(n int) error {
	run, err := c.o.prepareContinue(c.o.base(), n)
	if err != nil || run == nil {
		return err
	}
	go func() {
		if err := guard("continue", func() error { return run(c.o.base()) }); err != nil {
			log.Printf("continue #%d: %v", n, err)
		}
	}()
	return nil
}
