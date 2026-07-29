package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestResumePipelineResumesTheSavedSession(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("Finished the implementation and committed.", "s2")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus", Effort: "high"}}}
	if err := RunResumePipeline(context.Background(), c, cfg, "/wt", "bug", "s1"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.calls))
	}
	call := f.calls[0]
	if argAfter(call.args, "--resume") != "s1" {
		t.Errorf("must resume the saved session id, args = %v", call.args)
	}
	if call.dir != "/wt" || !hasArg(call.args, "--dangerously-skip-permissions") ||
		argAfter(call.args, "--model") != "opus" {
		t.Errorf("call = %+v", call)
	}
}

// A rework pickup must never close the issue as already-implemented: that
// check exists to let a FRESH session bail out of duplicate work, and applied
// to a session mid-way through implementing the issue it is the exact bug
// this pipeline exists to fix (closing with no PR). So even a reply carrying
// the sentinel is not parsed as one.
func TestResumePipelineNeverReturnsAlreadyDone(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		"PIPELINE_ALREADY_DONE: looks finished to me", "s2")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunResumePipeline(context.Background(), c, cfg, "/wt", "feature", "s1"); err != nil {
		t.Fatalf("resume must not treat an already-done claim as an error outcome: %v", err)
	}
}

func TestResumePipelinePropagatesError(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{err: fmt.Errorf("exit 1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunResumePipeline(context.Background(), c, cfg, "/wt", "bug", "s1"); err == nil {
		t.Error("want error, got nil")
	}
}

func TestResumePipelineRecordsSession(t *testing.T) {
	logDir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("Finished and committed.", "s2")}}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunResumePipeline(context.Background(), c, cfg, "/wt", "feature", "s1"); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "s2" || si.Kind != "feature" {
		t.Errorf("session = %+v, want s2/feature", si)
	}
}

// A resume that errors again (e.g. a fresh 429) must still advance the saved
// session id, exactly like the bug/feature pipelines, so the NEXT rework
// pickup resumes the latest session instead of the now-stale one this run
// started from.
func TestResumePipelineRecordsSessionOnError(t *testing.T) {
	logDir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: claudeErrorJSON("hit usage limit again", "s3")}}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunResumePipeline(context.Background(), c, cfg, "/wt", "bug", "s1"); err == nil {
		t.Fatal("want the error propagated so the issue is parked again")
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session must be recorded even when the call errors: %v", err)
	}
	if si.SessionID != "s3" {
		t.Errorf("session = %+v, want s3", si)
	}
}

func TestResumePipelinePromptContinuesRatherThanRestarts(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("done", "s2")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunResumePipeline(context.Background(), c, cfg, "/wt", "bug", "s1"); err != nil {
		t.Fatal(err)
	}
	prompt := f.calls[0].stdin
	if !strings.Contains(strings.ToLower(prompt), "resuming") && !strings.Contains(strings.ToLower(prompt), "continue") {
		t.Errorf("resume prompt should tell the session to continue rather than restart: %s", prompt)
	}
	if strings.Contains(prompt, alreadyDoneSentinel) {
		t.Errorf("resume prompt must not invite an already-done claim: %s", prompt)
	}
}
