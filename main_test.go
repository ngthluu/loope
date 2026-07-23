package main

import (
	"bytes"
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

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name       string
		configPath string
		version    bool
		doctor     bool
		want       cliMode
	}{
		{"version wins over config and doctor", "loope.json", true, true, modeVersion},
		{"version without config", "", true, false, modeVersion},
		{"config runs", "loope.json", false, false, modeRun},
		{"config with doctor still runs", "loope.json", false, true, modeRun},
		{"bare invocation is help", "", false, false, modeHelp},
		{"doctor without config is a usage error", "", false, true, modeDoctorNoConfig},
	}
	for _, c := range cases {
		if got := resolveMode(c.configPath, c.version, c.doctor); got != c.want {
			t.Errorf("%s: resolveMode(%q, %v, %v) = %d, want %d",
				c.name, c.configPath, c.version, c.doctor, got, c.want)
		}
	}
}
