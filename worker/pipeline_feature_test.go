package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func featureConfig() *Config {
	return &Config{
		MaxQARounds: 3,
		Models: Models{
			Architect: ModelConfig{Model: "opus", Effort: "high"},
			Answerer:  ModelConfig{Model: "sonnet", Effort: "medium"},
		},
	}
}

func writePlanFile(t *testing.T, wt string) string {
	t.Helper()
	dir := filepath.Join(wt, "docs", "superpowers", "plans")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "2026-07-06-thing.md")
	if err := os.WriteFile(p, []byte("# Plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// testGH/testWT are no-op GitHub/Worktree doubles for the push/PR/comment
// steps the feature pipeline now runs mid-flight. They're backed by their OWN
// fakeRunner, deliberately separate from whatever runner a test's *Claude
// uses — so a push/PR/comment call never lands in a test's claude call count
// or prompt list.
func testGH() *GitHub {
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if strings.Contains(strings.Join(c.args, " "), "pr create") {
			return "https://github.com/org/repo/pull/1\n", "", nil
		}
		return "", "", nil
	}}
	return NewGitHub(f, &Config{RepoSlug: "org/repo"})
}

func testWT() *Worktree {
	return &Worktree{runner: &fakeRunner{}}
}

func TestParseSpecReady(t *testing.T) {
	if p, ok := parseSpecReady("Spec done.\nSPEC_READY: docs/superpowers/specs/x-design.md\n"); !ok || p != "docs/superpowers/specs/x-design.md" {
		t.Errorf("parseSpecReady = %q,%v", p, ok)
	}
	if _, ok := parseSpecReady("no sentinel"); ok {
		t.Error("want ok=false when sentinel absent")
	}
}

func writeSpecFile(t *testing.T, wt string) string {
	t.Helper()
	dir := filepath.Join(wt, "docs", "superpowers", "specs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "2026-07-13-thing-design.md")
	if err := os.WriteFile(p, []byte("# Spec"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFindSpecFile(t *testing.T) {
	wt := t.TempDir()
	since := time.Now().Add(-time.Second)
	if _, ok := findSpecFile(wt, since); ok {
		t.Error("empty worktree should have no spec")
	}
	p := writeSpecFile(t, wt)
	got, ok := findSpecFile(wt, since)
	if !ok || got != p {
		t.Errorf("findSpecFile = %q,%v; want %q", got, ok, p)
	}
}

func TestResolveSpec(t *testing.T) {
	wt := t.TempDir()
	since := time.Now().Add(-time.Second)
	p := writeSpecFile(t, wt)
	// Explicit relative path resolves against the worktree.
	if got, ok := resolveSpec(wt, "docs/superpowers/specs/2026-07-13-thing-design.md", since); !ok || got != p {
		t.Errorf("resolveSpec(rel) = %q,%v; want %q", got, ok, p)
	}
	// Bogus path falls back to the specs-dir search.
	if got, ok := resolveSpec(wt, "nope.md", since); !ok || got != p {
		t.Errorf("resolveSpec(fallback) = %q,%v; want %q", got, ok, p)
	}
	// No spec anywhere: not found.
	if _, ok := resolveSpec(t.TempDir(), "", since); ok {
		t.Error("want ok=false when no spec exists")
	}
}

func TestFindPlanFile(t *testing.T) {
	wt := t.TempDir()
	since := time.Now().Add(-time.Second)
	if _, ok := findPlanFile(wt, since); ok {
		t.Error("empty worktree should have no plan")
	}
	p := writePlanFile(t, wt)
	got, ok := findPlanFile(wt, since)
	if !ok || got != p {
		t.Errorf("findPlanFile = %q, %v; want %q", got, ok, p)
	}
	// A file modified before `since` must not count.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := findPlanFile(wt, since); ok {
		t.Error("stale plan file should not count")
	}
}

func TestFeaturePipelineQALoopThenExecute(t *testing.T) {
	wt := t.TempDir()
	var prompts []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		prompt := c.stdin
		prompts = append(prompts, prompt)
		switch len(prompts) {
		case 1: // architect opening: asks a question
			return claudeJSON("What database should we use?", "arch-1"), "", nil
		case 2: // answerer
			return claudeJSON("Use SQLite.", "ans-1"), "", nil
		case 3: // architect resumed: commits the spec
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 4: // fresh plan session: commits the plan
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 5: // executor
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d: %v", len(prompts), c.args)
		return "", "", nil
	}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE CONTENT", "PERSONA", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 5 {
		t.Fatalf("got %d calls, want 5", len(prompts))
	}
	if !strings.Contains(prompts[0], "/superpowers:brainstorming") || !strings.Contains(prompts[0], "ISSUE CONTENT") ||
		!strings.Contains(prompts[0], specReadySentinel) {
		t.Errorf("brainstorm prompt = %s", prompts[0])
	}
	if !hasArg(f.calls[0].args, "--disallowedTools") {
		t.Error("architect must disable AskUserQuestion")
	}
	if !strings.Contains(prompts[1], "What database") || !strings.Contains(prompts[1], "PERSONA") {
		t.Errorf("answerer prompt = %s", prompts[1])
	}
	if got := argAfter(f.calls[2].args, "--resume"); got != "arch-1" {
		t.Errorf("resume session = %q", got)
	}
	if prompts[2] != "Use SQLite." {
		t.Errorf("resumed prompt = %q", prompts[2])
	}
	// Plan session: fresh, carries the spec path and the writing-plans skill.
	if !strings.Contains(prompts[3], "/superpowers:writing-plans") || !strings.Contains(prompts[3], "2026-07-13-thing-design.md") {
		t.Errorf("plan prompt = %s", prompts[3])
	}
	if got := argAfter(f.calls[3].args, "--resume"); got != "" {
		t.Error("plan session must be fresh, not a resume of the architect")
	}
	// Executor: fresh, carries the plan path.
	if !strings.Contains(prompts[4], "/superpowers:executing-plans") || !strings.Contains(prompts[4], "2026-07-06-thing.md") {
		t.Errorf("execute prompt = %s", prompts[4])
	}
	if got := argAfter(f.calls[4].args, "--resume"); got != "" {
		t.Error("executor must be a fresh session, not a resume")
	}
}

// TestBrainstormLoopPushesAndCreatesPRAfterSpec locks in spec §1's ordering:
// the spec-stage push/PR/comment must complete BEFORE the plan session ever
// starts, using ship's own idempotent CreatePR/Push (so a later ship at the
// end of the run recovers the same PR instead of erroring).
func TestBrainstormLoopPushesAndCreatesPRAfterSpec(t *testing.T) {
	wt := t.TempDir()
	logDir := t.TempDir()

	var ghCalls []string
	gf := &fakeRunner{}
	// NOTE: fakeRunner invokes the handler while already holding its own mutex,
	// so the handler must never re-lock gf.mu. The append is already serialized.
	gf.handler = func(c rcall) (string, string, error) {
		ghCalls = append(ghCalls, c.name+" "+strings.Join(c.args, " "))
		if c.name == "gh" && strings.Contains(strings.Join(c.args, " "), "pr create") {
			return "https://github.com/org/repo/pull/42\n", "", nil
		}
		return "", "", nil
	}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	wtree := &Worktree{runner: gf}

	var prompts []string
	cf := &fakeRunner{}
	cf.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1: // architect: commits the spec straight away
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 2: // fresh plan session: the spec-stage push/PR must already have run
			if len(ghCalls) == 0 {
				t.Fatal("plan session started before the spec-stage push/PR ran")
			}
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 3: // executor
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d: %v", len(prompts), c.args)
		return "", "", nil
	}
	c := &Claude{runner: cf, logDir: logDir}

	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE CONTENT", "PERSONA", nil,
		gh, wtree, "ai/issue-9", "Add export", 9); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 3 {
		t.Fatalf("got %d claude calls, want 3", len(prompts))
	}
	if len(ghCalls) < 3 {
		t.Fatalf("want at least push+create+comment on the gh/git runner, got %v", ghCalls)
	}
	if !strings.HasPrefix(ghCalls[0], "git push") {
		t.Errorf("first git/gh call should be the push, got %v", ghCalls)
	}
	var sawCreate, sawComment bool
	for _, call := range ghCalls {
		if strings.Contains(call, "pr create") {
			sawCreate = true
		}
		if strings.Contains(call, "issue comment") && strings.Contains(call, "pull/42") {
			sawComment = true
		}
	}
	if !sawCreate {
		t.Errorf("want a pr create call, got %v", ghCalls)
	}
	if !sawComment {
		t.Errorf("want the PR URL commented, got %v", ghCalls)
	}
	b, err := os.ReadFile(filepath.Join(logDir, "pr"))
	if err != nil || string(b) != "https://github.com/org/repo/pull/42" {
		t.Errorf("recordPR = %q, err=%v", b, err)
	}
}

// TestBrainstormLoopContinuesWhenSpecPushFails is decision 5: a push/PR/
// comment failure at the spec stage must not abort the pipeline — plan and
// execute still run.
func TestBrainstormLoopContinuesWhenSpecPushFails(t *testing.T) {
	wt := t.TempDir()
	gf := &fakeRunner{}
	gf.handler = func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "push") {
			return "", "connection refused", errors.New("git push: connection refused")
		}
		return "", "", nil
	}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	gh.retry = testRetry
	wtree := &Worktree{runner: gf, retry: testRetry}

	var prompts []string
	cf := &fakeRunner{}
	cf.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1:
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 2:
			writePlanFile(t, wt)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case 3:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", len(prompts))
		return "", "", nil
	}
	c := &Claude{runner: cf}

	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE CONTENT", "PERSONA", nil,
		gh, wtree, "ai/issue-9", "Add export", 9); err != nil {
		t.Fatalf("a failed spec-stage push must not fail the pipeline, got %v", err)
	}
	if len(prompts) != 3 {
		t.Fatalf("pipeline must still run plan+execute despite the push failure, got %d claude calls", len(prompts))
	}
}

func TestFeaturePipelineFailsAfterMaxRounds(t *testing.T) {
	wt := t.TempDir()
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		return claudeJSON("Still thinking...", "s1"), "", nil
	}
	c := &Claude{runner: f}
	err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "issue", "", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1)
	if err == nil || !strings.Contains(err.Error(), "rounds") {
		t.Errorf("want max-rounds error, got %v", err)
	}
}

// TestFeaturePipelineSucceedsWhenSpecCompletesOnFinalRound locks in the
// boundary behavior: when the architect finishes the spec (prints
// specReadySentinel) on the LAST permitted Q&A round, the pipeline must still
// detect it and run the plan + execute sessions rather than reporting
// "exceeded rounds". With MaxQARounds=1, round 1 is the only permitted round;
// the architect's resumed call made during round 1 ("brainstorm-1") is that
// final round's output, and it is only inspected for the sentinel at the top
// of the next loop iteration — which must happen before the bound check fires.
func TestFeaturePipelineSucceedsWhenSpecCompletesOnFinalRound(t *testing.T) {
	wt := t.TempDir()
	cfg := &Config{
		MaxQARounds: 1,
		Models: Models{
			Architect: ModelConfig{Model: "opus", Effort: "high"},
			Answerer:  ModelConfig{Model: "sonnet", Effort: "medium"},
		},
	}
	var prompts []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1:
			return claudeJSON("What database should we use?", "arch-1"), "", nil
		case 2:
			return claudeJSON("Use SQLite.", "ans-1"), "", nil
		case 3: // architect resumed on the LAST permitted round: commits the spec
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 4:
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 5:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d: %v", len(prompts), c.args)
		return "", "", nil
	}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "ISSUE CONTENT", "PERSONA", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatalf("pipeline should succeed when the spec completes on the last permitted round, got %v", err)
	}
	if len(prompts) != 5 {
		t.Fatalf("got %d calls, want 5", len(prompts))
	}
}

// TestFeaturePipelineAlreadyDoneConfirmedOnFinalRound locks in the symmetric
// boundary behavior for the already-done path: when the architect's output
// on the LAST permitted Q&A round is an already-done claim
// (PIPELINE_ALREADY_DONE: ...), the pipeline must still route it through the
// answerer confirmation and return *alreadyDoneError, rather than reporting
// "exceeded rounds". With MaxQARounds=1, round 1 is the only permitted
// round; the architect's resumed call made during round 1 ("brainstorm-1")
// produces that final round's output, and it is only inspected for the
// already-done sentinel at the top of the next loop iteration — which must
// happen before the bound check fires.
func TestFeaturePipelineAlreadyDoneConfirmedOnFinalRound(t *testing.T) {
	wt := t.TempDir()
	cfg := &Config{
		MaxQARounds: 1,
		Models: Models{
			Architect: ModelConfig{Model: "opus", Effort: "high"},
			Answerer:  ModelConfig{Model: "sonnet", Effort: "medium"},
		},
	}
	var prompts []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1: // architect opening: asks a question
			return claudeJSON("What database should we use?", "arch-1"), "", nil
		case 2: // answerer
			return claudeJSON("Use SQLite.", "ans-1"), "", nil
		case 3: // architect resumed on the LAST permitted round: claims already done
			return claudeJSON("Looked around.\nPIPELINE_ALREADY_DONE: dashboard already exists", "arch-1"), "", nil
		case 4: // answerer confirmation
			return claudeJSON("Agreed, nothing to build. DONE_CONFIRMED", "ans-1"), "", nil
		}
		t.Fatalf("unexpected call %d: %v", len(prompts), c.args)
		return "", "", nil
	}
	c := &Claude{runner: f}
	err := RunFeaturePipeline(context.Background(), c, cfg, wt, "ISSUE CONTENT", "PERSONA", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1)
	var done *alreadyDoneError
	if !errors.As(err, &done) {
		t.Fatalf("want *alreadyDoneError when already-done claim arrives on the final permitted round, got %v", err)
	}
	if done.reason != "dashboard already exists" {
		t.Errorf("reason = %q", done.reason)
	}
	if len(prompts) != 4 {
		t.Fatalf("got %d calls, want 4", len(prompts))
	}
}

func TestFeaturePipelineSpecSentinelWithoutFileKeepsGoing(t *testing.T) {
	wt := t.TempDir()
	count := 0
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		count++
		return claudeJSON("SPEC_READY: nope.md", "s1"), "", nil // lies: no spec file exists
	}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "issue", "", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err == nil {
		t.Error("want error when spec sentinel appears but no spec file ever exists")
	}
	if count < 3 {
		t.Errorf("pipeline gave up after %d calls; it should keep prodding until max rounds", count)
	}
}

func TestFeaturePipelineArchitectDoneConfirmed(t *testing.T) {
	wt := t.TempDir()
	var prompts []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1: // architect opening claims already implemented
			return claudeJSON("Looked around.\nPIPELINE_ALREADY_DONE: dashboard already exists", "arch-1"), "", nil
		case 2: // answerer confirmation
			return claudeJSON("Agreed, nothing to build. DONE_CONFIRMED", "ans-1"), "", nil
		}
		t.Fatalf("unexpected call %d", len(prompts))
		return "", "", nil
	}
	c := &Claude{runner: f}
	err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1)
	var done *alreadyDoneError
	if !errors.As(err, &done) {
		t.Fatalf("want *alreadyDoneError, got %v", err)
	}
	if done.reason != "dashboard already exists" {
		t.Errorf("reason = %q", done.reason)
	}
	if len(prompts) != 2 {
		t.Fatalf("want 2 calls (architect + confirm), got %d", len(prompts))
	}
	if !strings.Contains(prompts[1], "dashboard already exists") || !strings.Contains(prompts[1], doneConfirmSentinel) {
		t.Errorf("confirmation prompt should carry the reason and the confirm sentinel: %s", prompts[1])
	}
}

func TestFeaturePipelineArchitectDonePushbackContinues(t *testing.T) {
	wt := t.TempDir()
	var prompts []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1: // architect claims done
			return claudeJSON("PIPELINE_ALREADY_DONE: I think it exists", "arch-1"), "", nil
		case 2: // answerer disagrees (no DONE_CONFIRMED)
			return claudeJSON("No — the CSV export is missing. Please design it.", "ans-1"), "", nil
		case 3: // architect resumed with pushback, commits the spec
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 4: // fresh plan session
			writePlanFile(t, wt)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case 5: // executor
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", len(prompts))
		return "", "", nil
	}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 5 {
		t.Fatalf("want 5 calls, got %d", len(prompts))
	}
	if got := argAfter(f.calls[2].args, "--resume"); got != "arch-1" {
		t.Errorf("architect should be resumed with the pushback, resume=%q", got)
	}
	if prompts[2] != "No — the CSV export is missing. Please design it." {
		t.Errorf("architect should receive the answerer pushback verbatim, got %q", prompts[2])
	}
}

func TestFeaturePipelineLowConfidenceEscalates(t *testing.T) {
	wt := t.TempDir()
	cfg := &Config{
		MaxQARounds:         3,
		ConfidenceThreshold: 70,
		Models: Models{
			Architect: ModelConfig{Model: "opus"},
			Answerer:  ModelConfig{Model: "sonnet"},
		},
	}
	count := 0
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		count++
		return claudeJSON("CONFIDENCE: 40\nThe issue has no acceptance criteria.\nWhat output format is expected?", "arch-1"), "", nil
	}}
	c := &Claude{runner: f}
	err := RunFeaturePipeline(context.Background(), c, cfg, wt, "vague issue", "", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1)
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if lc.score != 40 {
		t.Errorf("score = %d, want 40", lc.score)
	}
	if !strings.Contains(lc.feedback, "acceptance criteria") || strings.Contains(lc.feedback, confidenceSentinel) {
		t.Errorf("feedback should carry the reasons without the CONFIDENCE line: %q", lc.feedback)
	}
	if count != 1 {
		t.Errorf("low confidence must stop after the first turn, got %d calls", count)
	}
}

func TestFeaturePipelineHighConfidenceProceeds(t *testing.T) {
	wt := t.TempDir()
	cfg := &Config{
		MaxQARounds:         3,
		ConfidenceThreshold: 70,
		Models: Models{
			Architect: ModelConfig{Model: "opus"},
			Answerer:  ModelConfig{Model: "sonnet"},
		},
	}
	var prompts []string
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1: // confident, commits spec immediately
			writeSpecFile(t, wt)
			return claudeJSON("CONFIDENCE: 90\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 2:
			writePlanFile(t, wt)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case 3:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", len(prompts))
		return "", "", nil
	}}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "clear issue", "", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatalf("high confidence should proceed, got %v", err)
	}
	if len(prompts) != 3 {
		t.Fatalf("got %d calls, want 3 (brainstorm, plan, execute)", len(prompts))
	}
}

func TestFeaturePipelineRecordsExecuteSession(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "brainstorming"):
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "architect-sess"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess"), "", nil
		default: // execute
			return claudeJSON("executed", "execute-sess"), "", nil
		}
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}, Answerer: ModelConfig{Model: "sonnet"}}}
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "the issue", "", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "execute-sess" || si.Kind != "feature" || si.Stage != stageExecute {
		t.Errorf("session = %+v, want execute-sess/feature/execute (latest primary session)", si)
	}
}

// TestFeaturePipelineRecordsSessionOnError verifies the architect's session is
// preserved for -rework even when its call errors (e.g. a 429 session limit)
// after a session id was assigned.
func TestFeaturePipelineRecordsSessionOnError(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: claudeErrorJSON("You've hit your session limit", "arch-429")}}}
	c := &Claude{runner: f, logDir: logDir}
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", "", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err == nil {
		t.Fatal("want the error propagated so the issue is parked")
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("architect session must be recorded even when its call errors: %v", err)
	}
	if si.SessionID != "arch-429" || si.Kind != "feature" || si.Stage != stageBrainstorm {
		t.Errorf("session = %+v, want arch-429/feature/brainstorm", si)
	}
}

// TestFeaturePipelineExecuteUsesExecuteConfig verifies the plan-execution step
// runs under the dedicated execute config while the bounded architect Q&A keeps
// the architect config.
func TestFeaturePipelineExecuteUsesExecuteConfig(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "brainstorming"):
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "architect-sess"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess"), "", nil
		default:
			return claudeJSON("executed", "execute-sess"), "", nil
		}
	}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{
		Architect: ModelConfig{Model: "opus", Effort: "high"},
		Answerer:  ModelConfig{Model: "sonnet"},
		Execute:   ModelConfig{Model: "opus", Effort: "max"},
	}}
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "the issue", "", nil, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	var execArgs, brainArgs []string
	for _, cl := range f.calls {
		if strings.Contains(cl.stdin, "executing-plans") {
			execArgs = cl.args
		}
		if strings.Contains(cl.stdin, "brainstorming") {
			brainArgs = cl.args
		}
	}
	if got := argAfter(execArgs, "--effort"); got != "max" {
		t.Errorf("execute --effort = %q, want max (execute config)", got)
	}
	if got := argAfter(brainArgs, "--effort"); got != "high" {
		t.Errorf("brainstorm --effort = %q, want high (architect config)", got)
	}
}

// TestExecutePlan locks in that executePlan is a single fresh session, no
// sentinel required.
func TestExecutePlan(t *testing.T) {
	wt := t.TempDir()
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		return claudeJSON("Executed, no sentinel here.", "exec-1"), "", nil
	}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := executePlan(context.Background(), c, cfg, wt, "docs/plan.md"); err != nil {
		t.Fatalf("execute must succeed without any sentinel, got %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(f.calls))
	}
	if got := argAfter(f.calls[0].args, "--resume"); got != "" {
		t.Error("execute must be a fresh session")
	}
}

func TestReadPersona(t *testing.T) {
	if got := readPersona(""); got != "" {
		t.Errorf("empty path = %q", got)
	}
	if got := readPersona("/nonexistent/persona.md"); got != "" {
		t.Errorf("missing file = %q", got)
	}
	p := filepath.Join(t.TempDir(), "persona.md")
	os.WriteFile(p, []byte("prefer Go"), 0o644)
	if got := readPersona(p); got != "prefer Go" {
		t.Errorf("persona = %q", got)
	}
}

// Decision 2: the UAT session is launched as soon as the spec is committed, and
// works from that spec. It races with plan/execute, so the assertions key on the
// prompt each session got, never on call ordinals.
func TestFeaturePipelineRunsUATOnTheCommittedSpec(t *testing.T) {
	wt := t.TempDir()
	var uatPrompt, uatModel string
	seen := map[string]int{}
	f := &fakeRunner{}
	// fakeRunner calls its handler under its own mutex, so the counters and the
	// captured prompt need no extra locking even with two sessions in flight.
	f.handler = func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "/superpowers:writing-plans"):
			seen["plan"]++
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case strings.Contains(c.stdin, "/superpowers:executing-plans"):
			seen["execute"]++
			return claudeJSON("Executed.", "exec-1"), "", nil
		case strings.Contains(c.stdin, uatBeginSentinel):
			seen["uat"]++
			uatPrompt, uatModel = c.stdin, argAfter(c.args, "--model")
			return claudeJSON(uatBeginSentinel+"\n- [ ] click it\n"+uatEndSentinel, "uat-1"), "", nil
		default: // architect: commits the spec straight away
			seen["architect"]++
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		}
	}
	tgt := &fakeUATTarget{body: "the issue body"}
	cfg := featureConfig()
	cfg.Models.UAT = ModelConfig{Model: "sonnet"}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "ISSUE", "PERSONA", &UAT{Target: tgt, Num: 7}, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"architect", "uat", "plan", "execute"} {
		if seen[want] != 1 {
			t.Errorf("%s sessions = %d, want exactly 1 (all: %v)", want, seen[want], seen)
		}
	}
	if !strings.Contains(uatPrompt, "2026-07-13-thing-design.md") {
		t.Errorf("the UAT session must work from the committed spec, got: %s", uatPrompt)
	}
	if uatModel != "sonnet" {
		t.Errorf("UAT model = %q, want the models.uat block", uatModel)
	}
	if len(tgt.posted) != 1 {
		t.Errorf("posted %d UAT comments, want 1", len(tgt.posted))
	}
}

// The UAT session and the plan session run at the same time: UAT is a side
// errand on the committed spec, so it must not hold the plan session up. The
// gate blocks the UAT call until the plan call has started — under a sequential
// pipeline the plan call never arrives and the gate times out.
func TestFeaturePipelineRunsUATConcurrentlyWithPlan(t *testing.T) {
	wt := t.TempDir()
	f := &fakeRunner{}
	planStarted := make(chan struct{})
	f.handler = func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "/superpowers:writing-plans"):
			close(planStarted)
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case strings.Contains(c.stdin, "/superpowers:executing-plans"):
			return claudeJSON("Executed.", "exec-1"), "", nil
		case strings.Contains(c.stdin, uatBeginSentinel):
			return claudeJSON(uatBeginSentinel+"\n- [ ] click it\n"+uatEndSentinel, "uat-1"), "", nil
		default: // architect: commits the spec straight away
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		}
	}
	var overlapped atomic.Bool
	g := &gateRunner{inner: f, gate: func(dir, name, stdin string) chan struct{} {
		if name == "claude" && strings.Contains(stdin, uatBeginSentinel) {
			select {
			case <-planStarted:
				overlapped.Store(true)
			case <-time.After(3 * time.Second):
			}
		}
		return nil
	}}
	tgt := &fakeUATTarget{body: "the issue body"}
	cfg := featureConfig()
	cfg.Models.UAT = ModelConfig{Model: "sonnet"}
	c := &Claude{runner: g}
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "ISSUE", "PERSONA", &UAT{Target: tgt, Num: 7}, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	if !overlapped.Load() {
		t.Error("the plan session did not start while the UAT session was still running — UAT is blocking the pipeline")
	}
	// Still published, and the pipeline waited for it before returning.
	if len(tgt.posted) != 1 {
		t.Errorf("posted %d UAT comments, want 1", len(tgt.posted))
	}
}

// Non-blocking: a UAT session that errors must not stop plan and execute.
func TestFeaturePipelineContinuesWhenUATFails(t *testing.T) {
	wt := t.TempDir()
	seen := map[string]int{}
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "/superpowers:writing-plans"):
			seen["plan"]++
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case strings.Contains(c.stdin, "/superpowers:executing-plans"):
			seen["execute"]++
			return claudeJSON("Executed.", "exec-1"), "", nil
		case strings.Contains(c.stdin, uatBeginSentinel):
			seen["uat"]++
			return "", "boom", fmt.Errorf("exit 1") // the UAT session fails
		default:
			seen["architect"]++
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		}
	}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA",
		&UAT{Target: &fakeUATTarget{body: "body"}, Num: 7}, testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatalf("a failed UAT session must never block the pipeline: %v", err)
	}
	if seen["plan"] != 1 || seen["execute"] != 1 {
		t.Errorf("sessions = %v, want the pipeline to have run plan and execute anyway", seen)
	}
}

// TestResumeFeaturePipelineBrainstormStage resumes an architect session with
// --resume and the trigger prompt instead of calling brainstorm-0, then
// continues through the ordinary round loop to a fresh plan+execute.
func TestResumeFeaturePipelineBrainstormStage(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.HasPrefix(c.stdin, "continue"):
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-sess"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess"), "", nil
		default: // execute
			return claudeJSON("executed", "execute-sess"), "", nil
		}
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := featureConfig()
	session := SessionNode{ID: "arch-sess", Kind: "feature", Stage: stageBrainstorm}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, wt, "the issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	// The resumed architect call must carry --resume arch-sess and lead with
	// the trigger (the sentinel-contract wrapper follows it).
	found := false
	for _, call := range f.calls {
		if call.name == "claude" && strings.HasPrefix(call.stdin, "continue") && argAfter(call.args, "--resume") == "arch-sess" {
			found = true
		}
	}
	if !found {
		t.Error("want a claude call with --resume arch-sess and a prompt leading with \"continue\"")
	}
	si, err := readSession(logDir)
	if err != nil || si.SessionID != "execute-sess" || si.Stage != stageExecute {
		t.Errorf("session = %+v, err = %v, want execute-sess/execute after resuming through to execute", si, err)
	}
}

// TestResumeFeaturePipelineBrainstormStageLowConfidenceEscalates mirrors
// TestFeaturePipelineLowConfidenceEscalates for the resume path: a
// brainstorm-resume turn scoring below threshold must escalate instead of
// falling through to the ordinary Q&A round loop.
func TestResumeFeaturePipelineBrainstormStageLowConfidenceEscalates(t *testing.T) {
	wt := t.TempDir()
	cfg := &Config{
		MaxQARounds:         3,
		ConfidenceThreshold: 70,
		Models: Models{
			Architect: ModelConfig{Model: "opus"},
			Answerer:  ModelConfig{Model: "sonnet"},
		},
	}
	count := 0
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		count++
		return claudeJSON("CONFIDENCE: 40\nThe issue has no acceptance criteria.\nWhat output format is expected?", "arch-sess"), "", nil
	}}
	c := &Claude{runner: f}
	session := SessionNode{ID: "arch-sess", Kind: "feature", Stage: stageBrainstorm}
	err := ResumeFeaturePipeline(context.Background(), c, cfg, wt, "vague issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-1", "Feature title", 1)
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if lc.score != 40 {
		t.Errorf("score = %d, want 40", lc.score)
	}
	if count != 1 {
		t.Errorf("low confidence must stop after the first turn, got %d calls", count)
	}
}

// TestResumeFeaturePipelinePlanStage resumes the plan session directly,
// skipping brainstorm entirely, then runs execute fresh.
func TestResumeFeaturePipelinePlanStage(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if argAfter(c.args, "--resume") == "plan-sess" {
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess-2"), "", nil
		}
		return claudeJSON("executed", "execute-sess"), "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := featureConfig()
	session := SessionNode{ID: "plan-sess", Kind: "feature", Stage: stagePlan}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, wt, "the issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil || si.Stage != stageExecute {
		t.Errorf("session = %+v, err = %v, want stage execute", si, err)
	}
}

// TestResumeFeaturePipelineExecuteStage resumes the execute session directly
// with the trigger prompt.
func TestResumeFeaturePipelineExecuteStage(t *testing.T) {
	logDir := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if argAfter(c.args, "--resume") == "exec-sess" && c.stdin == "continue" {
			return claudeJSON("executed more", "exec-sess-2"), "", nil
		}
		return "", "unexpected call", fmt.Errorf("unexpected call: %+v", c)
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := featureConfig()
	session := SessionNode{ID: "exec-sess", Kind: "feature", Stage: stageExecute}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, "/wt", "the issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil || si.SessionID != "exec-sess-2" || si.Stage != stageExecute {
		t.Errorf("session = %+v, err = %v, want exec-sess-2/execute", si, err)
	}
}

// TestResumeFeaturePipelineUnknownStageFallsBackToFresh is the safety net: a
// stage value that isn't one of the three known ones re-enters at brainstorm-0
// exactly like a fresh pipeline, rather than erroring.
func TestResumeFeaturePipelineUnknownStageFallsBackToFresh(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "brainstorming"):
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "fresh-arch"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess"), "", nil
		default:
			return claudeJSON("executed", "execute-sess"), "", nil
		}
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := featureConfig()
	session := SessionNode{ID: "stale-sess", Kind: "feature", Stage: "bogus"}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, wt, "the issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-1", "Feature title", 1); err != nil {
		t.Fatal(err)
	}
	// Fresh brainstorm-0 call must have fired with no --resume.
	for _, call := range f.calls {
		if call.name == "claude" && strings.Contains(call.stdin, "brainstorming") && argAfter(call.args, "--resume") != "" {
			t.Error("unknown stage must fall back to a FRESH brainstorm-0 call, not resume the stale session")
		}
	}
}

// TestBrainstormLoopCheckpointsPlanStageBeforePlanCall guards against the
// issue-5 incident: the plan session died mid-flight (usage limit) and, with
// the stage only recorded AFTER a call returns, the persisted stage stayed
// "brainstorm" — so the resume re-entered the Q&A loop against a session whose
// spec was long committed and burned all 20 rounds. The spec-complete
// checkpoint must land BEFORE the plan call starts: stage plan, with the spec
// path, so a mid-plan crash resumes at a fresh plan session instead.
func TestBrainstormLoopCheckpointsPlanStageBeforePlanCall(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	var atPlanCall SessionNode
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "brainstorming"):
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			node, ok := resumePoint(logDir)
			if !ok {
				t.Error("no resume point recorded when the plan call starts")
			}
			atPlanCall = node
			return "", "killed", errors.New("usage limit hit mid-plan")
		}
		t.Fatalf("unexpected call: %q", c.stdin)
		return "", "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA", nil, testGH(), testWT(), "ai/issue-5", "Feature title", 5)
	if err == nil {
		t.Fatal("want the plan failure propagated so the issue parks")
	}
	if atPlanCall.Stage != stagePlan {
		t.Errorf("stage at plan-call time = %q, want %q (checkpoint must precede the plan call)", atPlanCall.Stage, stagePlan)
	}
	if atPlanCall.Artifact != "docs/superpowers/specs/2026-07-13-thing-design.md" {
		t.Errorf("checkpoint artifact = %q, want the committed spec relative to the worktree", atPlanCall.Artifact)
	}
	if atPlanCall.ID != "" {
		t.Errorf("pending checkpoint id = %q, want empty — the plan session hasn't spawned, and offering the architect's id would resume the WRONG session", atPlanCall.ID)
	}
	// The checkpoint must survive the crash: this is what the next cycle resumes from.
	node, ok := resumePoint(logDir)
	if !ok || node.Stage != stagePlan || node.Artifact == "" {
		t.Errorf("post-crash resume point = %+v ok=%v; a resume would re-enter the brainstorm Q&A loop", node, ok)
	}
}

// TestResumeFeaturePipelinePlanStageFreshFromSpecCheckpoint: a stagePlan
// record carrying a SpecPath means the plan session never completed (a
// completed plan call re-records itself without one). Resume must start a
// FRESH plan session from the committed spec — not resume the recorded
// (architect) session, and not re-enter the brainstorm loop.
func TestResumeFeaturePipelinePlanStageFreshFromSpecCheckpoint(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	writeSpecFile(t, wt)
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case strings.Contains(c.stdin, "executing-plans"):
			return claudeJSON("executed", "exec-1"), "", nil
		}
		t.Fatalf("unexpected prompt %q — a spec checkpoint must resume at a fresh plan session", c.stdin)
		return "", "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	session := SessionNode{Kind: "feature", Stage: stagePlan,
		Artifact: "docs/superpowers/specs/2026-07-13-thing-design.md"}
	if err := ResumeFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-5", "Feature title", 5); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) == 0 {
		t.Fatal("no claude calls")
	}
	first := f.calls[0]
	if got := argAfter(first.args, "--resume"); got != "" {
		t.Errorf("plan session must be fresh, got --resume %q", got)
	}
	if !strings.Contains(first.stdin, "2026-07-13-thing-design.md") {
		t.Errorf("fresh plan prompt must carry the checkpointed spec path, got %q", first.stdin)
	}
}

// TestResumeFeaturePipelinePlanStageCheckpointMissingSpecFallsBackFresh is the
// safety net: a checkpoint whose spec file no longer exists restarts the full
// pipeline fresh rather than erroring or looping.
func TestResumeFeaturePipelinePlanStageCheckpointMissingSpecFallsBackFresh(t *testing.T) {
	wt := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "brainstorming"):
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "fresh-arch"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		default:
			return claudeJSON("executed", "exec-1"), "", nil
		}
	}}
	c := &Claude{runner: f}
	session := SessionNode{Kind: "feature", Stage: stagePlan, Artifact: "docs/superpowers/specs/gone.md"}
	if err := ResumeFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-5", "Feature title", 5); err != nil {
		t.Fatalf("missing checkpoint spec must fall back to a fresh pipeline, got %v", err)
	}
	fresh := false
	for _, call := range f.calls {
		if strings.Contains(call.stdin, "brainstorming") && argAfter(call.args, "--resume") == "" {
			fresh = true
		}
	}
	if !fresh {
		t.Error("want a fresh brainstorm-0 call when the checkpointed spec file is gone")
	}
}

// TestResumeFeaturePipelineBrainstormPromptRestatesSentinels guards the second
// leg of the issue-5 incident: the resumed architect was re-entered with a bare
// "continue" that never restated the sentinel contract, so it reported "already
// shipped" in prose the loop could not parse and burned every Q&A round. The
// resumed brainstorm prompt must carry the trigger AND re-teach both terminal
// sentinels.
func TestResumeFeaturePipelineBrainstormPromptRestatesSentinels(t *testing.T) {
	wt := t.TempDir()
	var resumedPrompt string
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case argAfter(c.args, "--resume") == "arch-sess":
			resumedPrompt = c.stdin
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-sess"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		default:
			return claudeJSON("executed", "exec-1"), "", nil
		}
	}}
	c := &Claude{runner: f}
	session := SessionNode{ID: "arch-sess", Kind: "feature", Stage: stageBrainstorm}
	if err := ResumeFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-5", "Feature title", 5); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resumedPrompt, "continue") {
		t.Errorf("resumed prompt must carry the trigger, got %q", resumedPrompt)
	}
	if !strings.Contains(resumedPrompt, specReadySentinel) || !strings.Contains(resumedPrompt, alreadyDoneSentinel) {
		t.Errorf("resumed prompt must restate the %s and %s contract, got %q", specReadySentinel, alreadyDoneSentinel, resumedPrompt)
	}
}

// TestBrainstormLoopNudgesArchitectWhenNothingToAnswer guards the third leg of
// the issue-5 incident: the architect sent pure status updates ("watching CI"),
// the answerer replied "Approved", and the pair ping-ponged a round away every
// few seconds. When the answerer signals there was nothing to answer, the loop
// must send the architect a nudge that restates the terminal sentinels instead
// of relaying the sentinel reply verbatim.
func TestBrainstormLoopNudgesArchitectWhenNothingToAnswer(t *testing.T) {
	wt := t.TempDir()
	var prompts []string
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1: // architect: a status update, no question
			return claudeJSON("Watcher armed; waiting on CI. Will merge on green.", "arch-1"), "", nil
		case 2: // answerer: nothing to answer
			return claudeJSON(nothingToAnswerSentinel, "ans-1"), "", nil
		case 3: // architect resumed: must receive the sentinel-restating nudge
			return claudeJSON("PIPELINE_ALREADY_DONE: shipped and merged already", "arch-1"), "", nil
		case 4: // answerer done-confirmation
			return claudeJSON("Agreed. DONE_CONFIRMED", "ans-1"), "", nil
		}
		t.Fatalf("unexpected call %d: %q", len(prompts), c.stdin)
		return "", "", nil
	}}
	c := &Claude{runner: f}
	err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA", nil, testGH(), testWT(), "ai/issue-5", "Feature title", 5)
	var done *alreadyDoneError
	if !errors.As(err, &done) {
		t.Fatalf("want *alreadyDoneError once the nudged architect prints the sentinel, got %v", err)
	}
	if len(prompts) != 4 {
		t.Fatalf("got %d calls, want 4", len(prompts))
	}
	if strings.Contains(prompts[2], nothingToAnswerSentinel) {
		t.Errorf("the architect must never see the raw answerer sentinel, got %q", prompts[2])
	}
	if !strings.Contains(prompts[2], specReadySentinel) || !strings.Contains(prompts[2], alreadyDoneSentinel) {
		t.Errorf("nudge must restate the terminal sentinels, got %q", prompts[2])
	}
}

// TestRunPlanThenExecuteCheckpointsExecuteStageBeforeExecuteCall is the
// issue-4 incident, one stage later than issue-5's: the execute session died
// mid-flight and the stage stayed "plan", so the resume re-entered the
// COMPLETED plan session with a bare "continue" — which explained itself in
// prose, never re-printed PIPELINE_READY, and parked the issue. The moment the
// plan file is committed, the checkpoint must advance to stage execute with
// the plan path, before the execute call starts.
func TestRunPlanThenExecuteCheckpointsExecuteStageBeforeExecuteCall(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	var atExecCall SessionNode
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "writing-plans"):
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case strings.Contains(c.stdin, "executing-plans"):
			node, ok := resumePoint(logDir)
			if !ok {
				t.Error("no resume point recorded when the execute call starts")
			}
			atExecCall = node
			return "", "killed", errors.New("usage limit hit mid-execute")
		}
		t.Fatalf("unexpected call: %q", c.stdin)
		return "", "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		"docs/superpowers/specs/2026-07-13-thing-design.md",
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), testGH(), testWT(), "ai/issue-4", 4)
	if err == nil {
		t.Fatal("want the execute failure propagated so the issue parks")
	}
	if atExecCall.Stage != stageExecute {
		t.Errorf("stage at execute-call time = %q, want %q (checkpoint must precede the execute call)", atExecCall.Stage, stageExecute)
	}
	if atExecCall.Artifact != "docs/superpowers/plans/2026-07-06-thing.md" {
		t.Errorf("checkpoint artifact = %q, want the committed plan relative to the worktree", atExecCall.Artifact)
	}
	if atExecCall.ID != "" {
		t.Errorf("pending checkpoint id = %q, want empty — offering the plan session's id would resume the WRONG (completed) session", atExecCall.ID)
	}
	node, ok := resumePoint(logDir)
	if !ok || node.Stage != stageExecute || node.Artifact == "" {
		t.Errorf("post-crash resume point = %+v ok=%v; a resume would re-enter the completed plan session", node, ok)
	}
}

// TestResumeFeaturePipelineExecuteStageFreshFromPlanCheckpoint: a stageExecute
// record carrying a PlanPath means the execute session never completed (a
// completed execute call re-records itself without one). Resume must run a
// FRESH execute session on the committed plan — the executing-plans skill
// picks up from whatever steps are already done — instead of resuming the
// recorded (plan) session.
func TestResumeFeaturePipelineExecuteStageFreshFromPlanCheckpoint(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	writePlanFile(t, wt)
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if strings.Contains(c.stdin, "executing-plans") {
			return claudeJSON("executed", "exec-1"), "", nil
		}
		t.Fatalf("unexpected prompt %q — a plan checkpoint must resume at a fresh execute session", c.stdin)
		return "", "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	session := SessionNode{Kind: "feature", Stage: stageExecute,
		Artifact: "docs/superpowers/plans/2026-07-06-thing.md"}
	if err := ResumeFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", "", nil, session, "continue", testGH(), testWT(), "ai/issue-4", "Feature title", 4); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) == 0 {
		t.Fatal("no claude calls")
	}
	first := f.calls[0]
	if got := argAfter(first.args, "--resume"); got != "" {
		t.Errorf("execute session must be fresh, got --resume %q", got)
	}
	if !strings.Contains(first.stdin, "2026-07-06-thing.md") {
		t.Errorf("fresh execute prompt must carry the checkpointed plan path, got %q", first.stdin)
	}
}

// TestRunPlanThenExecutePushesAndCommentsPlanUpdate locks in spec §1's
// plan-stage push point: a push, then a fixed "Updated plan: ..." comment
// naming the plan file relative to the worktree root — BEFORE the execute
// session starts. No PR is created here (the spec stage already created it).
func TestRunPlanThenExecutePushesAndCommentsPlanUpdate(t *testing.T) {
	wt := t.TempDir()

	var ghCalls []string
	gf := &fakeRunner{}
	gf.handler = func(c rcall) (string, string, error) {
		ghCalls = append(ghCalls, c.name+" "+strings.Join(c.args, " "))
		return "", "", nil
	}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	wtree := &Worktree{runner: gf}

	var calls int
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		calls++
		switch calls {
		case 1: // fresh plan session: commits the plan
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 2: // executor: the plan-stage push/comment must already have run
			if len(ghCalls) == 0 {
				t.Fatal("execute session started before the plan-stage push/comment ran")
			}
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", calls)
		return "", "", nil
	}}
	c := &Claude{runner: cf}

	err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		"docs/superpowers/specs/2026-07-13-thing-design.md",
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), gh, wtree, "ai/issue-9", 9)
	if err != nil {
		t.Fatal(err)
	}
	var sawComment, sawCreate bool
	pushes := 0
	for _, call := range ghCalls {
		if strings.HasPrefix(call, "git push") {
			pushes++
		}
		if strings.Contains(call, "issue comment") && strings.Contains(call, "Updated plan") &&
			strings.Contains(call, "docs/superpowers/plans/2026-07-06-thing.md") {
			sawComment = true
		}
		if strings.Contains(call, "pr create") {
			sawCreate = true
		}
	}
	if pushes != 2 {
		t.Errorf("want 2 pushes (plan-stage + execute-stage), got %d: %v", pushes, ghCalls)
	}
	if !sawComment {
		t.Errorf("want the 'Updated plan' comment naming the plan file, got %v", ghCalls)
	}
	if sawCreate {
		t.Error("the plan stage must never create a PR — the spec stage already did")
	}
}

// TestRunPlanThenExecutePlanPushFailureDoesNotFailPipeline is decision 5 for
// the plan stage: a push/comment failure must not abort the pipeline.
func TestRunPlanThenExecutePlanPushFailureDoesNotFailPipeline(t *testing.T) {
	wt := t.TempDir()
	gf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "push") {
			return "", "timeout", errors.New("git push: timeout")
		}
		return "", "", nil
	}}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	gh.retry = testRetry
	wtree := &Worktree{runner: gf, retry: testRetry}

	var calls int
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		calls++
		switch calls {
		case 1:
			writePlanFile(t, wt)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case 2:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		return "", "", nil
	}}
	c := &Claude{runner: cf}
	err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		"docs/superpowers/specs/2026-07-13-thing-design.md",
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), gh, wtree, "ai/issue-9", 9)
	if err != nil {
		t.Fatalf("a failed plan-stage push must not fail the pipeline, got %v", err)
	}
}

// TestRunPlanThenExecutePushesAfterExecuteCompletes locks in spec §1's third
// push point: after executePlan succeeds, push once more — no comment.
func TestRunPlanThenExecutePushesAfterExecuteCompletes(t *testing.T) {
	wt := t.TempDir()
	var ghCalls []string
	gf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		ghCalls = append(ghCalls, c.name+" "+strings.Join(c.args, " "))
		return "", "", nil
	}}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	wtree := &Worktree{runner: gf}

	var calls int
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		calls++
		switch calls {
		case 1:
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 2:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", calls)
		return "", "", nil
	}}
	c := &Claude{runner: cf}

	err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		"docs/superpowers/specs/2026-07-13-thing-design.md",
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), gh, wtree, "ai/issue-9", 9)
	if err != nil {
		t.Fatal(err)
	}
	pushes := 0
	for _, call := range ghCalls {
		if strings.HasPrefix(call, "git push") {
			pushes++
		}
		if strings.Contains(call, "issue comment") && !strings.Contains(call, "Updated plan") {
			t.Errorf("the execute stage must not comment, got %v", call)
		}
	}
	if pushes != 2 {
		t.Errorf("want 2 pushes (plan-stage + execute-stage), got %d: %v", pushes, ghCalls)
	}
}

// TestExecuteStagePushFailureDoesNotFailPipeline is decision 5 for the
// execute stage: a push failure after executePlan succeeds must not fail an
// otherwise-successful pipeline run.
func TestExecuteStagePushFailureDoesNotFailPipeline(t *testing.T) {
	wt := t.TempDir()
	gf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "push") {
			return "", "timeout", errors.New("git push: timeout")
		}
		return "", "", nil
	}}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	gh.retry = testRetry
	wtree := &Worktree{runner: gf, retry: testRetry}

	var calls int
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		calls++
		switch calls {
		case 1:
			writePlanFile(t, wt)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case 2:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		return "", "", nil
	}}
	c := &Claude{runner: cf}
	if err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		"docs/superpowers/specs/2026-07-13-thing-design.md",
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), gh, wtree, "ai/issue-9", 9); err != nil {
		t.Fatalf("a failed execute-stage push must not fail the pipeline, got %v", err)
	}
}

// TestResumeFeaturePipelineExecuteStagePushesAfterSuccess covers the OTHER
// path to the execute-stage push: ResumeFeaturePipeline's stageExecute case.
func TestResumeFeaturePipelineExecuteStagePushesAfterSuccess(t *testing.T) {
	logDir := t.TempDir()
	var ghCalls []string
	gf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		ghCalls = append(ghCalls, c.name+" "+strings.Join(c.args, " "))
		return "", "", nil
	}}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	wtree := &Worktree{runner: gf}
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		return claudeJSON("executed more", "exec-sess-2"), "", nil
	}}
	c := &Claude{runner: cf, logDir: logDir}
	cfg := featureConfig()
	session := SessionNode{ID: "exec-sess", Kind: "feature", Stage: stageExecute}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, "/wt", "the issue", "", nil, session, "continue",
		gh, wtree, "ai/issue-9", "Add export", 9); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range ghCalls {
		if strings.HasPrefix(call, "git push") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a push after the resumed execute session succeeds, got %v", ghCalls)
	}
}
