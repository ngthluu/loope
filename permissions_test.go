package main

import (
	"context"
	"strings"
	"testing"
)

// assertUnattended pins the baseline policy EVERY session must run under,
// triage included: auto permission mode (never the blanket
// --dangerously-skip-permissions bypass) and AskUserQuestion denied, so no
// headless session can waste its turns on a question nobody is there to answer.
// Call applies this itself, so a new session cannot opt out by omission.
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
}

// assertPreapproved pins the extra privilege the committing sessions get: the
// loop's own git/gh work runs without depending on the safety check's verdict.
func assertPreapproved(t *testing.T, label string, call rcall) {
	t.Helper()
	allowed := argAfter(call.args, "--allowedTools")
	for _, want := range pipelineAllowedTools {
		if !strings.Contains(allowed, want) {
			t.Errorf("%s: --allowedTools = %q, missing %q", label, allowed, want)
		}
	}
}

// assertNoPreapproval pins the other half: sessions that only read and judge
// (the PO proxy, triage) never commit, so they must not carry blanket
// pre-approval for every git and gh subcommand.
func assertNoPreapproval(t *testing.T, label string, call rcall) {
	t.Helper()
	if hasArg(call.args, "--allowedTools") {
		t.Errorf("%s: read-only session must not pre-approve git/gh: %v", label, call.args)
	}
}

// Triage is a session like any other: headless, and so subject to the baseline
// policy. It reads issue text and never commits, so it gets no pre-approval.
func TestTriageSessionRunsUnattended(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(`{"issueNumber":7,"kind":"bug","reason":"why"}`, "s1")}}}
	c := &Claude{runner: f}
	if _, err := Triage(context.Background(), c, ModelConfig{}, "/repo", []Issue{{Number: 7}}); err != nil {
		t.Fatal(err)
	}
	assertUnattended(t, "triage", f.calls[0])
	assertNoPreapproval(t, "triage", f.calls[0])
}

// The baseline must not be defeatable by a caller that sets DisallowedTools for
// its own reasons, and Call must not mutate the caller's slice to apply it.
func TestCallAppliesBaselineWithoutMutatingCaller(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	caller := []string{"WebSearch"}
	if _, err := c.Call(context.Background(), ClaudeCall{
		Label: "x", Prompt: "hi", DisallowedTools: caller,
	}); err != nil {
		t.Fatal(err)
	}
	got := argAfter(f.calls[0].args, "--disallowedTools")
	if !strings.Contains(got, "WebSearch") || !strings.Contains(got, "AskUserQuestion") {
		t.Errorf("--disallowedTools = %q, want both the caller's entry and the baseline", got)
	}
	if len(caller) != 1 || caller[0] != "WebSearch" {
		t.Errorf("Call mutated the caller's slice: %v", caller)
	}
}

// A call's AllowedTools must reach the CLI as given, and must not alias the
// package-level list — an append by one call would otherwise rewrite the policy
// for every session that follows it in the process.
func TestCallDoesNotAliasThePipelineAllowList(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}, {stdout: claudeJSON("ok", "s2")}}}
	c := &Claude{runner: f}
	call := commits(ClaudeCall{Label: "execute", Prompt: "hi"})
	call.AllowedTools = append(call.AllowedTools, "Bash(rm *)")
	if _, err := c.Call(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pipelineAllowedTools, ","); strings.Contains(got, "rm") {
		t.Fatalf("appending to one call's AllowedTools rewrote the shared policy: %v", pipelineAllowedTools)
	}
	if _, err := c.Call(context.Background(), commits(ClaudeCall{Label: "plan", Prompt: "hi"})); err != nil {
		t.Fatal(err)
	}
	if got := argAfter(f.calls[1].args, "--allowedTools"); strings.Contains(got, "rm") {
		t.Errorf("later session inherited the earlier call's grant: %q", got)
	}
}

func TestCallEmitsPermissionModeAndAllowedTools(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	_, err := c.Call(context.Background(), ClaudeCall{
		Dir: "/wt", Label: "debug", Prompt: "hi",
		AllowedTools: []string{"Bash(git *)", "Bash(gh *)"},
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

// A call that asks for no pre-approval must not grow an empty --allowedTools
// argument — but it still runs under the baseline, which is not opt-in.
func TestCallOmitsAllowedToolsWhenUnset(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f}
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "answer-1", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	call := f.calls[0]
	if hasArg(call.args, "--allowedTools") {
		t.Errorf("args = %v", call.args)
	}
	assertUnattended(t, "answer-1", call)
}

func TestBugPipelineSessionRunsUnattended(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("Fixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatal(err)
	}
	assertUnattended(t, "debug", f.calls[0])
	assertPreapproved(t, "debug", f.calls[0])
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
	// The architect, plan and execute sessions commit; the PO-proxy sessions
	// only read and judge, so they must not get blanket git/gh pre-approval.
	readOnly := map[string]bool{"answer-1": true, "done-confirm-2": true}
	for i, call := range f.calls {
		assertUnattended(t, labels[i], call)
		if readOnly[labels[i]] {
			assertNoPreapproval(t, labels[i], call)
		} else {
			assertPreapproved(t, labels[i], call)
		}
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
			assertPreapproved(t, "rework", c)
		}
	}
	if !found {
		t.Fatal("no resumed rework session recorded")
	}
}
