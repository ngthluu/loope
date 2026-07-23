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
