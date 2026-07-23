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
	if call.dir != "/wt" || !hasArg(call.args, "--dangerously-skip-permissions") ||
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

func TestBugPipelineLowConfidenceEscalates(t *testing.T) {
	// A one-element queue, not a handler: a handler would answer every call with
	// the same low score, so the call-count assertion below could never catch a
	// pipeline that kept going.
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		"CONFIDENCE: 40\nNo stack trace and no repro steps.\nWhich command triggers the crash?", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "crashes sometimes on startup")
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if lc.score != 40 {
		t.Errorf("score = %d, want 40", lc.score)
	}
	if !strings.Contains(lc.feedback, "repro steps") || strings.Contains(lc.feedback, confidenceSentinel) {
		t.Errorf("feedback should carry the reasons without the CONFIDENCE line: %q", lc.feedback)
	}
	if len(f.calls) != 1 {
		t.Errorf("low confidence must stop after the debug turn, got %d calls", len(f.calls))
	}
}

func TestBugPipelineHighConfidenceProceeds(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("CONFIDENCE: 85\nFixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatalf("a score at or above the threshold must proceed: %v", err)
	}
}

// A score exactly at the threshold is not below it, so it proceeds.
func TestBugPipelineConfidenceAtThresholdProceeds(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("CONFIDENCE: 70\nFixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatalf("score == threshold must proceed: %v", err)
	}
}

// confidenceThreshold: 0 disables the gate entirely — even an explicit low score
// in the output is ignored.
func TestBugPipelineZeroThresholdIgnoresScore(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("CONFIDENCE: 5\nFixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatalf("threshold 0 disables the gate: %v", err)
	}
}

// Fail open: a session that forgot the sentinel but fixed the bug still ships.
func TestBugPipelineMissingSentinelFailsOpen(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("Fixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatalf("an absent score must fail open, got %v", err)
	}
}

// Confidence outranks already-done: a session too unsure to fix the bug must not
// be able to close the issue as already implemented either.
func TestBugPipelineLowConfidenceBeatsAlreadyDone(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		"CONFIDENCE: 20\nI cannot tell what behavior is wrong.\nPIPELINE_ALREADY_DONE: looks fine to me", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE")
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %T (%v)", err, err)
	}
	var done *alreadyDoneError
	if errors.As(err, &done) {
		t.Error("a low-confidence session must not close the issue as already done")
	}
	// The feedback is posted verbatim as a public GitHub comment, so the ignored
	// already-done claim must not leak into it.
	if strings.Contains(lc.feedback, alreadyDoneSentinel) {
		t.Errorf("needs-info feedback must not leak the already-done sentinel: %q", lc.feedback)
	}
}
