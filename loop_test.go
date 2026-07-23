package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeEnv simulates gh/git/claude for orchestrator tests.
type fakeEnv struct {
	f          *fakeRunner
	wtDir      string
	failClaude bool // make pipeline claude calls fail
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	env := &fakeEnv{f: &fakeRunner{}, wtDir: t.TempDir()}
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		switch c.name {
		case "gh":
			switch {
			case strings.HasPrefix(joined, "issue list"):
				return `[{"number": 7, "title": "Fix crash", "body": "boom", "labels": [{"name": "ai-agent"}]}]`, "", nil
			case strings.HasPrefix(joined, "issue view"):
				return `{"title": "Fix crash", "body": "boom", "comments": []}`, "", nil
			case strings.HasPrefix(joined, "pr create"):
				return "https://github.com/org/repo/pull/99\n", "", nil
			}
			return "", "", nil
		case "git":
			switch {
			case strings.Contains(joined, "symbolic-ref"):
				return "origin/main\n", "", nil
			case strings.Contains(joined, "rev-list --count"):
				return "2\n", "", nil
			}
			return "", "", nil
		case "claude":
			prompt := c.stdin
			if strings.Contains(prompt, "triage agent") {
				return claudeJSON(`{"issueNumber": 7, "kind": "bug", "reason": "small"}`, "t1"), "", nil
			}
			if env.failClaude {
				return "", "boom", fmt.Errorf("exit 1")
			}
			return claudeJSON("Fixed and committed.", "d1"), "", nil
		}
		return "", "", nil
	}
	return env
}

func (e *fakeEnv) orchestrator() *Orchestrator {
	return e.orchestratorWithLabels(defaultStateLabels())
}

func (e *fakeEnv) orchestratorWithLabels(sl StateLabels) *Orchestrator {
	cfg := &Config{
		RepoPath: "/clone", RepoSlug: "org/repo", EligibleLabel: "ai-agent",
		WorkDir: e.wtDir, MaxQARounds: 3, StateLabels: sl,
		Models: Models{
			Architect: ModelConfig{Model: "opus", Effort: "high"},
			Answerer:  ModelConfig{Model: "sonnet"},
			Triage:    ModelConfig{Model: "sonnet"},
		},
	}
	o := &Orchestrator{cfg: cfg, runner: e.f, gh: NewGitHub(e.f, cfg), wt: &Worktree{runner: e.f, repoPath: cfg.RepoPath}}
	o.gh.retry = testRetry
	o.wt.retry = testRetry
	return o
}

// callsMatching returns joined arg strings of calls whose name and args match.
func (e *fakeEnv) callsMatching(name, substr string) []string {
	var out []string
	for _, c := range e.f.calls {
		joined := strings.Join(c.args, " ")
		if c.name == name && strings.Contains(joined, substr) {
			out = append(out, joined)
		}
	}
	return out
}

func TestProcessOnceLowConfidenceEscalatesToNeedsInfo(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		// Triage picks a feature; make the brainstorm session escalate.
		if c.name == "claude" && strings.Contains(c.stdin, "triage agent") {
			return claudeJSON(`{"issueNumber": 7, "kind": "feature", "reason": "needs design"}`, "t1"), "", nil
		}
		if c.name == "claude" && strings.Contains(c.stdin, "brainstorming") {
			return claudeJSON("CONFIDENCE: 30\nNo acceptance criteria — what should the export contain?", "arch-1"), "", nil
		}
		return base(c)
	}
	o := env.orchestrator()
	o.cfg.ConfidenceThreshold = 70
	if err := runCycle(o); err != nil {
		t.Fatalf("needs-info is a clean outcome, want nil error, got %v", err)
	}
	// Label swap ai-wip -> ai-needs-info (single atomic call).
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-needs-info") {
		t.Errorf("want single ai-wip->ai-needs-info swap, got: %v", swap)
	}
	// Feedback commented, with the score and without the CONFIDENCE sentinel line.
	var commented bool
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "acceptance criteria") {
			commented = true
		}
	}
	if !commented {
		t.Error("needs-info path should comment the architect's feedback on the issue")
	}
	// Must not close, ship, or mark rework/failed.
	if len(env.callsMatching("gh", "issue close")) != 0 {
		t.Error("needs-info must not close the issue")
	}
	if len(env.callsMatching("gh", "pr create")) != 0 {
		t.Error("needs-info must not create a PR")
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) != 0 {
		t.Error("needs-info must not park as rework")
	}
	// Worktree was created for the pipeline, then removed (no progress to keep).
	if len(env.callsMatching("git", "worktree remove")) == 0 {
		t.Error("needs-info path should remove the worktree")
	}
}

func TestProcessOnceNoIssuesIsNoop(t *testing.T) {
	env := newFakeEnv(t)
	env.f.handler = func(c rcall) (string, string, error) { return "[]", "", nil }
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.f.calls) != 1 {
		t.Errorf("calls = %d, want only the issue list", len(env.f.calls))
	}
}

func TestProcessOnceHappyPathBug(t *testing.T) {
	env := newFakeEnv(t)
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ name, substr string }{
		{"gh", "--add-label ai-wip"},
		{"git", "worktree add"},
		{"git", "push"},
		{"gh", "pr create"},
		{"gh", "--remove-label ai-wip"},
		{"gh", "--add-label ai-done"},
		{"git", "worktree remove"},
	} {
		if len(env.callsMatching(want.name, want.substr)) == 0 {
			t.Errorf("missing call %s %q", want.name, want.substr)
		}
	}
	if len(env.callsMatching("gh", "--add-label ai-failed")) != 0 {
		t.Error("happy path must not add ai-failed")
	}
	// wip->done swap must be a single atomic gh call, not two separate calls.
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-done") {
		t.Errorf("want a single gh call with both --remove-label ai-wip and --add-label ai-done, got matches: %v", swap)
	}
	// PR link commented on the issue
	found := false
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "pull/99") {
			found = true
		}
	}
	if !found {
		t.Error("PR URL should be commented on the issue")
	}
}

func TestProcessOnceUsesConfiguredStateLabels(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestratorWithLabels(StateLabels{WIP: "bot-wip", Failed: "bot-failed", Done: "bot-done"})
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "--add-label bot-wip")) == 0 {
		t.Error("pickup should add the configured wip label")
	}
	swap := env.callsMatching("gh", "--remove-label bot-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label bot-done") {
		t.Errorf("want single swap to configured done label, got: %v", swap)
	}
	for _, stale := range []string{"ai-wip", "ai-done", "ai-failed"} {
		if len(env.callsMatching("gh", stale)) != 0 {
			t.Errorf("default label %q must not be used when overridden", stale)
		}
	}
}

func TestProcessOnceFailurePathParksForRework(t *testing.T) {
	env := newFakeEnv(t)
	env.failClaude = true
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("a failing pipeline must not be returned from the cycle, got %v", err)
	}
	// Parked as ai-rework, not ai-failed.
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-rework") {
		t.Errorf("want single ai-wip->ai-rework swap, got: %v", swap)
	}
	if len(env.callsMatching("gh", "--add-label ai-failed")) != 0 {
		t.Error("failure path must no longer mark ai-failed")
	}
	// Progress preserved: no worktree removal, no branch deletion, no PR/push.
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("failure path must preserve the worktree for rework")
	}
	if len(env.callsMatching("git", "branch -D")) != 0 {
		t.Error("failure path must preserve the branch for rework")
	}
	if len(env.callsMatching("gh", "pr create")) != 0 {
		t.Error("failure path must not create a PR")
	}
	if len(env.callsMatching("git", "push")) != 0 {
		t.Error("failure path must not push")
	}
}

func TestParkWritesCauseAndShipClearsIt(t *testing.T) {
	env := newFakeEnv(t)
	env.failClaude = true
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if got := readParkCause(logDir); got == "" {
		t.Fatal("park must record the failure cause")
	}

	// A later successful run through ship() clears the stale cause.
	env2 := newFakeEnv(t)
	logDir2 := filepath.Join(env2.wtDir, "logs", "issue-7")
	recordParkCause(logDir2, "old cause")
	if err := runCycle(env2.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := readParkCause(logDir2); got != "" {
		t.Errorf("ship success must clear park-cause, got %q", got)
	}
}

// readLocalState returns the contents of the issue's local state marker, or ""
// if none was written.
func (e *fakeEnv) readLocalState(n int) string {
	b, err := os.ReadFile(filepath.Join(e.wtDir, "logs", fmt.Sprintf("issue-%d", n), "state"))
	if err != nil {
		return ""
	}
	return string(b)
}

// TestProcessOnceRecordsLocalStateDone asserts a shipped issue leaves an
// ai-done marker on disk so the dashboard reflects the transition without
// re-polling gh.
func TestProcessOnceRecordsLocalStateDone(t *testing.T) {
	env := newFakeEnv(t)
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := env.readLocalState(7); got != "ai-done" {
		t.Fatalf("local state marker = %q, want ai-done", got)
	}
}

// TestProcessOnceRecordsLocalStateRework asserts the park path records ai-rework
// locally, matching the gh label swap.
func TestProcessOnceRecordsLocalStateRework(t *testing.T) {
	env := newFakeEnv(t)
	env.failClaude = true
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	if got := env.readLocalState(7); got != "ai-rework" {
		t.Fatalf("local state marker = %q, want ai-rework", got)
	}
}

// A deterministic tooling failure (here: git push) happens AFTER the pipeline
// has already produced commits. It must NOT be marked ai-failed, and it must NOT
// discard that work: instead the issue is parked for rework (ai-wip->ai-rework)
// with the worktree preserved, so it resumes rather than re-running the whole
// pipeline from zero next cycle.
func TestToolingFailureDoesNotMarkFailed(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "push") {
			return "", "remote: protected branch hook declined", fmt.Errorf("exit 1")
		}
		return base(c)
	}
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	// It must not have swapped to a terminal state label.
	for _, term := range []string{"--add-label ai-failed", "--add-label ai-done"} {
		if len(env.callsMatching("gh", term)) != 0 {
			t.Errorf("tooling failure must not add a terminal label (%s)", term)
		}
	}
	// It parks for rework: ai-wip -> ai-rework, recorded locally too.
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-rework") {
		t.Errorf("want single ai-wip->ai-rework park swap, got: %v", swap)
	}
	if got := env.readLocalState(7); got != "ai-rework" {
		t.Errorf("local state = %q, want ai-rework", got)
	}
	// The worktree (holding the pipeline's commits) must be preserved for resume.
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("tooling failure must preserve the worktree for rework, not remove it")
	}
}

// If the terminal WIP->Done swap fails, the error must be surfaced, not
// swallowed: the PR was created but the issue would otherwise silently look
// unfinished. The cycle no longer returns pipeline errors (the pipeline outlives
// it), so the surface is the daemon log the goroutine writes.
func TestDoneSwapFailureIsSurfaced(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.Contains(joined, "--add-label ai-done") {
			return "", "label not found", fmt.Errorf("exit 1")
		}
		return base(c)
	}
	logged := captureLog(t)
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	out := logged()
	if !strings.Contains(out, "done") && !strings.Contains(out, "Done") {
		t.Errorf("the daemon log should explain the Done swap failed, got: %s", out)
	}
	// The PR was still created — a Done-swap failure must not mark it ai-failed.
	if len(env.callsMatching("gh", "--add-label ai-failed")) != 0 {
		t.Error("Done swap failure must not mark the issue ai-failed")
	}
}

// The pipeline (not triage) now reports "already implemented": triage picks a
// bug, and the debug session prints the done sentinel. handleIssue must close
// the issue via finishDone — no PR, no push — but it DID take the pipeline path,
// so a worktree was created (and must be removed) and ai-wip was applied.
func TestProcessOnceAlreadyDoneClosesIssue(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" && !strings.Contains(c.stdin, "triage agent") {
			return claudeJSON("PIPELINE_ALREADY_DONE: already in place", "d1"), "", nil
		}
		return base(c)
	}
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	// Terminal done actions.
	if len(env.callsMatching("gh", "issue comment")) == 0 {
		t.Error("done path should comment on the issue")
	}
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-done") {
		t.Errorf("want single ai-wip->ai-done swap, got: %v", swap)
	}
	if len(env.callsMatching("gh", "issue close")) == 0 {
		t.Error("done path should close the issue")
	}
	// Worktree was created for the pipeline, then cleaned up.
	if len(env.callsMatching("git", "worktree remove")) == 0 {
		t.Error("done path should remove the worktree")
	}
	// It must not ship anything.
	if len(env.callsMatching("gh", "pr create")) != 0 {
		t.Error("done path must not create a PR")
	}
	if len(env.callsMatching("git", "push")) != 0 {
		t.Error("done path must not push")
	}
	if len(env.callsMatching("gh", "--add-label ai-failed")) != 0 {
		t.Error("done path must not mark the issue ai-failed")
	}
}

func TestFinishDoneUsesConfiguredDoneLabel(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" && !strings.Contains(c.stdin, "triage agent") {
			return claudeJSON("PIPELINE_ALREADY_DONE: x", "d1"), "", nil
		}
		return base(c)
	}
	o := env.orchestratorWithLabels(StateLabels{WIP: "bot-wip", Failed: "bot-failed", Done: "bot-done"})
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	swap := env.callsMatching("gh", "--remove-label bot-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label bot-done") {
		t.Errorf("want single swap to configured labels, got: %v", swap)
	}
}

func TestHandleIssueZeroCommitsParksForRework(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "rev-list --count") {
			return "0\n", "", nil
		}
		return base(c)
	}
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil (the park is the observable outcome)", err)
	}
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-rework") {
		t.Errorf("zero commits should park as ai-rework, got: %v", swap)
	}
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("zero-commit park must preserve the worktree")
	}
}

// With ticketsPerCycle=2 and two eligible issues, one cycle selects and handles
// both (each in its own worktree/branch, each to its own PR). Selection is
// sequential (the fake triage picks the first still-eligible issue in the
// prompt); execution fans out. Run under -race to guard the parallel path.
func TestProcessOnceHandlesMultipleTickets(t *testing.T) {
	env := &fakeEnv{f: &fakeRunner{}, wtDir: t.TempDir()}
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		switch c.name {
		case "gh":
			switch {
			case strings.HasPrefix(joined, "issue list"):
				return `[{"number": 7, "title": "Fix crash", "body": "boom", "labels": [{"name": "ai-agent"}]},
				          {"number": 8, "title": "Fix leak", "body": "drip", "labels": [{"name": "ai-agent"}]}]`, "", nil
			case strings.HasPrefix(joined, "issue view"):
				return `{"title": "T", "body": "b", "comments": []}`, "", nil
			case strings.HasPrefix(joined, "pr create"):
				return "https://github.com/org/repo/pull/99\n", "", nil
			}
			return "", "", nil
		case "git":
			switch {
			case strings.Contains(joined, "symbolic-ref"):
				return "origin/main\n", "", nil
			case strings.Contains(joined, "rev-list --count"):
				return "2\n", "", nil
			}
			return "", "", nil
		case "claude":
			if strings.Contains(c.stdin, "triage agent") {
				// Pick the first issue still present in the candidate list.
				if strings.Contains(c.stdin, `"number": 7`) {
					return claudeJSON(`{"issueNumber": 7, "kind": "bug", "reason": "a"}`, "t"), "", nil
				}
				return claudeJSON(`{"issueNumber": 8, "kind": "bug", "reason": "b"}`, "t"), "", nil
			}
			return claudeJSON("Fixed and committed.", "d"), "", nil
		}
		return "", "", nil
	}
	o := env.orchestrator()
	o.gh.retry = testRetry
	o.wt.retry = testRetry
	o.cfg.TicketsPerCycle = 2

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	wip := env.callsMatching("gh", "--add-label ai-wip")
	if len(wip) != 2 {
		t.Fatalf("want 2 wip labels (one per ticket), got %d: %v", len(wip), wip)
	}
	var got7, got8 bool
	for _, s := range wip {
		if strings.Contains(s, "edit 7") {
			got7 = true
		}
		if strings.Contains(s, "edit 8") {
			got8 = true
		}
	}
	if !got7 || !got8 {
		t.Errorf("want both #7 and #8 picked up, got: %v", wip)
	}
	if n := len(env.callsMatching("git", "worktree add")); n != 2 {
		t.Errorf("worktree add count = %d, want 2", n)
	}
	if n := len(env.callsMatching("gh", "pr create")); n != 2 {
		t.Errorf("pr create count = %d, want 2", n)
	}
}

// seedRework creates the preserved worktree and session file that a parked
// issue would have left behind, so Rework can resume it.
func seedRework(t *testing.T, env *fakeEnv, n int, session, kind string) {
	t.Helper()
	if err := os.MkdirAll(worktreePath(env.wtDir, n), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(env.wtDir, "logs", fmt.Sprintf("issue-%d", n))
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"sessionId":%q,"kind":%q}`, session, kind)
	if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReworkResumesAndShips(t *testing.T) {
	env := newFakeEnv(t)
	seedRework(t, env, 7, "resume-me", "bug")
	if err := env.orchestrator().Rework(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	resumed := false
	for _, c := range env.f.calls {
		if c.name == "claude" && argAfter(c.args, "--resume") == "resume-me" {
			resumed = true
		}
	}
	if !resumed {
		t.Error("rework must resume the saved session id")
	}
	if len(env.callsMatching("git", "push")) == 0 {
		t.Error("rework must push")
	}
	if len(env.callsMatching("gh", "pr create")) == 0 {
		t.Error("rework must open a PR")
	}
	swap := env.callsMatching("gh", "--remove-label ai-rework")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-done") {
		t.Errorf("want single ai-rework->ai-done swap, got: %v", swap)
	}
}

func TestReworkMissingWorktreeErrors(t *testing.T) {
	env := newFakeEnv(t)
	if err := env.orchestrator().Rework(context.Background(), 7); err == nil {
		t.Fatal("want error when no preserved worktree exists")
	}
	if len(env.callsMatching("gh", "pr create")) != 0 || len(env.callsMatching("git", "push")) != 0 {
		t.Error("missing worktree must make no destructive changes")
	}
}

func TestReworkMissingSessionErrors(t *testing.T) {
	env := newFakeEnv(t)
	// Worktree exists but no session file.
	if err := os.MkdirAll(worktreePath(env.wtDir, 7), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := env.orchestrator().Rework(context.Background(), 7); err == nil {
		t.Fatal("want error when no session file exists")
	}
	if len(env.callsMatching("gh", "pr create")) != 0 {
		t.Error("missing session must not create a PR")
	}
}

func TestClassifyCause(t *testing.T) {
	cases := []struct {
		name, errMsg, wantSub string
		wantResumable         bool
	}{
		{"session limit", "claude debug: terminated: api_error; api status 429; You've hit your session limit", "usage", true},
		{"max turns", "claude execute: terminated: max_turns", "turn", true},
		{"network down", "claude execute: exec: could not resolve host api.anthropic.com", "network", true},
		{"timeout", "claude execute: request timed out", "network", true},
		{"unknown", "git push: permission denied", "", false},
		{"panic", "panic: runtime error: index out of range", "", false},
		{"panic with transient text", "panic: nil result after client call: i/o timeout", "", false},
		{"mixed case", "API STATUS 429: Usage Limit", "usage", true},
	}
	for _, tc := range cases {
		got, resumable := classifyCause(tc.errMsg)
		if resumable != tc.wantResumable {
			t.Errorf("%s: resumable = %v, want %v", tc.name, resumable, tc.wantResumable)
		}
		if tc.wantSub == "" {
			if got != "" {
				t.Errorf("%s: want no guidance for an unrecognized cause, got %q", tc.name, got)
			}
			continue
		}
		if !strings.Contains(strings.ToLower(got), tc.wantSub) {
			t.Errorf("%s: guidance %q should mention %q", tc.name, got, tc.wantSub)
		}
	}
	// The wrapper keeps working for the park comment path.
	if failureGuidance(nil) != "" {
		t.Error("nil cause must yield no guidance")
	}
	if g := failureGuidance(fmt.Errorf("usage limit reached")); !strings.Contains(g, "usage") {
		t.Errorf("failureGuidance wrapper = %q", g)
	}
}

// TestParkCommentIncludesGuidance verifies a max_turns park explains the cause
// (turn/budget ceiling) in the GitHub comment, not just a raw error dump.
func TestParkCommentIncludesGuidance(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	err := o.park(context.Background(), 7, o.cfg.StateLabels.WIP, fmt.Errorf("claude execute: terminated: max_turns"))
	if err == nil {
		t.Fatal("park must return the cause so the caller still fails")
	}
	comments := env.callsMatching("gh", "issue comment")
	if len(comments) == 0 {
		t.Fatal("park must comment on the issue")
	}
	joined := strings.ToLower(strings.Join(comments, "\n"))
	if !strings.Contains(joined, "turn") {
		t.Errorf("park comment should classify max_turns as a turn/budget cause: %s", joined)
	}
}

// prepParked stages issue 7 as a parked, resumable issue: preserved worktree,
// saved session, recorded park cause.
func prepParked(t *testing.T, env *fakeEnv, cause string) {
	t.Helper()
	if err := os.MkdirAll(worktreePath(env.wtDir, 7), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(`{"sessionId":"s1","kind":"bug"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	recordParkCause(logDir, cause)
}

func TestReparkResumableCauseSkipsComment(t *testing.T) {
	env := newFakeEnv(t)
	prepParked(t, env, "usage limit reached")
	// The resumed claude call fails with a usage limit again.
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" {
			return claudeErrorJSON("You've hit your usage limit; resets 5pm", "s2"), "", nil
		}
		return base(c)
	}
	if err := env.orchestrator().Rework(context.Background(), 7); err == nil {
		t.Fatal("want rework failure")
	}
	if got := env.callsMatching("gh", "issue comment"); len(got) != 0 {
		t.Errorf("resumable re-park must not comment again, got %v", got)
	}
}

func TestReparkNewErrorStillComments(t *testing.T) {
	env := newFakeEnv(t)
	prepParked(t, env, "usage limit reached")
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" {
			return "", "segfault", fmt.Errorf("exit 1")
		}
		return base(c)
	}
	if err := env.orchestrator().Rework(context.Background(), 7); err == nil {
		t.Fatal("want rework failure")
	}
	if got := env.callsMatching("gh", "issue comment"); len(got) == 0 {
		t.Error("a non-resumable failure during rework is new information and must comment")
	}
}

// TestReworkRecordsSessionOnError verifies a rework that fails again (e.g. a
// fresh 429) stays parked AND updates the saved session to the latest one, so
// the next `loop -rework` resumes from where this attempt left off rather than
// the stale pre-rework session.
func TestReworkRecordsSessionOnError(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" {
			return claudeErrorJSON("You've hit your session limit", "resumed-429"), "", nil
		}
		return base(c)
	}
	seedRework(t, env, 7, "resume-me", "bug")
	if err := env.orchestrator().Rework(context.Background(), 7); err == nil {
		t.Fatal("want error so the issue stays parked as ai-rework")
	}
	si, err := readSession(filepath.Join(env.wtDir, "logs", "issue-7"))
	if err != nil {
		t.Fatalf("session must remain recorded after a failed rework: %v", err)
	}
	if si.SessionID != "resumed-429" {
		t.Errorf("session id = %q, want resumed-429 (updated to the latest session)", si.SessionID)
	}
}

// reworkHandler makes the fake gh return issue 7 as ai-rework for label
// scans, on top of newFakeEnv's defaults.
func reworkHandler(env *fakeEnv) func(rcall) (string, string, error) {
	base := env.f.handler
	return func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.HasPrefix(joined, "issue list") && strings.Contains(joined, "--label ai-rework") {
			return `[{"number": 7, "title": "Fix crash", "labels": [{"name": "ai-rework"}]}]`, "", nil
		}
		return base(c)
	}
}

func TestResumeParkedResumesAndShips(t *testing.T) {
	env := newFakeEnv(t)
	prepParked(t, env, "api status 429: usage limit")
	env.f.handler = reworkHandler(env)
	o := env.orchestrator()
	if err := resumeCycle(o); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("claude", "--resume s1")) == 0 {
		t.Error("must resume the saved session")
	}
	if len(env.callsMatching("gh", "--remove-label ai-rework")) == 0 ||
		len(env.callsMatching("gh", "--add-label ai-done")) == 0 {
		t.Error("successful resume must swap ai-rework -> ai-done")
	}
}

func TestResumeParkedSkipsNonResumable(t *testing.T) {
	env := newFakeEnv(t)
	prepParked(t, env, "git push: permission denied")
	env.f.handler = reworkHandler(env)
	if err := resumeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := env.callsMatching("claude", ""); len(got) != 0 {
		t.Errorf("non-resumable cause must not spawn claude, got %v", got)
	}
}

func TestResumeParkedSkipsMissingWorktree(t *testing.T) {
	env := newFakeEnv(t)
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	recordParkCause(logDir, "usage limit") // cause resumable, but no worktree/session
	env.f.handler = reworkHandler(env)
	if err := resumeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := env.callsMatching("claude", ""); len(got) != 0 {
		t.Errorf("missing worktree must not spawn claude, got %v", got)
	}
}

func TestResumeParkedBacksOffAfterFailure(t *testing.T) {
	env := newFakeEnv(t)
	prepParked(t, env, "usage limit reached")
	base := reworkHandler(env)
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" {
			return claudeErrorJSON("still over the usage limit", "s2"), "", nil
		}
		return base(c)
	}
	now := time.Unix(1_700_000_000, 0)
	o := env.orchestrator()
	o.now = func() time.Time { return now }

	if err := resumeCycle(o); err != nil {
		t.Fatalf("listing succeeded, want nil error, got %v", err)
	}
	first := len(env.callsMatching("claude", "--resume"))
	if first != 1 {
		t.Fatalf("claude resume calls = %d, want 1", first)
	}

	// Same instant: still inside the 5-minute backoff window.
	_ = resumeCycle(o)
	if got := len(env.callsMatching("claude", "--resume")); got != first {
		t.Errorf("resume inside backoff window: calls = %d, want %d", got, first)
	}

	// Past the window: retries.
	now = now.Add(6 * time.Minute)
	_ = resumeCycle(o)
	if got := len(env.callsMatching("claude", "--resume")); got != first+1 {
		t.Errorf("resume after backoff: calls = %d, want %d", got, first+1)
	}
}

func TestSweepOrphansRequeuesStaleWIP(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.HasPrefix(joined, "issue list") && strings.Contains(joined, "--label ai-wip") {
			return `[{"number": 7, "title": "Fix crash", "labels": [{"name": "ai-wip"}]}]`, "", nil
		}
		return base(c)
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	recordState(logDir, "ai-wip")

	if err := env.orchestrator().SweepOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ name, substr string }{
		{"git", "worktree remove --force"},
		{"git", "branch -D ai/issue-7"},
		{"gh", "--remove-label ai-wip"},
	} {
		if len(env.callsMatching(want.name, want.substr)) == 0 {
			t.Errorf("missing call %s %q", want.name, want.substr)
		}
	}
	if len(env.callsMatching("gh", "issue comment")) != 0 {
		t.Error("orphan sweep must not comment on issues")
	}
	if _, err := os.Stat(filepath.Join(logDir, "state")); !os.IsNotExist(err) {
		t.Error("sweep must clear the local state marker")
	}
}

// A crashed run that left a worktree AND a recorded session behind is resumable:
// SweepOrphans must preserve it (park for rework, worktree intact) rather than
// force-removing it and re-running the whole pipeline from zero.
func TestSweepOrphansPreservesResumableWIP(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.HasPrefix(joined, "issue list") && strings.Contains(joined, "--label ai-wip") {
			return `[{"number": 7, "title": "Fix crash", "labels": [{"name": "ai-wip"}]}]`, "", nil
		}
		return base(c)
	}
	// Simulate the crash residue: a worktree on disk and a recorded session.
	wtPath := worktreePath(env.wtDir, 7)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	recordState(logDir, "ai-wip")
	(&Claude{logDir: logDir}).RecordSession("sess-7", "bug")

	if err := env.orchestrator().SweepOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Must NOT reclaim: no worktree/branch delete.
	if len(env.callsMatching("git", "worktree remove")) != 0 || len(env.callsMatching("git", "branch -D")) != 0 {
		t.Error("resumable orphan must not have its worktree/branch removed")
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Error("resumable orphan's worktree must be preserved on disk")
	}
	// Must park for rework: ai-wip -> ai-rework, with a resumable cause recorded.
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-rework") {
		t.Errorf("want single ai-wip->ai-rework park swap, got: %v", swap)
	}
	if got := env.readLocalState(7); got != "ai-rework" {
		t.Errorf("local state = %q, want ai-rework", got)
	}
	cause := readParkCause(logDir)
	if _, resumable := classifyCause(cause); !resumable {
		t.Errorf("sweep park cause %q must be auto-resumable", cause)
	}
}

func TestSweepOrphansPropagatesListError(t *testing.T) {
	env := newFakeEnv(t)
	env.f.handler = func(c rcall) (string, string, error) {
		return "", "could not resolve host github.com", fmt.Errorf("exit 1")
	}
	o := env.orchestrator()
	if err := o.SweepOrphans(context.Background()); err == nil {
		t.Fatal("offline sweep must return an error so runLoop retries next cycle")
	}
}

func TestReworkAlreadyDoneCloses(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" {
			return claudeJSON("PIPELINE_ALREADY_DONE: nothing left to do", "d1"), "", nil
		}
		return base(c)
	}
	seedRework(t, env, 7, "resume-me", "feature")
	if err := env.orchestrator().Rework(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	swap := env.callsMatching("gh", "--remove-label ai-rework")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-done") {
		t.Errorf("want ai-rework->ai-done swap, got: %v", swap)
	}
	if len(env.callsMatching("gh", "issue close")) == 0 {
		t.Error("already-done rework must close the issue")
	}
	if len(env.callsMatching("gh", "pr create")) != 0 {
		t.Error("already-done rework must not create a PR")
	}
}

// A panic anywhere inside a single issue's pipeline must not take down the
// daemon or its sibling pipelines: the goroutine recovers, parks the issue
// (panic text is non-resumable, so it waits for a human), and releases its slot.
func TestHandleIssuePanicParksIssue(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		prompt := c.stdin
		if c.name == "claude" && !strings.Contains(prompt, "triage agent") {
			panic("pipeline bug")
		}
		return base(c)
	}
	o := env.orchestrator()
	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v, want nil (the panic is handled in the pipeline)", err)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) == 0 {
		t.Error("a panicking pipeline must park the issue for rework")
	}
	cause := readParkCause(filepath.Join(env.wtDir, "logs", "issue-7"))
	if !strings.Contains(cause, "panic") {
		t.Errorf("park cause = %q, want it to record the panic", cause)
	}
	if free := o.freeSlots(); free != 1 {
		t.Errorf("freeSlots after a panicking pipeline = %d, want 1 (slot must be released)", free)
	}
}
