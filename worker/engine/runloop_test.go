package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGuardConvertsPanicToError(t *testing.T) {
	err := guard("cycle", func() error { panic("boom") })
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the panic message", err)
	}
	if err := guard("cycle", func() error { return nil }); err != nil {
		t.Fatalf("clean run must return nil, got %v", err)
	}
	want := errors.New("plain")
	if err := guard("cycle", func() error { return want }); err != want {
		t.Fatalf("plain errors must pass through, got %v", err)
	}
}

// The workDir lock is released by main's deferred release() after runLoop
// returns, so runLoop must not return while pipelines are still running — a
// second daemon could otherwise steal live ai-wip work. Shutdown is now driven
// only by context cancellation, so the loop must drain in-flight pipelines on
// that path before returning.
func TestRunLoopDrainsInFlightPipelinesOnCancel(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	// A long poll interval parks the loop in its select so cancellation is the
	// only wake-up, making the drain deterministic.
	o.cfg.PollIntervalSec = 3600
	started, release := gatePipelines(o, env.f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, o, o.cfg, false /* sweep */)
		close(done)
	}()

	awaitStarted(t, started, 1)
	cancel() // signal shutdown while the pipeline is still gated

	select {
	case <-done:
		t.Fatal("runLoop returned while a pipeline was still in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not return after pipelines drained")
	}
	if n := len(env.callsMatching("gh", "pr create")); n != 1 {
		t.Fatalf("pr create count = %d, want 1 (the pipeline must have completed)", n)
	}
}
