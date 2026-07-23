package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
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
