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
