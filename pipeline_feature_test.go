package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE CONTENT", "PERSONA"); err != nil {
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

func TestFeaturePipelineFailsAfterMaxRounds(t *testing.T) {
	wt := t.TempDir()
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		return claudeJSON("Still thinking...", "s1"), "", nil
	}
	c := &Claude{runner: f}
	err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "issue", "")
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
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "ISSUE CONTENT", "PERSONA"); err != nil {
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
	err := RunFeaturePipeline(context.Background(), c, cfg, wt, "ISSUE CONTENT", "PERSONA")
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
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "issue", ""); err == nil {
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
	err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA")
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
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA"); err != nil {
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
	err := RunFeaturePipeline(context.Background(), c, cfg, wt, "vague issue", "")
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
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "clear issue", ""); err != nil {
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
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "the issue", ""); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "execute-sess" || si.Kind != "feature" {
		t.Errorf("session = %+v, want execute-sess/feature (latest primary session)", si)
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
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", ""); err == nil {
		t.Fatal("want the error propagated so the issue is parked")
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("architect session must be recorded even when its call errors: %v", err)
	}
	if si.SessionID != "arch-429" || si.Kind != "feature" {
		t.Errorf("session = %+v, want arch-429/feature", si)
	}
}

// TestFeaturePipelineExecuteUsesExecuteConfig verifies the plan-execution step
// runs under the dedicated execute config (higher turn ceiling) while the
// bounded architect Q&A keeps the architect config — so raising execute turns
// doesn't inflate brainstorm rounds.
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
		Architect: ModelConfig{Model: "opus", MaxTurns: 100},
		Answerer:  ModelConfig{Model: "sonnet"},
		Execute:   ModelConfig{Model: "opus", MaxTurns: 300},
	}}
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "the issue", ""); err != nil {
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
	if got := argAfter(execArgs, "--max-turns"); got != "300" {
		t.Errorf("execute --max-turns = %q, want 300 (execute config)", got)
	}
	if got := argAfter(brainArgs, "--max-turns"); got != "100" {
		t.Errorf("brainstorm --max-turns = %q, want 100 (architect config)", got)
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
