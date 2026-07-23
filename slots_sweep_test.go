package main

import (
	"context"
	"testing"
	"time"
)

// A sweep that failed at boot is retried on later cycles — and by then this
// process has its own live pipelines wearing ai-wip. The sweep exists to clean
// up a CRASHED run's leftovers, so it must skip anything the slot ledger says is
// in flight: parking a live issue relabels it out from under its own pipeline,
// and the reclaim path force-removes the worktree that pipeline is committing
// into.
func TestSweepOrphansSkipsInFlightPipelines(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("ProcessOnce: %v", err)
	}
	awaitStarted(t, started, 1)

	// The live pipeline has applied ai-wip, so the retried sweep now sees it.
	env.setWIP(7)
	if err := o.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}

	if n := len(env.callsMatching("gh", "--remove-label ai-wip")); n != 0 {
		t.Errorf("sweep relabelled a live pipeline's issue (%d swaps), want 0", n)
	}
	if n := len(env.callsMatching("git", "worktree remove")); n != 0 {
		t.Errorf("sweep removed a live pipeline's worktree (%d calls), want 0", n)
	}
	if n := len(env.callsMatching("git", "branch -D")); n != 0 {
		t.Errorf("sweep deleted a live pipeline's branch (%d calls), want 0", n)
	}

	// Drain before returning: the pipeline goroutine writes under workDir, and
	// t.TempDir's cleanup would race it.
	close(release)
	o.Wait()
}

// A genuine orphan — ai-wip with no pipeline in flight — must still be swept.
// The in-flight filter narrows the sweep; it must not disable it.
func TestSweepOrphansStillReclaimsGenuineOrphans(t *testing.T) {
	env := newSlotEnv(t)
	o := env.orchestrator()
	env.setWIP(4)

	if err := o.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n := len(env.callsMatching("gh", "--remove-label ai-wip")); n != 1 {
		t.Fatalf("orphan not reclaimed: %d ai-wip removals, want 1", n)
	}
}

// ProcessOnce ran first and always claimed every free slot, so a parked issue
// could never be resumed while eligible work kept arriving — its preserved
// worktree and session would sit forever. Resuming existing work takes priority
// over starting new work.
func TestResumeIsNotStarvedByAFullEligibleQueue(t *testing.T) {
	env := newSlotEnv(t, 1, 2, 3)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 2
	env.setRework(5)
	prepParkedIn(t, env.fakeEnv, 5, "usage limit reached")

	started, release := gatePipelines(o, env.f)
	done := make(chan struct{})
	go func() {
		runLoop(context.Background(), o, o.cfg, true /* once */, false /* sweep */)
		close(done)
	}()

	got := awaitStarted(t, started, 2)
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not return after pipelines drained")
	}

	resumed := false
	for _, n := range got {
		if n == 5 {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("parked issue #5 was starved by the eligible queue; started %v", got)
	}
}

// Starving resumes must not flip into starving new work: with slots to spare,
// a cycle runs the resume AND tops up from the eligible queue.
func TestResumeAndNewWorkShareTheBudget(t *testing.T) {
	env := newSlotEnv(t, 1)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 2
	env.setRework(5)
	prepParkedIn(t, env.fakeEnv, 5, "usage limit reached")

	started, release := gatePipelines(o, env.f)
	done := make(chan struct{})
	go func() {
		runLoop(context.Background(), o, o.cfg, true /* once */, false /* sweep */)
		close(done)
	}()

	got := awaitStarted(t, started, 2)
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not return after pipelines drained")
	}

	seen := map[int]bool{}
	for _, n := range got {
		seen[n] = true
	}
	if !seen[5] || !seen[1] {
		t.Fatalf("want both the resume (#5) and new work (#1) started, got %v", got)
	}
}
