package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
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

func TestRegisterFlagsParsesStopAndContinue(t *testing.T) {
	fs := flag.NewFlagSet("loope", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := registerFlags(fs)
	if err := fs.Parse([]string{"-stop", "42"}); err != nil {
		t.Fatal(err)
	}
	if *f.stopIssue != 42 {
		t.Fatalf("-stop = %d, want 42", *f.stopIssue)
	}

	fs2 := flag.NewFlagSet("loope", flag.ContinueOnError)
	fs2.SetOutput(io.Discard)
	f2 := registerFlags(fs2)
	if err := fs2.Parse([]string{"-continue", "7"}); err != nil {
		t.Fatal(err)
	}
	if *f2.continueIssue != 7 {
		t.Fatalf("-continue = %d, want 7", *f2.continueIssue)
	}
	if *f2.stopIssue != 0 {
		t.Fatalf("-stop default = %d, want 0", *f2.stopIssue)
	}
}

func TestRegisterFlagsKeepsExistingFlags(t *testing.T) {
	fs := flag.NewFlagSet("loope", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerFlags(fs)
	for _, name := range []string{"config", "once", "rework", "serve", "addr", "version", "stop", "continue", "doctor"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("flag -%s must be registered", name)
		}
	}
}

// The workDir lock is released by main's deferred release() after runLoop
// returns, so runLoop must not return while pipelines are still running — a
// second daemon could otherwise steal live ai-wip work. Before slots, this held
// because ProcessOnce blocked; now it must be explicit.
func TestRunLoopOnceDrainsInFlightPipelines(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	done := make(chan struct{})
	go func() {
		runLoop(context.Background(), o, o.cfg, true /* once */, false /* sweep */)
		close(done)
	}()

	awaitStarted(t, started, 1)
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

// Recovery has to be continuous, not a boot-time formality. Everything that can
// strand a ticket — a label swap that failed, a resume that could not start, a
// stop nobody could finish — happens while the daemon is up, and a sweep that
// only ran at startup meant every one of those was "stuck until a human restarts
// the daemon".
func TestRunLoopSweepsEveryCycleNotJustAtStartup(t *testing.T) {
	env := newSlotEnv(t)
	o := env.orchestrator()
	o.cfg.PollIntervalSec = 0
	captureLog(t)
	// A stop nobody finished — the daemon was down, or GitHub was — waiting for
	// the second sweep to pick it up.
	env.stateLabels(5, "ai-wip")
	recordStopRequest(o.issueLogDir(5))
	abandonStops(o)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runLoop(ctx, o, o.cfg, false /* once */, true /* sweeping */)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for {
		sweeps := len(env.callsMatching("gh", "issue list --repo org/repo --label ai-wip"))
		settled := len(env.callsMatching("gh", "--add-label ai-stopped"))
		if sweeps >= 3 && settled > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("orphan sweep ran %d times (want it every cycle) and the stop sweep settled %d pending stops (want 1)",
				sweeps, settled)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not return after cancellation")
	}
}

func TestGateBlocksOnRequiredFailure(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"claude --version": {err: errors.New("not found")},
	})}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), false)
	if proceed {
		t.Fatal("proceed = true, want false when a required check failed")
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "claude") {
		t.Fatalf("report must name the failing check, got %q", buf.String())
	}
}

func TestGateWarningsOnlyProceedSilently(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"curl --version": {err: errors.New("not found")},
	})}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), false)
	if !proceed || code != 0 {
		t.Fatalf("gate = (%d, %v), want (0, true) for warnings only", code, proceed)
	}
	if buf.String() != "" {
		t.Fatalf("a healthy run must print nothing, got %q", buf.String())
	}
}

func TestGateDoctorAlwaysReportsAndNeverProceeds(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), true)
	if proceed {
		t.Fatal("-doctor must never proceed into the loop")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when everything passes", code)
	}
	if !strings.Contains(buf.String(), "loope preflight") {
		t.Fatalf("-doctor must print the report even when healthy, got %q", buf.String())
	}

	f2 := &fakeRunner{handler: okHandler(map[string]rresp{"gh --version": {err: errors.New("not found")}})}
	var buf2 bytes.Buffer
	code2, _ := gate(context.Background(), &buf2, f2, preflightConfig(), true)
	if code2 != 1 {
		t.Fatalf("-doctor exit code = %d, want 1 when a required check failed", code2)
	}
}
