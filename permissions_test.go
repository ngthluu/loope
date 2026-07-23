package main

import (
	"context"
	"strings"
	"testing"
)

// assertUnattended pins the permission policy every pipeline session must run
// under: auto permission mode (never the blanket --dangerously-skip-permissions
// bypass), the pipeline's own git/gh operations pre-approved so an auto-mode
// escalation can't abort a headless run, and AskUserQuestion denied so a
// session can never stop waiting on a human.
func assertUnattended(t *testing.T, label string, call rcall) {
	t.Helper()
	if call.name != "claude" {
		t.Fatalf("%s: not a claude call: %v", label, call.name)
	}
	if hasArg(call.args, "--dangerously-skip-permissions") {
		t.Errorf("%s: must not bypass permissions wholesale: %v", label, call.args)
	}
	if got := argAfter(call.args, "--permission-mode"); got != permissionModeAuto {
		t.Errorf("%s: --permission-mode = %q, want %q (args: %v)", label, got, permissionModeAuto, call.args)
	}
	if got := argAfter(call.args, "--disallowedTools"); !strings.Contains(got, "AskUserQuestion") {
		t.Errorf("%s: --disallowedTools = %q, must deny AskUserQuestion", label, got)
	}
	allowed := argAfter(call.args, "--allowedTools")
	for _, want := range pipelineAllowedTools {
		if !strings.Contains(allowed, want) {
			t.Errorf("%s: --allowedTools = %q, missing %q", label, allowed, want)
		}
	}
}

func TestCallEmitsPermissionModeAndAllowedTools(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	_, err := c.Call(context.Background(), ClaudeCall{
		Dir: "/wt", Label: "debug", Prompt: "hi",
		PermissionMode: permissionModeAuto,
		AllowedTools:   []string{"Bash(git *)", "Bash(gh *)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	call := f.calls[0]
	if hasArg(call.args, "--dangerously-skip-permissions") {
		t.Errorf("args must not contain the bypass flag: %v", call.args)
	}
	if got := argAfter(call.args, "--permission-mode"); got != "auto" {
		t.Errorf("--permission-mode = %q, want auto", got)
	}
	if got := argAfter(call.args, "--allowedTools"); got != "Bash(git *),Bash(gh *)" {
		t.Errorf("--allowedTools = %q", got)
	}
}

// An unset PermissionMode must not emit the flag at all: the triage call runs
// under claude's default mode and shouldn't grow a bogus empty argument.
func TestCallOmitsPermissionFlagsWhenUnset(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "triage", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	call := f.calls[0]
	if hasArg(call.args, "--permission-mode") || hasArg(call.args, "--allowedTools") {
		t.Errorf("args = %v", call.args)
	}
}

func TestBugPipelineSessionRunsUnattended(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("Fixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatal(err)
	}
	assertUnattended(t, "debug", f.calls[0])
}

// Every session in the feature pipeline runs unattended — including the
// answerer and done-confirm sessions, which previously left AskUserQuestion
// enabled and so could stall a headless run.
func TestFeaturePipelineEverySessionRunsUnattended(t *testing.T) {
	wt := t.TempDir()
	var n int
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		n++
		switch n {
		case 1: // architect: asks a question
			return claudeJSON("What database should we use?", "arch-1"), "", nil
		case 2: // answerer
			return claudeJSON("Use SQLite.", "ans-1"), "", nil
		case 3: // architect resumed: claims already done
			return claudeJSON("PIPELINE_ALREADY_DONE: shipped last week", "arch-1"), "", nil
		case 4: // done-confirm answerer: objects, so the loop continues
			return claudeJSON("Not done: the export format is still missing.", "ans-2"), "", nil
		case 5: // architect resumed: commits the spec
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 6: // plan session
			writePlanFile(t, wt)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case 7: // execute session
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", n)
		return "", "", nil
	}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 7 {
		t.Fatalf("calls = %d, want 7", len(f.calls))
	}
	labels := []string{"brainstorm-0", "answer-1", "brainstorm-1", "done-confirm-2", "brainstorm-2", "plan", "execute"}
	for i, call := range f.calls {
		assertUnattended(t, labels[i], call)
	}
}

func TestReworkSessionRunsUnattended(t *testing.T) {
	env := newFakeEnv(t)
	seedRework(t, env, 7, "resume-me", "bug")
	if err := env.orchestrator().Rework(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range env.f.calls {
		if c.name == "claude" && argAfter(c.args, "--resume") == "resume-me" {
			found = true
			assertUnattended(t, "rework", c)
		}
	}
	if !found {
		t.Fatal("no resumed rework session recorded")
	}
}
