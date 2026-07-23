package main

import "fmt"

// The slot ledger turns ticketsPerCycle into a live concurrency budget. A cycle
// tops the in-flight set back up to the budget and returns; pipelines started in
// different cycles run side by side. The in-process ledger is authoritative —
// slots are NOT derived from counting ai-wip issues on GitHub, because the
// daemon holds an exclusive workDir lock (so no other instance can own live
// pipelines) and the label can lag or fail to apply.

// slots is the effective budget: ticketsPerCycle, clamped to a minimum of 1.
// Callers must hold mu (cfg is immutable, but every caller is already inside the
// critical section and mu is not reentrant).
func (o *Orchestrator) slots() int {
	n := o.cfg.TicketsPerCycle
	if n < 1 {
		n = 1
	}
	return n
}

// tryAcquire claims a slot for issue n, reporting whether it got one. It refuses
// when n is already in flight or the budget is full.
//
// The already-in-flight check is not redundant with the ai-wip label check. It
// closes two real windows: between launching a pipeline and its AddLabel(ai-wip)
// landing the issue still looks eligible to ListEligibleIssues, and park swaps
// ai-wip->ai-rework before the pipeline goroutine returns, so ResumeParked in the
// same cycle could otherwise resume an issue whose goroutine still holds its
// worktree.
func (o *Orchestrator) tryAcquire(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.active) >= o.slots() {
		return false
	}
	return o.take(n)
}

// acquireOperator claims a slot for work a human asked for by name, waiting for
// no budget. The budget bounds the LOOP's own concurrency; queueing an operator
// behind it means queueing them behind an eligible list that may never empty —
// with the default budget of one they would be racing the poll cycle for a slot
// that reopens for a few seconds every few minutes, and nothing records the
// intent or retries it.
//
// The over-commit is self-correcting rather than unbounded: freeSlots floors at
// zero, so the cycles that follow start no new work until the operator's run
// finishes. Total concurrency stays at the budget plus what a human has
// explicitly asked for — the same accounting `loope -continue` in another shell
// has always had, since a second process shares no ledger at all.
//
// It refuses only a genuine conflict: a run already in flight for this issue,
// or a daemon that is shutting down.
func (o *Orchestrator) acquireOperator(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.take(n)
}

// take records the slot. Callers must hold mu.
func (o *Orchestrator) take(n int) bool {
	if o.draining {
		return false
	}
	if o.active == nil {
		o.active = map[int]struct{}{}
	}
	if _, busy := o.active[n]; busy {
		return false
	}
	o.active[n] = struct{}{}
	o.inFlight.Add(1)
	return true
}

// operatorRefusal explains why acquireOperator turned issue n away, for a caller
// with a human waiting on the answer (the dashboard).
func (o *Orchestrator) operatorRefusal(n int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.draining {
		return fmt.Errorf("the daemon is shutting down — start it again to continue #%d", n)
	}
	return fmt.Errorf("#%d is already running", n)
}

// release returns issue n's slot. Every successful tryAcquire must be paired
// with exactly one release, deferred first in the goroutine so it runs last.
func (o *Orchestrator) release(n int) {
	o.mu.Lock()
	delete(o.active, n)
	o.mu.Unlock()
	o.inFlight.Done()
}

// freeSlots reports how many pipelines may still be started, floored at zero.
func (o *Orchestrator) freeSlots() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	free := o.slots() - len(o.active)
	if free < 0 {
		return 0
	}
	return free
}

// filterInactive drops issues that already have a pipeline in flight, so a stale
// listing can't start a second run for one.
func (o *Orchestrator) filterInactive(issues []Issue) []Issue {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := issues[:0:0]
	for _, is := range issues {
		if _, busy := o.active[is.Number]; busy {
			continue
		}
		out = append(out, is)
	}
	return out
}

// Wait blocks until every in-flight pipeline and resume has finished.
func (o *Orchestrator) Wait() { o.inFlight.Wait() }

// drain closes the ledger and then waits for every slot still out to come back.
// runLoop calls it before returning, so the workDir lock outlives all work.
//
// Closing it first is what makes waiting safe at all. The dashboard's continue
// takes a slot from an HTTP handler, which can run at any moment — including
// while a drain is under way, since closing the listener does not join handlers
// already inside one. A WaitGroup panics if an Add lands concurrently with a
// Wait, and net/http would swallow that panic, leaving the ledger holding a slot
// that never comes back and an issue this process can never start again. Under
// the same mutex every acquire takes, no Add can follow the flag.
func (o *Orchestrator) drain() {
	o.mu.Lock()
	o.draining = true
	o.mu.Unlock()
	o.inFlight.Wait()
}
