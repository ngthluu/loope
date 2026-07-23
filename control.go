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
// in-memory half of the stop mechanism; the on-disk stop marker and run-owner
// file are the halves that cross process boundaries and restarts.
type runRegistry struct {
	mu   sync.Mutex
	live map[int]liveRun

	// claim and stopPending are the two filesystem halves of a register and a
	// release. Seams for tests, which need to hold one open to observe what the
	// rest of the registry can do while it is in flight — the whole point of
	// doing them outside the mutex. nil means claimRunOwner / stopRequested.
	claim       func(logDir string) (*pidLock, bool)
	stopPending func(logDir string) bool
}

// liveRun is one registered pipeline: the cancel func a stop pulls, and the
// on-disk claim the run holds for as long as it is running. The claim is a held
// kernel lock rather than a written file, so it must live exactly as long as the
// entry — released, not merely deleted, when the run ends.
type liveRun struct {
	cancel context.CancelFunc
	lock   *pidLock
}

// register claims issue n for a pipeline in this process, reporting whether the
// claim was won. Both halves are taken here: the map, which answers "is this
// running in THIS process", and logDir's run-owner claim, which is the only
// thing that can answer "in ANY process".
//
// Both are needed, because a second Claude session in a worktree one is already
// running in is the same corruption whether the first session belongs to this
// daemon or to a `loope -rework` in another shell. The map alone cannot see the
// second case, and the workDir lock cannot either: it proves no other DAEMON is
// up, while -once, -rework and -continue drive pipelines holding no lock.
//
// Taking the on-disk claim as part of the register, rather than after it, is
// what makes the handshake with Stop work — see Stop.
//
// The two halves are taken in sequence rather than under one lock, because the
// on-disk half touches the filesystem: workDir can be a network mount, and a
// register blocked in the kernel used to block every other registry caller
// behind it — Stop and the stop watcher could not even RECORD a cancellation
// while a claim hung, and the sweep and dashboard stalled with them. The map
// entry is therefore reserved first, with the cancel func already in it, so a
// stop landing mid-claim is honoured rather than queued; a lost claim then
// removes the reservation again.
func (r *runRegistry) register(n int, logDir string, cancel context.CancelFunc) bool {
	r.mu.Lock()
	if r.live == nil {
		r.live = map[int]liveRun{}
	}
	if _, ok := r.live[n]; ok {
		r.mu.Unlock()
		return false
	}
	r.live[n] = liveRun{cancel: cancel}
	r.mu.Unlock()

	claim := r.claim
	if claim == nil {
		claim = claimRunOwner
	}
	lock, won := claim(logDir)

	r.mu.Lock()
	defer r.mu.Unlock()
	if !won {
		delete(r.live, n)
		return false
	}
	r.live[n] = liveRun{cancel: cancel, lock: lock}
	return true
}

// release drops this process's claim on issue n and reports whether a stop
// marker was pending as it did so — a stop that landed too late for the run to
// act on, which the caller must now finish.
//
// Retracting the claim BEFORE reading the marker is what makes a stop from
// another process impossible to lose. Stop's order is the mirror: write the
// marker, then look for an owner. Suppose it finds one — that probe ran before
// this retraction, so its write ran before it too, and the read below sees it.
// Suppose it does not — the retraction already happened, and Stop finishes the
// stop itself. Both may fire, and finishStopped is idempotent for exactly that
// reason; neither can miss. Read first and there is a schedule where both do:
// read(nothing) < write < probe(owner still here) < retract, which left the
// marker for the issue's next life to read as a fresh hold.
//
// The in-process half of the same handshake is the map, which Stop's cancel
// takes the mutex for: a stop that finds the entry cancels a live run, and one
// that does not finds no owner either, because the entry outlives the claim.
//
// The filesystem work is deliberately outside the mutex, for the reason
// register's is: workDir can be a network mount, and one issue's release must
// not be able to block Stop for every other issue.
//
// Only a claim we actually hold is retracted: a refused register must never
// release the claim of the process that beat us to it.
func (r *runRegistry) release(n int, logDir string) (stopPending bool) {
	r.mu.Lock()
	run, ok := r.live[n]
	if ok {
		delete(r.live, n)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	run.lock.release()
	if r.stopPending != nil {
		return r.stopPending(logDir)
	}
	return stopRequested(logDir)
}

// cancel halts issue n's pipeline if it is running in this process, reporting
// whether one was found. The entry is left in place: the pipeline goroutine
// releases its claim as it unwinds.
func (r *runRegistry) cancel(n int) bool {
	r.mu.Lock()
	run, ok := r.live[n]
	r.mu.Unlock()
	if !ok {
		return false
	}
	run.cancel()
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
//
// It shares firstLabel with the dashboard's pickStateLabel but deliberately
// keeps its own search set: this is the label a transition will move the issue
// OUT of, so it spans every terminal state (needs-info, failed) and never falls
// back to the eligible label, which is not a state and cannot be swapped from.
func (o *Orchestrator) currentStateLabel(ctx context.Context, n int) (string, error) {
	labels, err := o.gh.IssueLabels(ctx, n)
	if err != nil {
		return "", err
	}
	sl := o.cfg.StateLabels
	return firstLabel(labels, sl.WIP, sl.Stopped, sl.Rework, sl.Done, sl.NeedsInfo, sl.Failed), nil
}

// Stop halts work on issue n and parks it in the operator-held stopped state,
// preserving every artifact. The stop marker is written FIRST, so the request is
// durable before anything else can fail — that is what lets `loope -stop <N>` in
// a second shell halt a run a daemon in another process owns, and what makes the
// stop survive a daemon restart.
//
// Then, by what is actually running: a pipeline live in THIS process is
// cancelled and does its own labeling as it unwinds; one live in ANOTHER process
// is left to that process's stop watcher (~2s); anything else — queued, parked,
// or a label left behind by a run that is no longer alive — is labeled here and
// now, because nobody else will.
//
// "Running elsewhere" is read from the issue's own run-owner file, not from the
// workDir lock. The lock only says a daemon is up; an ai-wip label with no live
// pipeline behind it (a crashed run, a `-rework` that died) would otherwise be
// handed to a daemon that has no such run to halt, leaving the issue stuck in
// wip with a marker nothing ever consumes.
//
// The write-then-check ordering here pairs with the claim's own (write the
// run-owner file, then check the marker — see handleIssue): the two orders
// cannot both miss, so a stop racing a pickup is always seen by at least one
// side, and both outcomes are the same stopped issue.
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
	if otherProcessRunning(o.issueLogDir(n)) {
		log.Printf("stop requested for #%d — the process running it will halt it shortly", n)
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
// an already-cancelled one, and clears the park cause so ResumeParked can never
// see the issue as resumable.
//
// It is idempotent, because two processes legitimately race to finish the same
// stop: the one that ran `-stop` (which labels when it has no reason to believe
// another process will) and the one that owns the run (which labels as it
// unwinds through the cancelled context). Whichever arrives second finds the
// issue already stopped and must do nothing — a second "Stopped by request"
// comment is noise, and swapping a label that has already moved is a spurious
// error on a stop that in fact succeeded.
func (o *Orchestrator) finishStopped(ctx context.Context, n int, fromLabel string) error {
	cctx := context.WithoutCancel(ctx)
	// The issue's own labels outrank what the caller believes about them: the
	// caller's fromLabel was read before whatever it did next, and handleIssue
	// passes "" whenever its read failed outright. Adding ai-stopped on top of
	// the ai-wip that is really there would leave the ticket in two states at
	// once, which continue, stop and the dashboard all read differently and no
	// transition can undo.
	//
	// A read failure falls back to the caller's fromLabel — unless that is ""
	// too, which is where guessing does the damage: "" means "a queued ticket
	// carrying no state label" to the code below, and adding ai-stopped to an
	// issue that in fact carries ai-wip is the wedge above. Nobody knows the
	// state, so nobody labels: the request stays pending for the sweep or the
	// next pickup to finish, which is what a marker is for.
	state, rerr := o.currentStateLabel(cctx, n)
	switch {
	case rerr != nil && fromLabel == "":
		return fmt.Errorf("issue #%d: marking stopped failed: reading its labels failed: %w", n, rerr)
	case rerr == nil:
		switch state {
		case o.cfg.StateLabels.Stopped:
			o.settleStopped(n)
			return nil
		case o.cfg.StateLabels.Done, o.cfg.StateLabels.NeedsInfo:
			// The run finished before the stop reached it. There is nothing left to
			// stop, and relabelling would take the issue back out of a state it
			// legitimately reached, so retire the request instead — a marker kept
			// here would be read as a fresh hold by the issue's next life.
			log.Printf("issue #%d: stop arrived after the run finished as %s — nothing to stop", n, state)
			clearStopRequest(o.issueLogDir(n))
			return nil
		}
		fromLabel = state
	}
	_ = o.gh.Comment(cctx, n, fmt.Sprintf(
		"🤖 Stopped by request. Progress is preserved — continue with `loope -continue %d` or the dashboard.", n))
	if fromLabel == "" {
		if err := o.gh.AddLabel(cctx, n, o.cfg.StateLabels.Stopped); err != nil {
			return fmt.Errorf("issue #%d: marking stopped failed: %w", n, err)
		}
	} else if err := o.gh.SwapLabels(cctx, n, fromLabel, o.cfg.StateLabels.Stopped); err != nil {
		return fmt.Errorf("issue #%d: marking stopped failed: %w", n, err)
	}
	o.settleStopped(n)
	return nil
}

// settleStopped records the bookkeeping of a stop that has LANDED — reached only
// once the ai-stopped label is on the issue, never on the failure path, where
// the hold must stay pending for the sweep to recover.
//
// Retiring the marker here is what makes "a marker exists" mean exactly one
// thing: a stop was requested and has not completed yet. A marker that outlived
// its stop would be read by the issue's next life (stopped, then re-queued by a
// human removing the label) as a fresh hold, which is why the claim used to
// clear markers it judged stale — a judgement no process could make correctly
// while another was writing one. The durable record of a completed stop is the
// label, which continue and the dashboard already read.
//
// The exception is a run owned by another process: it is still unwinding
// through its cancelled context and the marker is what tells it that the
// cancellation was a stop and not a shutdown. That process clears the marker
// when it finishes.
func (o *Orchestrator) settleStopped(n int) {
	logDir := o.issueLogDir(n)
	recordState(logDir, o.cfg.StateLabels.Stopped)
	clearParkCause(logDir)
	if !otherProcessRunning(logDir) {
		clearStopRequest(logDir)
	}
}

// releaseClaim ends a pipeline's claim on issue n and finishes a stop that
// arrived too late for the run itself to honour — after its last stop check but
// before it let the claim go. Every path that registers a run defers this.
//
// Not to be confused with the slot ledger's release (slots.go), which returns
// the concurrency budget a cycle handed out. This one retracts the "issue #N
// has a live run behind it" claim that Stop, continue and the orphan sweep all
// route on. A pipeline holds both, and gives up this one first.
//
// The late stop is a real one: the operator asked for the ticket to halt while
// it was still running, so it is finished here rather than discarded. Whether
// that means parking it as stopped or simply retiring the request is
// finishStopped's call — it re-reads the issue, and a run that had already
// shipped leaves nothing to stop.
//
// The context is the run's own, which a stop has cancelled by the time we get
// here; finishStopped works on a cancellation-proof copy of it.
func (o *Orchestrator) releaseClaim(ctx context.Context, n int, logDir string) {
	if !o.registry.release(n, logDir) {
		return
	}
	log.Printf("issue #%d: stop arrived as the run was finishing — settling it now", n)
	if err := o.finishStopped(ctx, n, ""); err != nil {
		log.Printf("issue #%d: settling a late stop failed (the request stays pending): %v", n, err)
	}
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
	logDir := o.issueLogDir(n)
	// Two guards for one question, because neither alone answers it: the registry
	// covers a run live in this process (a dashboard continue racing the daemon),
	// the owner file a run live in another (`loope -continue` against a daemon
	// that is mid-stop on the same issue). Either way a second Claude session in
	// the same worktree is the outcome to prevent.
	if o.registry.running(n) || otherProcessRunning(logDir) {
		return nil, fmt.Errorf("#%d is already running", n)
	}

	resumable := false
	if _, err := os.Stat(worktreePath(o.cfg.WorkDir, n)); err == nil {
		if si, serr := readSession(logDir); serr == nil && si.SessionID != "" {
			resumable = true
		}
	}

	// The marker is cleared only once the transition off ai-stopped has actually
	// landed. Clearing it first would, on a failed transition, leave the issue
	// labelled stopped with no pending hold anywhere — invisible to the sweep,
	// and un-stoppable again, since Stop short-circuits on an already-stopped
	// issue without re-writing the marker.
	if !resumable {
		if err := o.gh.RemoveLabel(ctx, n, o.cfg.StateLabels.Stopped); err != nil {
			return nil, fmt.Errorf("issue #%d: re-queueing failed: %w", n, err)
		}
		clearStopRequest(logDir)
		clearState(logDir)
		log.Printf("issue #%d: nothing to resume — re-queued for a fresh run", n)
		return nil, nil
	}
	if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.Stopped, o.cfg.StateLabels.WIP); err != nil {
		return nil, fmt.Errorf("issue #%d: marking wip failed: %w", n, err)
	}
	clearStopRequest(logDir)
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
// UI can report "#N is not stopped", "#N is already running" or a full budget —
// then runs the multi-minute resume in the background on the daemon's context.
//
// The resume is a full pipeline, so it takes a slot before anything else and
// holds it until it finishes. The slot is what makes it part of the two
// accountings a bare goroutine escaped: the ledger the poll cycle reads, so the
// loop backs off by one while an operator's run is going (see acquireOperator —
// the operator does not queue behind the budget, but the budget yields to them),
// and the in-flight set shutdown drains, so a SIGTERM no longer returns from the
// drain, releases the workDir lock and exits out from under a live session with
// none of its labeling done.
//
// The slot is taken BEFORE prepareContinue, so a refused continue leaves the
// ticket in the operator hold rather than swapping it to ai-wip for a run that
// never starts. Every path that does not reach the goroutine hands it straight
// back.
//
// The CLI's -continue needs none of this: it is the whole process, drives one
// pipeline, and returns before main does.
func (c orchestratorController) Continue(n int) error {
	if !c.o.acquireOperator(n) {
		return c.o.operatorRefusal(n)
	}
	run, err := c.o.prepareContinue(c.o.base(), n)
	if err != nil || run == nil {
		c.o.release(n)
		return err
	}
	go func() {
		defer c.o.release(n)
		if err := guard("continue", func() error { return run(c.o.base()) }); err != nil {
			log.Printf("continue #%d: %v", n, err)
		}
	}()
	return nil
}
