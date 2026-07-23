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
	if o.active == nil {
		o.active = map[int]struct{}{}
	}
	if _, busy := o.active[n]; busy {
		return false
	}
	if len(o.active) >= o.slots() {
		return false
	}
	o.active[n] = struct{}{}
	o.inFlight.Add(1)
	return true
}

// slotRefusal explains why tryAcquire turned issue n away, for a caller that has
// a human waiting on the answer (the dashboard). It re-reads the ledger, so it
// can in principle name a reason that has just stopped being true — this is an
// error message, not a decision.
func (o *Orchestrator) slotRefusal(n int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, busy := o.active[n]; busy {
		return fmt.Errorf("#%d is already running", n)
	}
	return fmt.Errorf("#%d cannot start yet: all %d ticket slots are busy — try again when one finishes", n, o.slots())
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

// Wait blocks until every in-flight pipeline and resume has finished. runLoop
// calls it before returning so the workDir lock outlives all work, exactly as it
// did when ProcessOnce blocked on its own WaitGroup.
func (o *Orchestrator) Wait() { o.inFlight.Wait() }
