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

// The fix route: one entry session commits the fix and reports fix_committed —
// the whole pipeline is that single call.
func TestRunPipelineSingleFixSession(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntry("s1", "fix_committed", "fixed the crash")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus", Effort: "high"}}}
	if err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(f.Calls))
	}
	call := f.Calls[0]
	prompt := call.Stdin
	if !strings.HasPrefix(prompt, "Handle this GitHub issue:") || !strings.Contains(prompt, "ISSUE") ||
		!strings.Contains(prompt, "failing test first") {
		t.Errorf("prompt = %s", prompt)
	}
	if call.Dir != "/wt" || !testkit.HasArg(call.Args, "--dangerously-skip-permissions") ||
		testkit.ArgAfter(call.Args, "--model") != "opus" {
		t.Errorf("call = %+v", call)
	}
}

func TestRunPipelinePropagatesError(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Err: fmt.Errorf("exit 1")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunPipeline(context.Background(), c, cfg, "/wt", "issue", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1); err == nil {
		t.Error("want error, got nil")
	}
}

// An already-done claim from the entry session goes through the PO-proxy
// confirmation before closing — the route-agnostic tightening of the old
// bug route's self-trusted claim.
func TestRunPipelineAlreadyDoneConfirmed(t *testing.T) {
	var prompts []string
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		prompts = append(prompts, c.Stdin)
		switch len(prompts) {
		case 1:
			return testkit.ClaudeEntry("s1", "already_done", "fixed in guard.go"), "", nil
		case 2: // answerer confirmation
			return testkit.ClaudeDoneConfirm("ans-1", ""), "", nil
		}
		t.Fatalf("unexpected call %d", len(prompts))
		return "", "", nil
	}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{MaxQARounds: 3, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}, Answerer: shared.ModelConfig{Model: "sonnet"}}}
	err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1)
	var done *alreadyDoneError
	if !errors.As(err, &done) {
		t.Fatalf("want *alreadyDoneError, got %v", err)
	}
	if done.reason != "fixed in guard.go" {
		t.Errorf("reason = %q", done.reason)
	}
}

// The fix outcome stamps kind "bug" onto the chain head; the entry call itself
// checkpoints with an empty kind (unknown at call time).
func TestRunPipelineFixOutcomeStampsBugKind(t *testing.T) {
	logDir := t.TempDir()
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		return testkit.ClaudeEntry("entry-sess", "fix_committed", "done"), "", nil
	}}
	c := infra.NewClaude(f, logDir, "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunPipeline(context.Background(), c, cfg, "/wt", "the issue", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1); err != nil {
		t.Fatal(err)
	}
	si, err := shared.ReadSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "entry-sess" || si.Kind != "bug" || si.Stage != shared.StageEntry {
		t.Errorf("session = %+v, want entry-sess/bug/entry", si)
	}
	if got := shared.ResolvedKind(logDir); got != "bug" {
		t.Errorf("ResolvedKind = %q, want bug", got)
	}
}

// The spec outcome stamps kind "feature" onto the entry node before the plan
// stage checkpoint lands.
func TestRunPipelineSpecOutcomeStampsFeatureKind(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		switch {
		case strings.HasPrefix(c.Stdin, "Handle this GitHub issue:"):
			writeSpecFile(t, wt)
			return testkit.ClaudeSpecReady("entry-sess", "docs/superpowers/specs/2026-07-13-thing-design.md"), "", nil
		case strings.Contains(c.Stdin, "writing-plans"):
			writePlanFile(t, wt)
			return testkit.ClaudePlanReady("plan-sess"), "", nil
		default:
			return testkit.ClaudeJSON("executed", "exec-sess"), "", nil
		}
	}}
	c := infra.NewClaude(f, logDir, "")
	if err := RunPipeline(context.Background(), c, featureConfig(), wt, "the issue", "", "main", nil, testGH(), testWT(), "ai/issue-1", "T", 1); err != nil {
		t.Fatal(err)
	}
	chain := shared.ReadSessionChain(logDir)
	if len(chain) == 0 || chain[0].ID != "entry-sess" || chain[0].Stage != shared.StageEntry || chain[0].Kind != "feature" {
		t.Errorf("chain[0] = %+v, want the entry node stamped kind feature", chain[0])
	}
	if got := shared.ResolvedKind(logDir); got != "feature" {
		t.Errorf("ResolvedKind = %q, want feature", got)
	}
}

// TestRunPipelineRecordsSessionOnError reproduces the -rework gap: the entry
// call errored (e.g. a 429 session limit) but returned a valid session id. The
// pipeline must still persist that session so a re-entry can resume it, while
// propagating the error so the issue gets parked.
func TestRunPipelineRecordsSessionOnError(t *testing.T) {
	logDir := t.TempDir()
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeErrorJSON("You've hit your session limit", "entry-429")}}}
	c := infra.NewClaude(f, logDir, "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunPipeline(context.Background(), c, cfg, "/wt", "the issue", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1); err == nil {
		t.Fatal("want the error propagated so the issue is parked")
	}
	si, err := shared.ReadSession(logDir)
	if err != nil {
		t.Fatalf("session must be recorded even when the call errors, so a re-entry can resume: %v", err)
	}
	if si.SessionID != "entry-429" || si.Stage != shared.StageEntry {
		t.Errorf("session = %+v, want entry-429/entry", si)
	}
}

func TestRunPipelineLowConfidenceEscalates(t *testing.T) {
	// A one-element queue, not a handler: a handler would answer every call with
	// the same low score, so the call-count assertion below could never catch a
	// pipeline that kept going.
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntryConfidence(
		"s1", "question", "No stack trace and no repro steps.\nWhich command triggers the crash?", 40)}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunPipeline(context.Background(), c, cfg, "/wt", "crashes sometimes on startup", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1)
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if lc.score != 40 {
		t.Errorf("score = %d, want 40", lc.score)
	}
	if !strings.Contains(lc.feedback, "repro steps") {
		t.Errorf("feedback should carry the session's reasons: %q", lc.feedback)
	}
	if len(f.Calls) != 1 {
		t.Errorf("low confidence must stop after the entry turn, got %d calls", len(f.Calls))
	}
}

func TestRunPipelineHighConfidenceProceeds(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntryConfidence("s1", "fix_committed", "done", 85)}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1); err != nil {
		t.Fatalf("a score at or above the threshold must proceed: %v", err)
	}
}

// A score exactly at the threshold is not below it, so it proceeds.
func TestRunPipelineConfidenceAtThresholdProceeds(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntryConfidence("s1", "fix_committed", "done", 70)}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1); err != nil {
		t.Fatalf("score == threshold must proceed: %v", err)
	}
}

// confidenceThreshold: 0 disables the gate entirely — even an explicit low score
// in the output is ignored.
func TestRunPipelineZeroThresholdIgnoresScore(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntryConfidence("s1", "fix_committed", "done", 5)}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	if err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1); err != nil {
		t.Fatalf("threshold 0 disables the gate: %v", err)
	}
}

// Confidence outranks already-done: a session too unsure must not be able to
// close the issue as already implemented either.
func TestRunPipelineLowConfidenceBeatsAlreadyDone(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntryConfidence(
		"s1", "already_done", "I cannot tell what behavior is wrong; looks fine to me", 20)}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1)
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %T (%v)", err, err)
	}
	var done *alreadyDoneError
	if errors.As(err, &done) {
		t.Error("a low-confidence session must not close the issue as already done")
	}
	// The feedback is posted verbatim as a public GitHub comment. It carries the
	// session's own detail text — the outcome itself lives in a separate
	// structured field, so an ignored already-done claim can no longer leak
	// into the comment as a stray sentinel line.
	if !strings.Contains(lc.feedback, "cannot tell what behavior is wrong") {
		t.Errorf("needs-info feedback must carry the session's detail: %q", lc.feedback)
	}
}

func TestRunPipelineRunsUATAfterFix(t *testing.T) {
	var prompts []string
	f := &testkit.FakeRunner{}
	f.Handler = func(c testkit.RCall) (string, string, error) {
		prompts = append(prompts, c.Stdin)
		if len(prompts) == 1 {
			return testkit.ClaudeEntry("entry-1", "fix_committed", "fixed"), "", nil
		}
		return testkit.ClaudeStructured("uat-1", map[string]any{"checklist": "- [ ] reproduce the old crash and see it gone"}), "", nil
	}
	tgt := &fakeUATTarget{body: "the issue body"}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}, UAT: shared.ModelConfig{Model: "sonnet"}}}
	wt := infra.NewWorktreeAt(&testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "1\n"}}}, "", testkit.TestRetry)
	if err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", &UAT{Target: tgt, Num: 7}, testGH(), wt, "ai/issue-7", "T", 7); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("calls = %d, want entry then uat", len(prompts))
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

// A fix_committed outcome with zero commits ahead of base is not a fix: the run
// escalates to needs-info with the session's output as the public comment,
// instead of reaching ship's "produced no commits" park (issues #70/#83).
func TestRunPipelineFixClaimWithNoCommitsEscalatesToNeedsInfo(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntry("s1", "fix_committed",
		"I found the root cause in parseCodeReview. Want me to proceed with a fix?")}}}
	tgt := &fakeUATTarget{body: "body"}
	wt := infra.NewWorktreeAt(&testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "0\n"}}}, "", testkit.TestRetry)
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", &UAT{Target: tgt, Num: 7}, testGH(), wt, "ai/issue-7", "T", 7)
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
	if tgt.bodyCalls != 0 || len(tgt.posted) != 0 {
		t.Error("the UAT step must not run when the entry step produced no commits")
	}
}

// A session that merely MENTIONS a terminal outcome in its prose can no longer
// terminate the loop: the outcome is a schema-enforced enum field, so prose is
// only ever carried in detail (the class of false positive behind issues
// #73/#76, now unrepresentable).
func TestEntryOutcomeComesFromStructuredFieldNotProse(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntry("s1", "question",
		"I will report fix_committed once done.")}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{MaxQARounds: 0, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	err := RunPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, testGH(), nil, "ai/issue-1", "T", 1)
	if err == nil || !strings.Contains(err.Error(), "Q&A rounds") {
		t.Fatalf("prose naming an outcome must not terminate the loop, got %v", err)
	}
}

// TestResumePipelineEntryStage verifies a StageEntry chain node resumes its own
// session with --resume and the restated-contract wrapper around the trigger,
// and the gates still run against the resumed output.
func TestResumePipelineEntryStage(t *testing.T) {
	logDir := t.TempDir()
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		if testkit.ArgAfter(c.Args, "--resume") == "entry-sess" && strings.HasPrefix(c.Stdin, "continue") {
			return testkit.ClaudeEntry("entry-sess-2", "fix_committed", "done"), "", nil
		}
		return "", "unexpected call", fmt.Errorf("unexpected call: %+v", c)
	}}
	c := infra.NewClaude(f, logDir, "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	node := shared.SessionNode{ID: "entry-sess", Stage: shared.StageEntry}
	if err := ResumePipeline(context.Background(), c, cfg, "/wt", "the issue", "", "main", nil, node, "continue", testGH(), nil, "ai/issue-1", "T", 1); err != nil {
		t.Fatal(err)
	}
	// The resumed prompt restates all three terminal outcomes.
	prompt := f.Calls[0].Stdin
	for _, outcome := range []string{entryOutcomeFix, entryOutcomeSpec, entryOutcomeDone} {
		if !strings.Contains(prompt, outcome) {
			t.Errorf("resumed prompt must restate %s, got %q", outcome, prompt)
		}
	}
	// And the schema is enforced on the resumed turn, so the contract holds even
	// if the prose is ignored (the issue-5 incident).
	if !strings.Contains(testkit.ArgAfter(f.Calls[0].Args, "--json-schema"), "fix_committed") {
		t.Error("resumed entry turn must carry the entry outcome schema")
	}
	if got := shared.ResolvedKind(logDir); got != "bug" {
		t.Errorf("ResolvedKind = %q, want bug stamped on the resumed fix outcome", got)
	}
}

// TestResumePipelineEntryStageLowConfidenceEscalates: the confidence gate runs
// against a resumed entry turn too.
func TestResumePipelineEntryStageLowConfidenceEscalates(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntryConfidence("entry-sess-2", "question", "still unclear", 20)}}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}, ConfidenceThreshold: 70}
	node := shared.SessionNode{ID: "entry-sess", Stage: shared.StageEntry}
	err := ResumePipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, node, "continue", testGH(), nil, "ai/issue-1", "T", 1)
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
}

// A pending entry node (no session id) has nothing to resume: fall back to a
// fully fresh pipeline.
func TestResumePipelinePendingEntryNodeRunsFresh(t *testing.T) {
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		if testkit.ArgAfter(c.Args, "--resume") != "" {
			t.Errorf("a pending entry node must not resume anything, got %+v", c)
		}
		return testkit.ClaudeEntry("fresh-entry", "fix_committed", "done"), "", nil
	}}
	c := infra.NewClaude(f, "", "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	node := shared.SessionNode{Stage: shared.StageEntry}
	if err := ResumePipeline(context.Background(), c, cfg, "/wt", "the issue", "", "main", nil, node, "continue", testGH(), nil, "ai/issue-1", "T", 1); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.Calls[0].Stdin, "Handle this GitHub issue:") {
		t.Errorf("want a fresh entry-0 call, got prompt %q", f.Calls[0].Stdin)
	}
}

// LEGACY: a chain checkpointed by the old bug pipeline (kind "bug", stage
// "debug") resumes its debug session with the BARE trigger prompt — the old
// contract — and still runs afterFix's gates.
func TestResumePipelineLegacyBugChainResumesDebugSession(t *testing.T) {
	logDir := t.TempDir()
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		if testkit.ArgAfter(c.Args, "--resume") == "debug-sess" && c.Stdin == "continue" {
			return testkit.ClaudeEntry("debug-sess-2", "fix_committed", "Fixed and committed."), "", nil
		}
		return "", "unexpected call", fmt.Errorf("unexpected call: %+v", c)
	}}
	c := infra.NewClaude(f, logDir, "")
	cfg := &shared.Config{Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	node := shared.SessionNode{ID: "debug-sess", Kind: "bug", Stage: shared.StageDebug}
	if err := ResumePipeline(context.Background(), c, cfg, "/wt", "the issue", "", "main", nil, node, "continue", testGH(), nil, "ai/issue-1", "T", 1); err != nil {
		t.Fatal(err)
	}
	si, err := shared.ReadSession(logDir)
	if err != nil || si.SessionID != "debug-sess-2" || si.Kind != "bug" || si.Stage != shared.StageDebug {
		t.Errorf("session = %+v, err = %v, want debug-sess-2/bug/debug", si, err)
	}
}

// LEGACY: a resumed debug session that keeps asking its needs-info questions
// without committing (issue #83) must route back to needs-info, not fall
// through to ship's "produced no commits" park.
func TestResumePipelineLegacyBugChainEscalatesWhenStalled(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeEntry("s2", "question",
		"I still can't responsibly pick a fix. Please answer the 5 questions above.")}}}
	wt := infra.NewWorktreeAt(&testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "0\n"}}}, "", testkit.TestRetry)
	c := infra.NewClaude(f, t.TempDir(), "")
	cfg := &shared.Config{ConfidenceThreshold: 70, Models: shared.Models{Architect: shared.ModelConfig{Model: "opus"}}}
	node := shared.SessionNode{ID: "s1", Kind: "bug", Stage: shared.StageDebug}
	err := ResumePipeline(context.Background(), c, cfg, "/wt", "ISSUE", "", "main", nil, node, "continue", testGH(), wt, "ai/issue-1", "T", 1)
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if lc.score != noConfidenceScore {
		t.Errorf("score = %d, want noConfidenceScore", lc.score)
	}
}
