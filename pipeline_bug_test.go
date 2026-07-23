package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBugPipelineSingleDebugSession(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("Fixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus", Effort: "high"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.calls))
	}
	call := f.calls[0]
	prompt := call.stdin
	if !strings.Contains(prompt, "/superpowers:systematic-debugging") || !strings.Contains(prompt, "ISSUE") ||
		!strings.Contains(prompt, "failing test first") {
		t.Errorf("prompt = %s", prompt)
	}
	if call.dir != "/wt" || argAfter(call.args, "--permission-mode") != permissionModeAuto ||
		argAfter(call.args, "--model") != "opus" {
		t.Errorf("call = %+v", call)
	}
}

func TestBugPipelinePropagatesError(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{err: fmt.Errorf("exit 1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "issue"); err == nil {
		t.Error("want error, got nil")
	}
}

func TestBugPipelineReturnsAlreadyDone(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		"I reproduced nothing; the guard already exists.\nPIPELINE_ALREADY_DONE: fixed in guard.go", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE")
	var done *alreadyDoneError
	if !errors.As(err, &done) {
		t.Fatalf("want *alreadyDoneError, got %v", err)
	}
	if done.reason != "fixed in guard.go" {
		t.Errorf("reason = %q", done.reason)
	}
}

func TestBugPipelineRecordsSession(t *testing.T) {
	logDir := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		return claudeJSON("Fixed and committed.", "debug-sess"), "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "the issue"); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "debug-sess" || si.Kind != "bug" {
		t.Errorf("session = %+v, want debug-sess/bug", si)
	}
}

// TestBugPipelineRecordsSessionOnError reproduces the -rework gap: the debug
// call errored (e.g. a 429 session limit) but returned a valid session id. The
// pipeline must still persist that session so `loop -rework <N>` can resume it,
// while propagating the error so the issue gets parked.
func TestBugPipelineRecordsSessionOnError(t *testing.T) {
	logDir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: claudeErrorJSON("You've hit your session limit", "debug-429")}}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "the issue"); err == nil {
		t.Fatal("want the error propagated so the issue is parked")
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session must be recorded even when the call errors, so -rework can resume: %v", err)
	}
	if si.SessionID != "debug-429" || si.Kind != "bug" {
		t.Errorf("session = %+v, want debug-429/bug", si)
	}
}

func TestBugPipelinePromptMentionsAlreadyDoneSentinel(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("Fixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.calls[0].stdin, alreadyDoneSentinel) {
		t.Errorf("bug prompt should tell the architect how to signal already-done:\n%s", f.calls[0].stdin)
	}
}
