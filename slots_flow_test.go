package main

import (
	"context"
	"testing"
	"time"
)

// ProcessOnce must launch pipelines and return, so the next poll cycle can top
// the in-flight set back up instead of waiting for the batch.
func TestProcessOnceReturnsBeforePipelinesFinish(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	returned := make(chan error, 1)
	go func() { returned <- o.ProcessOnce(context.Background()) }()

	awaitStarted(t, started, 1)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("ProcessOnce error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessOnce did not return while its pipeline was still running")
	}

	close(release)
	o.Wait()
	if n := len(env.callsMatching("gh", "pr create")); n != 1 {
		t.Fatalf("pr create count = %d, want 1 after Wait", n)
	}
	if free := o.freeSlots(); free != 1 {
		t.Fatalf("freeSlots after drain = %d, want 1 (slot released)", free)
	}
}

// A continue started from the dashboard runs a full pipeline — the same
// multi-minute Claude session as any other — so it has to be in the ledger every
// other pipeline is in. It used to be in neither half of it: shutdown drained
// only the cycle's pipelines and then exited out from under it, killing the
// session mid-flight with none of its labeling done, and the poll loop kept
// starting work as though nothing were running.
//
// It does NOT queue behind the budget, though. A human asking for one named
// ticket would be racing the poll cycle for a slot that reopens for seconds
// every few minutes; the loop yields to them instead, by having no free slots
// while their run is in flight.
func TestDashboardContinueTakesASlotTheLoopThenYields(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 1
	env.stateLabels(9, "ai-stopped")
	seedResumable(t, o, 9, "sess-9")
	started, release := gatePipelines(o, env.f)
	ctl := o.controller()

	// One pipeline holds the only slot.
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := awaitStarted(t, started, 1); got[0] != 7 {
		t.Fatalf("first pipeline = #%d, want #7", got[0])
	}

	if err := ctl.Continue(9); err != nil {
		t.Fatalf("an operator continue must not queue behind the loop's budget: %v", err)
	}
	if got := awaitStarted(t, started, 1); got[0] != 9 {
		t.Fatalf("continued pipeline = #%d, want #9", got[0])
	}

	// ...and the loop yields for as long as it runs: no free slots, so the next
	// cycle starts nothing new.
	if free := o.freeSlots(); free != 0 {
		t.Fatalf("freeSlots = %d, want 0 with the budget over-committed", free)
	}
	env.setEligible(7, 8)
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNoStart(t, started, 200*time.Millisecond)

	// Shutdown drains the continue like anything else.
	drained := make(chan struct{})
	go func() { o.drain(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("shutdown drained while pipelines were still mid-session")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the continue never released its slot")
	}
}

// Every way out of Continue that does not reach the goroutine has to hand the
// slot back. A leaked one is permanent: with the default budget of one, a single
// refused click stops the daemon picking up any further work, and the shutdown
// drain then waits forever for a run that never started.
func TestARefusedDashboardContinueHandsItsSlotBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label string
	}{
		{"not stopped", "ai-wip"},           // prepareContinue returns an error
		{"nothing to resume", "ai-stopped"}, // prepareContinue re-queues: no run
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newSlotEnv(t)
			o := env.orchestrator()
			o.cfg.TicketsPerCycle = 1
			env.stateLabels(9, tc.label)

			_ = o.controller().Continue(9)

			if free := o.freeSlots(); free != 1 {
				t.Fatalf("freeSlots = %d, want 1: a continue that started nothing must return its slot", free)
			}
			drained := make(chan struct{})
			go func() { o.drain(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown hung waiting for a run that never started")
			}
		})
	}
}

// A continue arriving as the daemon shuts down must be refused outright. The
// dashboard's handler can run at any moment — closing the listener does not join
// handlers already inside one — and a slot taken after the drain started would
// panic the WaitGroup, which net/http would swallow, leaving the ledger holding
// a slot that never comes back.
func TestDashboardContinueIsRefusedOnceTheDrainStarts(t *testing.T) {
	env := newSlotEnv(t)
	o := env.orchestrator()
	env.stateLabels(9, "ai-stopped")
	seedResumable(t, o, 9, "sess-9")

	o.drain() // nothing in flight, so this returns at once and closes the ledger

	if err := o.controller().Continue(9); err == nil {
		t.Fatal("a continue arriving during shutdown must be refused")
	}
	if len(env.callsMatching("gh", "--add-label ai-wip")) != 0 {
		t.Fatal("a refused continue must leave the ticket in the operator hold")
	}
	if len(env.callsMatching("claude", "--resume")) != 0 {
		t.Fatal("no session may be started during shutdown")
	}
}

// The reported scenario: budget 3, two pipelines already in flight from an
// earlier cycle, a third issue labelled while they run. The next cycle must
// start exactly that third issue without waiting for the first two.
func TestProcessOnceTopsUpAcrossCycles(t *testing.T) {
	env := newSlotEnv(t, 7, 8)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 3
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("first cycle error = %v, want nil", err)
	}
	first := awaitStarted(t, started, 2)
	if len(first) != 2 {
		t.Fatalf("first cycle started %v, want two pipelines", first)
	}

	// A third issue becomes eligible while the first two are still blocked. The
	// listing still contains the two in-flight ones (their labels are applied,
	// but a stale listing is exactly the case the filter must handle).
	env.setEligible(7, 8, 9)
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("second cycle error = %v, want nil", err)
	}
	third := awaitStarted(t, started, 1)
	if third[0] != 9 {
		t.Fatalf("second cycle started issue #%d, want #9", third[0])
	}
	if free := o.freeSlots(); free != 0 {
		t.Fatalf("freeSlots = %d, want 0 with three pipelines in flight", free)
	}

	close(release)
	o.Wait()
	if n := len(env.callsMatching("gh", "pr create")); n != 3 {
		t.Fatalf("pr create count = %d, want 3", n)
	}
}

// Budget 2 with three eligible issues: only two pipelines start; the third
// waits for a completion to free a slot.
func TestProcessOnceRespectsBudget(t *testing.T) {
	env := newSlotEnv(t, 7, 8, 9)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 2
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	awaitStarted(t, started, 2)
	assertNoStart(t, started, 200*time.Millisecond)

	// A cycle at a full budget starts nothing.
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("second cycle error = %v, want nil", err)
	}
	assertNoStart(t, started, 200*time.Millisecond)

	close(release)
	o.Wait()

	// Slots freed: the third issue now starts.
	env.setEligible(9)
	if err := runCycle(o); err != nil {
		t.Fatalf("third cycle error = %v, want nil", err)
	}
	wip := env.callsMatching("gh", "--add-label ai-wip")
	if len(wip) != 3 {
		t.Fatalf("ai-wip labels = %d, want 3 (all three issues eventually run)", len(wip))
	}
}

// With no free slot, a cycle must not even ask GitHub for the queue.
func TestProcessOnceFullBudgetSkipsListing(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator() // budget clamps to 1
	if !o.tryAcquire(42) {
		t.Fatal("setup: acquire must succeed")
	}
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	if got := env.callsMatching("gh", "issue list"); len(got) != 0 {
		t.Fatalf("a full budget must make no gh issue list call, got %v", got)
	}
	o.release(42)
}

// A stale listing that still includes an in-flight issue must not start a
// second pipeline for it.
func TestProcessOnceFiltersInFlightIssues(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 3
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("first cycle error = %v, want nil", err)
	}
	awaitStarted(t, started, 1)

	// Same listing again: #7 is in flight, nothing else is eligible.
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("second cycle error = %v, want nil", err)
	}
	assertNoStart(t, started, 200*time.Millisecond)

	close(release)
	o.Wait()
	if wip := env.callsMatching("gh", "--add-label ai-wip"); len(wip) != 1 {
		t.Fatalf("ai-wip labels = %d, want 1 (no duplicate pipeline for #7)", len(wip))
	}
	// The second cycle must not have burned a triage call either: the filtered
	// list was empty, so selection was skipped.
	if triage := env.callsMatching("claude", ""); len(triage) == 0 {
		t.Fatal("sanity: the first cycle should have made claude calls")
	}
}

// Resumes draw from the same budget as new work: with every slot taken by a
// pipeline, no resume starts.
func TestResumeParkedYieldsToFullBudget(t *testing.T) {
	env := newSlotEnv(t, 7)
	env.setRework(11)
	prepParkedIn(t, env.fakeEnv, 11, "api status 429: usage limit")
	o := env.orchestrator() // budget clamps to 1
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	awaitStarted(t, started, 1)

	if err := o.ResumeParked(context.Background()); err != nil {
		t.Fatalf("ResumeParked error = %v, want nil", err)
	}
	if got := env.callsMatching("claude", "--resume"); len(got) != 0 {
		t.Fatalf("no slot free, want no resume, got %v", got)
	}

	close(release)
	o.Wait()
}

// An issue whose pipeline is still in flight must not be resumed, even after
// park has already swapped its label to ai-rework.
func TestResumeParkedSkipsIssueStillInFlight(t *testing.T) {
	env := newSlotEnv(t, 7)
	env.setRework(7)
	prepParkedIn(t, env.fakeEnv, 7, "api status 429: usage limit")
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 3
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	awaitStarted(t, started, 1)

	if err := o.ResumeParked(context.Background()); err != nil {
		t.Fatalf("ResumeParked error = %v, want nil", err)
	}
	if got := env.callsMatching("claude", "--resume"); len(got) != 0 {
		t.Fatalf("issue #7 is in flight, want no resume, got %v", got)
	}

	close(release)
	o.Wait()
}

// With a free slot and no competing work, a resume starts and returns before
// the resume session finishes.
func TestResumeParkedRunsConcurrently(t *testing.T) {
	env := newSlotEnv(t) // nothing eligible
	env.setRework(11)
	prepParkedIn(t, env.fakeEnv, 11, "api status 429: usage limit")
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	returned := make(chan error, 1)
	go func() { returned <- o.ResumeParked(context.Background()) }()
	awaitStarted(t, started, 1)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("ResumeParked error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResumeParked did not return while its resume was still running")
	}
	close(release)
	o.Wait()
	if got := env.callsMatching("claude", "--resume"); len(got) == 0 {
		t.Fatal("want the saved session resumed")
	}
	if free := o.freeSlots(); free != 1 {
		t.Fatalf("freeSlots after drain = %d, want 1", free)
	}
}
