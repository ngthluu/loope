package main

import (
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
	for _, name := range []string{"config", "once", "rework", "serve", "addr", "version", "stop", "continue"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("flag -%s must be registered", name)
		}
	}
}
