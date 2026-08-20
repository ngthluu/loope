package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

func TestBugPipelineSingleDebugSession(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("Fixed and committed.", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus", Effort: "high"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.Calls))
	}
	call := f.Calls[0]
	prompt := call.Stdin
	if !strings.Contains(prompt, "/superpowers:systematic-debugging") || !strings.Contains(prompt, "ISSUE") ||
		!strings.Contains(prompt, "failing test first") {
		t.Errorf("prompt = %s", prompt)
	}
	if call.Dir != "/wt" || !testkit.HasArg(call.Args, "--dangerously-skip-permissions") ||
		testkit.ArgAfter(call.Args, "--model") != "opus" {
		t.Errorf("call = %+v", call)
	}
}

func TestBugPipelinePropagatesError(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Err: fmt.Errorf("exit 1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "issue", "main", nil, nil); err == nil {
		t.Error("want error, got nil")
	}
}

func TestBugPipelineReturnsAlreadyDone(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		"I reproduced nothing; the guard already exists.\nPIPELINE_ALREADY_DONE: fixed in guard.go", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil)
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
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		return testkit.ClaudeJSON("Fixed and committed.", "debug-sess"), "", nil
	}}
	c := infra.NewClaude(f, logDir, "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "the issue", "main", nil, nil); err != nil {
		t.Fatal(err)
	}
	si, err := shared.ReadSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "debug-sess" || si.Kind != "bug" || si.Stage != shared.StageDebug {
		t.Errorf("session = %+v, want debug-sess/bug/debug", si)
	}
}

// TestBugPipelineRecordsSessionOnError reproduces the -rework gap: the debug
// call errored (e.g. a 429 session limit) but returned a valid session id. The
// pipeline must still persist that session so `loop -rework <N>` can resume it,
// while propagating the error so the issue gets parked.
func TestBugPipelineRecordsSessionOnError(t *testing.T) {
	logDir := t.TempDir()
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeErrorJSON("You've hit your session limit", "debug-429")}}}
	c := infra.NewClaude(f, logDir, "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "the issue", "main", nil, nil); err == nil {
		t.Fatal("want the error propagated so the issue is parked")
	}
	si, err := shared.ReadSession(logDir)
	if err != nil {
		t.Fatalf("session must be recorded even when the call errors, so -rework can resume: %v", err)
	}
	if si.SessionID != "debug-429" || si.Kind != "bug" || si.Stage != shared.StageDebug {
		t.Errorf("session = %+v, want debug-429/bug/debug", si)
	}
}

func TestBugPipelinePromptMentionsAlreadyDoneSentinel(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("Fixed and committed.", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Calls[0].Stdin, alreadyDoneSentinel) {
		t.Errorf("bug prompt should tell the architect how to signal already-done:\n%s", f.Calls[0].Stdin)
	}
}

func TestBugPipelineLowConfidenceEscalates(t *testing.T) {
	// A one-element queue, not a handler: a handler would answer every call with
	// the same low score, so the call-count assertion below could never catch a
	// pipeline that kept going.
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		"CONFIDENCE: 40\nNo stack trace and no repro steps.\nWhich command triggers the crash?", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "crashes sometimes on startup", "main", nil, nil)
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
	if len(f.Calls) != 1 {
		t.Errorf("low confidence must stop after the debug turn, got %d calls", len(f.Calls))
	}
}

func TestBugPipelineHighConfidenceProceeds(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("CONFIDENCE: 85\nFixed and committed.", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil); err != nil {
		t.Fatalf("a score at or above the threshold must proceed: %v", err)
	}
}

// A score exactly at the threshold is not below it, so it proceeds.
func TestBugPipelineConfidenceAtThresholdProceeds(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("CONFIDENCE: 70\nFixed and committed.", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil); err != nil {
		t.Fatalf("score == threshold must proceed: %v", err)
	}
}

// confidenceThreshold: 0 disables the gate entirely — even an explicit low score
// in the output is ignored.
func TestBugPipelineZeroThresholdIgnoresScore(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("CONFIDENCE: 5\nFixed and committed.", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil); err != nil {
		t.Fatalf("threshold 0 disables the gate: %v", err)
	}
}

// Fail open: a session that forgot the sentinel but fixed the bug still ships.
func TestBugPipelineMissingSentinelFailsOpen(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("Fixed and committed.", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil); err != nil {
		t.Fatalf("an absent score must fail open, got %v", err)
	}
}

// Confidence outranks already-done: a session too unsure to fix the bug must not
// be able to close the issue as already implemented either.
func TestBugPipelineLowConfidenceBeatsAlreadyDone(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		"CONFIDENCE: 20\nI cannot tell what behavior is wrong.\nPIPELINE_ALREADY_DONE: looks fine to me", "s1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil)
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

func TestBugPipelineRunsUATAfterDebug(t *testing.T) {
	var prompts []string
	f := &testkit.FakeRunner{}
	f.Handler = func(c testkit.RCall) (string, string, error) {
		prompts = append(prompts, c.Stdin)
		if len(prompts) == 1 {
			return testkit.ClaudeJSON("Fixed and committed.", "debug-1"), "", nil
		}
		return testkit.ClaudeJSON(uatBeginSentinel+"\n- [ ] reproduce the old crash and see it gone\n"+uatEndSentinel, "uat-1"), "", nil
	}
	tgt := &fakeUATTarget{body: "the issue body"}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}, UAT: shared.ModelConfig{Model: "sonnet"}}}
	wt := infra.NewWorktreeAt(&testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "1\n"}}}, "", testkit.TestRetry)
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", &UAT{Target: tgt, Num: 7}, wt); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("calls = %d, want debug then uat", len(prompts))
	}
	// The UAT prompt carries the issue content and names the base for the diff.
	if !strings.Contains(prompts[1], "ISSUE") || !strings.Contains(prompts[1], "origin/main") {
		t.Errorf("uat prompt = %s", prompts[1])
	}
	if testkit.ArgAfter(f.Calls[1].Args, "--model") != "sonnet" {
		t.Errorf("the uat call must use models.uat, got %v", f.Calls[1].Args)
	}
	if len(tgt.posted) != 1 {
		t.Errorf("posted %d UAT comments, want 1", len(tgt.posted))
	}
}

// The systematic-debugging route can end by asking a question instead of
// committing a fix or emitting a sentinel (violating the HEADLESS instruction,
// but real behavior seen on issues #70 and #83) — neither the confidence gate
// nor the already-done check catches that, so afterDebug must fall back to
// checking the worktree itself: zero commits ahead of base means nothing to
// ship, and the run escalates to needs-info with the session's questions as
// the feedback instead of parking as "produced no commits".
func TestBugPipelineEscalatesToNeedsInfoWhenNoCommitsProduced(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		"I found the root cause in parseCodeReview. Want me to proceed with a fix?", "s1")}}}
	tgt := &fakeUATTarget{body: "body"}
	wt := infra.NewWorktreeAt(&testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "0\n"}}}, "", testkit.TestRetry)
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", &UAT{Target: tgt, Num: 7}, wt)
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if lc.score != noConfidenceScore {
		t.Errorf("score = %d, want noConfidenceScore", lc.score)
	}
	if !strings.Contains(lc.feedback, "Want me to proceed with a fix?") {
		t.Errorf("feedback must carry the session's questions, got %q", lc.feedback)
	}
	if len(f.Calls) != 1 {
		t.Errorf("calls = %d, want only the debug turn (no UAT session)", len(f.Calls))
	}
	if tgt.bodyCalls != 0 || len(tgt.posted) != 0 {
		t.Error("the UAT step must not run when the debug step produced no commits")
	}
}

// A resumed debug session that keeps asking its needs-info questions without
// re-printing the CONFIDENCE sentinel (issue #83) must route back to
// needs-info, not fall through to ship's "produced no commits" park.
func TestResumeBugPipelineEscalatesToNeedsInfoWhenStalled(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		"I still can't responsibly pick a fix. Please answer the 5 questions above.", "s2")}}}
	wt := infra.NewWorktreeAt(&testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "0\n"}}}, "", testkit.TestRetry)
	c := infra.NewClaude(f, t.TempDir(), "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := ResumeBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, wt,
		shared.SessionNode{ID: "s1", Kind: "bug", Stage: shared.StageDebug}, "continue")
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if lc.score != noConfidenceScore {
		t.Errorf("score = %d, want noConfidenceScore", lc.score)
	}
}

// A low-confidence outcome escalates to ai-needs-info with no fix, so there is
// nothing to accept: the UAT step must not run.
func TestBugPipelineSkipsUATOnLowConfidence(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("CONFIDENCE: 40\nNo repro steps.", "s1")}}}
	tgt := &fakeUATTarget{body: "body"}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", &UAT{Target: tgt, Num: 7}, nil)
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if len(f.Calls) != 1 {
		t.Errorf("calls = %d, want only the debug turn", len(f.Calls))
	}
	if tgt.bodyCalls != 0 || len(tgt.posted) != 0 {
		t.Error("the UAT step must not run on the low-confidence outcome")
	}
}

// Already-done closes the issue without a fix — again nothing to accept.
func TestBugPipelineSkipsUATOnAlreadyDone(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("PIPELINE_ALREADY_DONE: the guard already exists", "s1")}}}
	tgt := &fakeUATTarget{body: "body"}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", &UAT{Target: tgt, Num: 7}, nil)
	var done *alreadyDoneError
	if !errors.As(err, &done) {
		t.Fatalf("want *alreadyDoneError, got %v", err)
	}
	if tgt.bodyCalls != 0 || len(tgt.posted) != 0 {
		t.Error("the UAT step must not run on the already-done outcome")
	}
}

// Non-blocking: a UAT session that errors still leaves the pipeline successful.
func TestBugPipelineReturnsNilWhenUATFails(t *testing.T) {
	var n int
	f := &testkit.FakeRunner{}
	f.Handler = func(c testkit.RCall) (string, string, error) {
		n++
		if n == 1 {
			return testkit.ClaudeJSON("Fixed and committed.", "debug-1"), "", nil
		}
		return "", "boom", fmt.Errorf("exit 1")
	}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	wt := infra.NewWorktreeAt(&testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "1\n"}}}, "", testkit.TestRetry)
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main",
		&UAT{Target: &fakeUATTarget{body: "body"}, Num: 7}, wt); err != nil {
		t.Fatalf("a failed UAT session must never fail the pipeline: %v", err)
	}
}

// TestResumeBugPipelineReentersWithResumeAndPrompt verifies the resumed call
// carries --resume <id> and the trigger prompt instead of bugPrompt, and still
// runs the confidence gate / already-done check / UAT on the resumed result.
func TestResumeBugPipelineReentersWithResumeAndPrompt(t *testing.T) {
	logDir := t.TempDir()
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		if testkit.ArgAfter(c.Args, "--resume") == "debug-sess" && c.Stdin == "continue" {
			return testkit.ClaudeJSON("Fixed and committed.", "debug-sess-2"), "", nil
		}
		return "", "unexpected call", fmt.Errorf("unexpected call: %+v", c)
	}}
	c := infra.NewClaude(f, logDir, "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	session := shared.SessionNode{ID: "debug-sess", Kind: "bug", Stage: shared.StageDebug}
	if err := ResumeBugPipeline(context.Background(), c, cfg, "/wt", "the issue", "main", nil, nil, session, "continue"); err != nil {
		t.Fatal(err)
	}
	si, err := shared.ReadSession(logDir)
	if err != nil || si.SessionID != "debug-sess-2" || si.Stage != shared.StageDebug {
		t.Errorf("session = %+v, err = %v, want debug-sess-2/debug", si, err)
	}
}

// TestResumeBugPipelineLowConfidenceEscalates verifies the confidence gate still
// runs against the resumed session's output.
func TestResumeBugPipelineLowConfidenceEscalates(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("CONFIDENCE: 20\nstill unclear", "debug-sess-2")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}, ConfidenceThreshold: 70}
	session := shared.SessionNode{ID: "debug-sess", Kind: "bug", Stage: shared.StageDebug}
	err := ResumeBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, nil, session, "continue")
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
}
