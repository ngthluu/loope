package main

import (
	"context"
	"testing"
)

// A sweep that failed at boot is retried on later cycles — and by then this
// process has its own live pipelines wearing ai-wip. The sweep exists to
// re-queue a CRASHED run's leftovers, so it must skip anything the slot ledger
// says is in flight: stripping a live pipeline's ai-wip label would let the next
// cycle start a second run for the same issue.
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
		t.Errorf("sweep relabelled a live pipeline's issue (%d removals), want 0", n)
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
func TestSweepOrphansStillRequeuesGenuineOrphans(t *testing.T) {
	env := newSlotEnv(t)
	o := env.orchestrator()
	env.setWIP(4)

	if err := o.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n := len(env.callsMatching("gh", "--remove-label ai-wip")); n != 1 {
		t.Fatalf("orphan not re-queued: %d ai-wip removals, want 1", n)
	}
}
