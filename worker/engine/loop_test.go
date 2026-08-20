package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// fakeEnv simulates gh/git/claude for orchestrator tests.
type fakeEnv struct {
	f          *testkit.FakeRunner
	wtDir      string
	failClaude bool // make pipeline claude calls fail
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	env := &fakeEnv{f: &testkit.FakeRunner{}, wtDir: t.TempDir()}
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		switch c.Name {
		case "gh":
			switch {
			case strings.HasPrefix(joined, "issue list"):
				return `[{"number": 7, "title": "Fix crash", "body": "boom", "labels": [{"name": "ai-agent"}]}]`, "", nil
			case strings.HasPrefix(joined, "issue view"):
				return `{"title": "Fix crash", "body": "boom", "comments": []}`, "", nil
			case strings.HasPrefix(joined, "pr create"):
				return "https://github.com/org/repo/pull/99\n", "", nil
			case strings.HasPrefix(joined, "pr view"):
				return `{"number": 99, "url": "https://github.com/org/repo/pull/99"}`, "", nil
			case strings.HasPrefix(joined, "pr review"):
				return "", "", nil
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
			if strings.Contains(c.Stdin, "/code-review") {
				return testkit.ClaudeJSON("Reviewing...\n"+codeReviewBeginSentinel+"\nSTATUS: clean\nNothing to fix.\n"+codeReviewEndSentinel, "cr1"), "", nil
			}
			if env.failClaude {
				return "", "boom", fmt.Errorf("exit 1")
			}
			return testkit.ClaudeJSON("FIX_COMMITTED: fixed and committed", "d1"), "", nil
		}
		return "", "", nil
	}
	return env
}

func (e *fakeEnv) orchestrator() *Orchestrator {
	return e.orchestratorWithLabels(testStateLabels())
}

func (e *fakeEnv) orchestratorWithLabels(sl shared.StateLabels) *Orchestrator {
	cfg := &shared.Config{
		RepoPath: "/clone", RepoSlug: "org/repo", EligibleLabel: "ai-agent",
		WorkDir: e.wtDir, MaxQARounds: 3, StateLabels: sl,
		Models: shared.Models{
			Architect: shared.ModelConfig{Model: "opus", Effort: "high"},
			Answerer:  shared.ModelConfig{Model: "sonnet"},
		},
	}
	return newTestOrch(cfg, e.f)
}

// callsMatching returns joined arg strings of calls whose name and args match.
func (e *fakeEnv) callsMatching(name, substr string) []string {
	var out []string
	for _, c := range e.f.Calls {
		joined := strings.Join(c.Args, " ")
		if c.Name == name && strings.Contains(joined, substr) {
			out = append(out, joined)
		}
	}
	return out
}

func TestProcessOnceLowConfidenceEscalatesToNeedsInfo(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		// Make the entry session escalate.
		if c.Name == "claude" && strings.HasPrefix(c.Stdin, "Handle this GitHub issue") {
			return testkit.ClaudeJSON("CONFIDENCE: 30\nNo acceptance criteria — what should the export contain?", "arch-1"), "", nil
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
	// Worktree is preserved (never-delete): a human answering needs-info must
	// resume into it, not restart from zero.
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("needs-info path must preserve the worktree, not remove it")
	}
}

func TestProcessOnceNoIssuesIsNoop(t *testing.T) {
	env := newFakeEnv(t)
	env.f.Handler = func(c testkit.RCall) (string, string, error) { return "[]", "", nil }
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.f.Calls) != 1 {
		t.Errorf("calls = %d, want only the issue list", len(env.f.Calls))
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
	} {
		if len(env.callsMatching(want.name, want.substr)) == 0 {
			t.Errorf("missing call %s %q", want.name, want.substr)
		}
	}
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("a shipped issue must preserve its worktree, not remove it")
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
	o := env.orchestratorWithLabels(shared.StateLabels{WIP: "bot-wip", Done: "bot-done"})
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
	for _, stale := range []string{"ai-wip", "ai-done"} {
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
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-rework") {
		t.Errorf("want single ai-wip->ai-rework swap, got: %v", swap)
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
	if got := shared.ReadParkCause(logDir); got == "" {
		t.Fatal("park must record the failure cause")
	}

	// A later successful run through ship() clears the stale cause.
	env2 := newFakeEnv(t)
	logDir2 := filepath.Join(env2.wtDir, "logs", "issue-7")
	shared.RecordParkCause(logDir2, "old cause")
	if err := runCycle(env2.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := shared.ReadParkCause(logDir2); got != "" {
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
// has already produced commits. It must NOT discard that work: instead the issue
// is parked for rework (ai-wip->ai-rework) with the worktree preserved, so it
// resumes rather than re-running the whole pipeline from zero next cycle.
func TestToolingFailureParksForRework(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "git" && strings.Contains(strings.Join(c.Args, " "), "push") {
			return "", "remote: protected branch hook declined", fmt.Errorf("exit 1")
		}
		return base(c)
	}
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	// It must not have swapped to a terminal state label.
	if len(env.callsMatching("gh", "--add-label ai-done")) != 0 {
		t.Error("tooling failure must not add the done label")
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
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		if c.Name == "gh" && strings.Contains(joined, "--add-label ai-done") {
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
}

// The entry session reports "already implemented" and the PO proxy confirms
// it. handleIssue must close the issue via finishDone — no PR, no push — but
// it DID take the pipeline path, so a worktree was created and ai-wip applied.
func TestProcessOnceAlreadyDoneClosesIssue(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "claude" {
			if strings.Contains(c.Stdin, "ALREADY fully implemented") {
				return testkit.ClaudeJSON("Agreed. DONE_CONFIRMED", "ans-1"), "", nil
			}
			return testkit.ClaudeJSON("PIPELINE_ALREADY_DONE: already in place", "d1"), "", nil
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
	// Worktree is preserved (never-delete), even for a closed/already-done issue.
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("already-done path must preserve the worktree, not remove it")
	}
	// It must not ship anything.
	if len(env.callsMatching("gh", "pr create")) != 0 {
		t.Error("done path must not create a PR")
	}
	if len(env.callsMatching("git", "push")) != 0 {
		t.Error("done path must not push")
	}
}

func TestFinishDoneUsesConfiguredDoneLabel(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "claude" {
			if strings.Contains(c.Stdin, "ALREADY fully implemented") {
				return testkit.ClaudeJSON("Agreed. DONE_CONFIRMED", "ans-1"), "", nil
			}
			return testkit.ClaudeJSON("PIPELINE_ALREADY_DONE: x", "d1"), "", nil
		}
		return base(c)
	}
	o := env.orchestratorWithLabels(shared.StateLabels{WIP: "bot-wip", Done: "bot-done"})
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	swap := env.callsMatching("gh", "--remove-label bot-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label bot-done") {
		t.Errorf("want single swap to configured labels, got: %v", swap)
	}
}

// A debug run that ends with zero commits and no sentinel escalates to
// needs-info with the session's questions as the comment (issues #70/#83) —
// the old outcome, parking as ai-rework with "produced no commits", buried
// the questions in the log.
func TestHandleIssueZeroCommitsEscalatesToNeedsInfo(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "git" && strings.Contains(strings.Join(c.Args, " "), "rev-list --count") {
			return "0\n", "", nil
		}
		return base(c)
	}
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil (the escalation is the observable outcome)", err)
	}
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-needs-info") {
		t.Errorf("zero commits should escalate to ai-needs-info, got: %v", swap)
	}
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("zero-commit escalation must preserve the worktree")
	}
}

// With ticketsPerCycle=2 and two eligible issues, one cycle selects and handles
// both (each in its own worktree/branch, each to its own PR). Selection is
// deterministic (oldest first); execution fans out. Run under -race to guard
// the parallel path.
func TestProcessOnceHandlesMultipleTickets(t *testing.T) {
	env := &fakeEnv{f: &testkit.FakeRunner{}, wtDir: t.TempDir()}
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		switch c.Name {
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
			return testkit.ClaudeJSON("FIX_COMMITTED: fixed", "d"), "", nil
		}
		return "", "", nil
	}
	o := env.orchestrator()
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

func TestClassifyCause(t *testing.T) {
	cases := []struct{ name, errMsg, wantSub string }{
		// Failures are explained but never acted on: the guidance is for the human
		// who has to remove the label.
		{"session limit", "claude debug: terminated: api_error; api status 429; You've hit your session limit", "usage"},
		{"max turns", "claude execute: terminated: max_turns", "turn"},
		{"network down", "claude execute: exec: could not resolve host api.anthropic.com", "network"},
		{"timeout", "claude execute: request timed out", "network"},
		{"unknown", "git push: permission denied", ""},
		{"panic", "panic: runtime error: index out of range", ""},
		{"panic with transient text", "panic: nil result after client call: i/o timeout", ""},
		{"mixed case", "API STATUS 429: Usage Limit", "usage"},
	}
	for _, tc := range cases {
		got := classifyCause(tc.errMsg)
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
}

// TestParkCommentIncludesGuidance verifies a max_turns park explains the cause
// (turn/budget ceiling) in the GitHub comment, not just a raw error dump.
func TestParkCommentIncludesGuidance(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	err := o.park(context.Background(), 7, fmt.Errorf("claude execute: terminated: max_turns"))
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
	if err := os.MkdirAll(shared.WorktreePath(env.wtDir, 7), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(`{"sessionId":"s1","kind":"bug"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	shared.RecordParkCause(logDir, cause)
}

func TestSweepOrphansRequeuesStaleWIP(t *testing.T) {
	env := newFakeEnv(t)
	env.f.Handler = wipListHandler(env)
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	shared.RecordState(logDir, "ai-wip")
	shared.RecordParkCause(logDir, "some older failure")

	if err := env.orchestrator().SweepOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A bare ai-wip removal, so the issue falls back to the eligible queue.
	rm := env.callsMatching("gh", "--remove-label ai-wip")
	if len(rm) != 1 || strings.Contains(rm[0], "--add-label") {
		t.Fatalf("want a bare ai-wip removal, got %v", rm)
	}
	if len(env.callsMatching("gh", "issue comment")) != 0 {
		t.Error("orphan sweep must not comment on issues")
	}
	if _, err := os.Stat(filepath.Join(logDir, "state")); !os.IsNotExist(err) {
		t.Error("sweep must clear the local state marker")
	}
	if got := shared.ReadParkCause(logDir); got != "" {
		t.Errorf("sweep must clear the park cause, got %q", got)
	}
}

// The sweep must never destroy what a crashed run left behind: the re-queued run
// reuses the worktree (Worktree.Create returns the existing path), so deleting it
// here would throw away the crashed run's commits and pay for the whole pipeline
// again from zero.
func TestSweepOrphansPreservesWorktreeAndSession(t *testing.T) {
	env := newFakeEnv(t)
	env.f.Handler = wipListHandler(env)
	// Simulate the crash residue: a worktree on disk and a recorded session.
	wtPath := shared.WorktreePath(env.wtDir, 7)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	shared.RecordState(logDir, "ai-wip")
	shared.RecordCheckpoint(logDir, shared.SessionInfo{SessionID: "sess-7", Kind: "bug", Stage: shared.StageDebug})

	if err := env.orchestrator().SweepOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("git", "worktree remove")) != 0 || len(env.callsMatching("git", "branch -D")) != 0 {
		t.Error("sweep must not remove the orphan's worktree or branch")
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Error("orphan's worktree must be preserved on disk")
	}
	if si, err := shared.ReadSession(logDir); err != nil || si.SessionID != "sess-7" {
		t.Errorf("session must survive the sweep, got %+v (err %v)", si, err)
	}
	// ai-rework is only ever entered by a failure, never by the sweep: an
	// interrupted run goes straight back to the eligible queue.
	if got := env.callsMatching("gh", "--add-label ai-rework"); len(got) != 0 {
		t.Errorf("sweep must not park the orphan as ai-rework, got %v", got)
	}
	if got := env.readLocalState(7); got != "" {
		t.Errorf("local state = %q, want cleared", got)
	}
}

// wipListHandler makes the fake gh return issue 7 as ai-wip for label scans, on
// top of newFakeEnv's defaults.
func wipListHandler(env *fakeEnv) func(testkit.RCall) (string, string, error) {
	base := env.f.Handler
	return func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		if c.Name == "gh" && strings.HasPrefix(joined, "issue list") && strings.Contains(joined, "--label ai-wip") {
			return `[{"number": 7, "title": "Fix crash", "labels": [{"name": "ai-wip"}]}]`, "", nil
		}
		return base(c)
	}
}

func TestSweepOrphansPropagatesListError(t *testing.T) {
	env := newFakeEnv(t)
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		return "", "could not resolve host github.com", fmt.Errorf("exit 1")
	}
	o := env.orchestrator()
	if err := o.SweepOrphans(context.Background()); err == nil {
		t.Fatal("offline sweep must return an error so runLoop retries next cycle")
	}
}

// A panic anywhere inside a single issue's pipeline must not take down the
// daemon or its sibling pipelines: the goroutine recovers, parks the issue
// (panic text is non-resumable, so it waits for a human), and releases its slot.
func TestHandleIssuePanicParksIssue(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "claude" {
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
	cause := shared.ReadParkCause(filepath.Join(env.wtDir, "logs", "issue-7"))
	if !strings.Contains(cause, "panic") {
		t.Errorf("park cause = %q, want it to record the panic", cause)
	}
	if free := o.freeSlots(); free != 1 {
		t.Errorf("freeSlots after a panicking pipeline = %d, want 1 (slot must be released)", free)
	}
}

// TestHandleIssueRecordsTitle asserts pickup mirrors the issue title into the
// log dir, so the dashboard can name the ticket after a restart without a
// label-scoped gh query returning it (issue #16).
func TestHandleIssueRecordsTitle(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	if err := runCycle(o); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(o.issueLogDir(7), shared.TitleFile))
	if err != nil {
		t.Fatalf("title not recorded: %v", err)
	}
	if strings.TrimSpace(string(body)) == "" {
		t.Fatal("recorded title is empty")
	}
}

func TestCancellationHelpers(t *testing.T) {
	o := &Orchestrator{}
	called := false
	o.setCancel(7, func() { called = true })

	// isStopping is false until a stop is flagged.
	if o.isStopping(7) {
		t.Fatal("isStopping(7) = true before any stop")
	}
	// consumeStopping is false when nothing is flagged.
	if o.consumeStopping(7) {
		t.Fatal("consumeStopping(7) = true with no flag set")
	}

	o.stopping = map[int]bool{7: true}
	if !o.isStopping(7) {
		t.Fatal("isStopping(7) = false after flag set")
	}
	if !o.consumeStopping(7) {
		t.Fatal("consumeStopping(7) = false after flag set")
	}
	// consume cleared it.
	if o.isStopping(7) {
		t.Fatal("consumeStopping did not clear the flag")
	}

	o.clearCancel(7)
	if _, ok := o.cancels[7]; ok {
		t.Fatal("clearCancel did not remove the cancel func")
	}
	_ = called
}

func TestStopFlagsAndCancelsRunningTicket(t *testing.T) {
	o := &Orchestrator{}
	cancelled := false
	o.setCancel(7, func() { cancelled = true })

	if err := o.Stop(7); err != nil {
		t.Fatalf("Stop(7) on a running ticket: %v", err)
	}
	if !cancelled {
		t.Fatal("Stop did not invoke the registered cancel func")
	}
	if !o.isStopping(7) {
		t.Fatal("Stop did not set the stopping flag")
	}
}

func TestStopNotRunningReturnsSentinel(t *testing.T) {
	o := &Orchestrator{}
	if err := o.Stop(99); err != ErrNotRunning {
		t.Fatalf("Stop(99) with nothing in flight = %v, want ErrNotRunning", err)
	}
}

func TestPauseTransitionsToStoppedAndPreservesState(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A recorded session that pause must leave untouched.
	if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(`{"sessionId":"s1","kind":"bug"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	shared.RecordState(logDir, "ai-wip")

	o.pause(context.Background(), 7)

	// ai-wip -> ai-stopped as one atomic swap.
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-stopped") {
		t.Fatalf("want single ai-wip->ai-stopped swap, got %v", swap)
	}
	if got := env.readLocalState(7); got != "ai-stopped" {
		t.Fatalf("local state = %q, want ai-stopped", got)
	}
	// Session preserved.
	if si, err := shared.ReadSession(logDir); err != nil || si.SessionID != "s1" {
		t.Fatalf("session not preserved: %+v err=%v", si, err)
	}
	// No park cause recorded — nothing auto-resumes a stopped ticket.
	if c := shared.ReadParkCause(logDir); c != "" {
		t.Fatalf("pause recorded a park cause %q, want none", c)
	}
	// A stop comment was posted.
	var commented bool
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "Stopped by user") {
			commented = true
		}
	}
	if !commented {
		t.Fatal("pause did not comment the stop notice")
	}
}

func TestStopDuringPipelineParksAsStopped(t *testing.T) {
	env := newFakeEnv(t) // issue 7 eligible; pipeline succeeds unless stopped
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	go func() { _ = o.ProcessOnce(context.Background()) }()
	n := awaitStarted(t, started, 1)[0]

	if err := o.Stop(n); err != nil {
		t.Fatalf("Stop(%d): %v", n, err)
	}
	close(release) // let the gated claude call return
	o.Wait()

	// The run ends in ai-stopped, not shipped.
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-stopped") {
		t.Fatalf("want ai-wip->ai-stopped swap, got %v", swap)
	}
	if got := env.readLocalState(n); got != "ai-stopped" {
		t.Fatalf("local state = %q, want ai-stopped", got)
	}
	// Ship was skipped: no PR created.
	if pr := env.callsMatching("gh", "pr create"); len(pr) != 0 {
		t.Fatalf("a stopped run must not ship a PR, got %v", pr)
	}
}

// Continue always re-queues to the eligible state, session or no session: the
// daemon has no resume path any more, so a stopped ticket goes back through the
// normal cycle — which reuses the preserved worktree.
func TestContinueRequeuesEligible(t *testing.T) {
	for _, withSession := range []bool{true, false} {
		name := "no session"
		if withSession {
			name = "saved session"
		}
		t.Run(name, func(t *testing.T) {
			env := newFakeEnv(t)
			o := env.orchestrator()
			logDir := filepath.Join(env.wtDir, "logs", "issue-7")
			if err := os.MkdirAll(logDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if withSession {
				shared.RecordCheckpoint(logDir, shared.SessionInfo{SessionID: "s1", Kind: "bug", Stage: shared.StageDebug})
			}
			shared.RecordState(logDir, "ai-stopped")
			shared.RecordParkCause(logDir, "whatever")

			if err := o.Continue(context.Background(), 7); err != nil {
				t.Fatalf("Continue: %v", err)
			}
			// ai-stopped removed (not swapped) so the ticket falls back to eligible.
			rm := env.callsMatching("gh", "--remove-label ai-stopped")
			if len(rm) != 1 || strings.Contains(rm[0], "--add-label") {
				t.Fatalf("want a bare ai-stopped removal, got %v", rm)
			}
			if got := env.readLocalState(7); got != "" {
				t.Fatalf("local state = %q, want cleared", got)
			}
			if c := shared.ReadParkCause(logDir); c != "" {
				t.Fatalf("park cause = %q, want cleared", c)
			}
		})
	}
}

func TestContinueWhileRunningReturnsSentinel(t *testing.T) {
	o := &Orchestrator{active: map[int]struct{}{7: {}}}
	if err := o.Continue(context.Background(), 7); err != ErrAlreadyRunning {
		t.Fatalf("Continue while running = %v, want ErrAlreadyRunning", err)
	}
}

func TestPauseSwapFailureLeavesStateUnchanged(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	// Make the ai-wip->ai-stopped swap fail — either GitHub is unreachable, or the
	// ticket already left ai-wip (a ship/park won the stop's narrow race window).
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		if c.Name == "gh" && strings.Contains(joined, "--remove-label ai-wip") && strings.Contains(joined, "--add-label ai-stopped") {
			return "", "label ai-wip not found", fmt.Errorf("exit 1")
		}
		return base(c)
	}
	o := env.orchestrator()
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shared.RecordState(logDir, "ai-wip")

	o.pause(context.Background(), 7)

	// The swap failed, so local state must NOT flip to ai-stopped: recording it
	// would diverge from the real label — the dashboard would show stopped while
	// GitHub still reads ai-wip, and SweepOrphans could act on the "stopped" ticket.
	if got := env.readLocalState(7); got != "ai-wip" {
		t.Fatalf("local state = %q, want unchanged ai-wip after a failed swap", got)
	}
	// And no spurious stop notice on a ticket that never actually stopped.
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "Stopped by user") {
			t.Fatalf("posted a stop notice despite the swap failing: %v", c)
		}
	}
}

func TestStopFlagConsumedWhenPipelinePanics(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	// Blow up while handleIssue applies the ai-wip label, simulating a pipeline
	// that panics mid-run after a Stop was already flagged for the ticket.
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		if c.Name == "gh" && strings.Contains(joined, "issue edit") && strings.Contains(joined, "--add-label ai-wip") {
			panic("boom mid-pipeline")
		}
		return base(c)
	}
	o := env.orchestrator()
	o.stopping = map[int]bool{7: true} // a Stop landed just before the panic

	_ = o.ProcessOnce(context.Background())
	o.Wait()

	// The panic's recover handler must consume the stop flag. A leaked flag would
	// make isStopping(7) true forever and spuriously stop issue 7's very next run.
	if o.isStopping(7) {
		t.Fatal("stopping flag leaked after a panic: issue 7's next run would be wrongly stopped")
	}
	// The panic outcome wins over the pending stop: the issue is parked for a human.
	if got := env.readLocalState(7); got != "ai-rework" {
		t.Fatalf("local state = %q, want ai-rework (parked after panic)", got)
	}
}

// TestHandleIssueResumesPersistedFeatureSession simulates a rework-then-removed
// re-entry: a first cycle parks the issue with a recorded brainstorm session,
// then a second cycle (the label removed, same worktree/logs preserved) must
// call the architect with --resume and prompt "continue" instead of
// brainstorm-0.
func TestHandleIssueResumesPersistedFeatureSession(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "claude" && strings.HasPrefix(c.Stdin, "Handle this GitHub issue") {
			// First (fresh) attempt fails outright (e.g. a session-limit 429) but
			// still returns a session id, so the issue parks with a recorded
			// session and the worktree preserved.
			return testkit.ClaudeErrorJSON("session limit hit", "arch-sess"), "", nil
		}
		return base(c)
	}
	o := env.orchestrator()
	if err := runCycle(o); err != nil {
		t.Fatalf("park is a clean cycle outcome, got %v", err)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) == 0 {
		t.Fatal("setup: first attempt must park as ai-rework")
	}

	// Second cycle: the entry session now resumes instead of failing, and the
	// issue is still listed as eligible (no state-label filtering in this
	// harness, mirroring "label removed -> eligible again").
	env.f.Handler = base
	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	var resumed bool
	for _, call := range env.f.Calls {
		if call.Name == "claude" && strings.HasPrefix(call.Stdin, "continue") && testkit.ArgAfter(call.Args, "--resume") != "" {
			resumed = true
		}
	}
	if !resumed {
		t.Error("second attempt must resume the architect session with --resume and a prompt leading with \"continue\", not restart brainstorm-0")
	}
	// The park-then-resume cycle must never have deleted the worktree.
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("resuming must never have deleted the worktree along the way")
	}
}

// TestHandleIssueResumesWithDiffAfterNeedsInfo simulates the ai-needs-info
// answered trigger: the first cycle escalates to needs-info (recording the
// snapshot and a brainstorm session); the second cycle, with a new human
// comment in the fetched issue content, must resume with a prompt containing
// the diffed comment, not the bare literal "continue".
func TestHandleIssueResumesWithDiffAfterNeedsInfo(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "claude" && strings.HasPrefix(c.Stdin, "Handle this GitHub issue") {
			return testkit.ClaudeJSON("CONFIDENCE: 30\nNo acceptance criteria — what should the export contain?", "arch-1"), "", nil
		}
		return base(c)
	}
	o := env.orchestrator()
	o.cfg.ConfidenceThreshold = 70
	if err := runCycle(o); err != nil {
		t.Fatalf("needs-info is a clean outcome, want nil error, got %v", err)
	}

	// Second cycle: the issue now carries a human's answer in its comments, and
	// the entry session resumes instead of scoring low again.
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		if c.Name == "gh" && strings.HasPrefix(joined, "issue view") {
			return `{"title": "Fix crash", "body": "boom", "comments": [{"author": {"login": "alice"}, "body": "export should include CSV rows only"}]}`, "", nil
		}
		return base(c)
	}
	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	var diffPrompt string
	for _, call := range env.f.Calls {
		if call.Name == "claude" && testkit.ArgAfter(call.Args, "--resume") == "arch-1" {
			diffPrompt = call.Stdin
		}
	}
	if diffPrompt == "" {
		t.Fatal("want a resumed architect call with --resume arch-1")
	}
	if diffPrompt == "continue" || !strings.Contains(diffPrompt, "export should include CSV rows only") {
		t.Errorf("resume prompt = %q, want the diffed new comment, not a bare continue", diffPrompt)
	}
}

// TestHandleIssueNoSessionUsesFreshPath is the control: an issue with no
// session file at all (first-ever attempt) must call brainstorm-0/debug
// exactly as today, with no --resume anywhere.
func TestHandleIssueNoSessionUsesFreshPath(t *testing.T) {
	env := newFakeEnv(t)
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	for _, call := range env.f.Calls {
		if call.Name == "claude" && testkit.ArgAfter(call.Args, "--resume") != "" {
			t.Errorf("a first-ever attempt must never use --resume, got call: %+v", call)
		}
	}
}

// TestFinishDoneAndNeedsInfoPreserveBranch is a direct regression test for the
// spec's "never delete" rule: finishDone and finishNeedsInfo must not delete
// the branch any more than they delete the worktree.
func TestFinishDoneAndNeedsInfoPreserveBranch(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "claude" {
			if strings.Contains(c.Stdin, "ALREADY fully implemented") {
				return testkit.ClaudeJSON("Agreed. DONE_CONFIRMED", "ans-1"), "", nil
			}
			return testkit.ClaudeJSON("PIPELINE_ALREADY_DONE: already in place", "d1"), "", nil
		}
		return base(c)
	}
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("git", "branch -D")) != 0 {
		t.Error("finishDone must not delete the branch")
	}
}

// TestSweepOrphansThenNextCycleResumesSession is the daemon-restart case (spec
// §5): an issue stranded in ai-wip by a crash, with a session already recorded
// from before the crash, must have that session resumed on the very next cycle
// after SweepOrphans requeues it — with no bespoke "was this a restart" signal
// anywhere, just the ordinary loadResumableSession check in handleIssue.
func TestSweepOrphansThenNextCycleResumesSession(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()

	// Simulate a crash: ai-wip is set, and a brainstorm session was recorded
	// before the process died mid-pipeline.
	n := 7
	if err := o.gh.AddLabel(context.Background(), n, o.cfg.StateLabels.WIP); err != nil {
		t.Fatal(err)
	}
	logDir := o.issueLogDir(n)
	c := infra.NewClaude(nil, logDir, "")
	c.RecordCheckpoint(shared.SessionInfo{SessionID: "stranded-sess", Kind: "feature", Stage: shared.StageBrainstorm})
	c.RecordSnapshot("# Fix crash (#7)\n\nboom\n")

	if err := o.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if len(env.callsMatching("gh", "--remove-label ai-wip")) == 0 {
		t.Fatal("setup: SweepOrphans must strip the stale ai-wip label")
	}

	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	var resumed bool
	for _, call := range env.f.Calls {
		if call.Name == "claude" && testkit.ArgAfter(call.Args, "--resume") == "stranded-sess" {
			resumed = true
		}
	}
	if !resumed {
		t.Error("the next cycle after a sweep must resume the crash-stranded session, not restart brainstorm-0")
	}
}

// TestShipSkipsCodeReviewWhenNotConfigured verifies an absent
// Models.CodeReview block makes ship() behave exactly as before: no PR-number
// lookup, no review comment.
func TestShipSkipsCodeReviewWhenNotConfigured(t *testing.T) {
	env := newFakeEnv(t)
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "pr view")) != 0 || len(env.callsMatching("gh", "pr review")) != 0 {
		t.Error("code review must make no gh calls when Models.CodeReview is nil")
	}
	if env.readLocalState(7) != "ai-done" {
		t.Errorf("state = %q, want ai-done", env.readLocalState(7))
	}
}

// TestShipRunsCodeReviewWhenConfigured verifies ship() invokes the post-ship
// code review loop and still reaches ai-done.
func TestShipRunsCodeReviewWhenConfigured(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	o.cfg.Models.CodeReview = &shared.CodeReviewConfig{ModelConfig: shared.ModelConfig{Model: "sonnet"}, Rounds: 1}
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "pr review")) != 1 {
		t.Errorf("want exactly one pr review call, got %v", env.callsMatching("gh", "pr review"))
	}
	if env.readLocalState(7) != "ai-done" {
		t.Errorf("state = %q, want ai-done even with code review enabled", env.readLocalState(7))
	}
}

// TestShipParksWhenCodeReviewErrors verifies a code-review loop failure
// (here, the PR lookup fails) parks the issue for a human instead of marking
// it done: code review is a session stage, and its failures wait in
// ai-rework like any other error while the PR itself stays up.
func TestShipParksWhenCodeReviewErrors(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	o.cfg.Models.CodeReview = &shared.CodeReviewConfig{ModelConfig: shared.ModelConfig{Model: "sonnet"}, Rounds: 1}
	orig := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		if c.Name == "gh" && strings.HasPrefix(joined, "pr view") {
			return "", "no pull requests found", fmt.Errorf("exit 1")
		}
		return orig(c)
	}
	_ = runCycle(o) // the park cause propagates out of the cycle; labels are what matter
	if env.readLocalState(7) != "ai-rework" {
		t.Errorf("state = %q, want ai-rework when the code review loop fails", env.readLocalState(7))
	}
}

// TestShipSkipsPRCommentWhenAlreadyRecorded is spec §3: when the spec stage
// already created the PR and posted the link comment (hasPR is true), ship
// must still run CommitCount/Push/CreatePR/the label swap, but skip
// re-posting the comment and re-writing the pr file.
func TestShipSkipsPRCommentWhenAlreadyRecorded(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	logDir := o.issueLogDir(7)
	shared.RecordPR(logDir, "https://github.com/org/repo/pull/99")
	issue := shared.Issue{Number: 7, Title: "Fix crash"}
	c := infra.NewClaude(env.f, logDir, "")
	if err := o.ship(context.Background(), issue, c, shared.WorktreePath(o.cfg.WorkDir, 7), shared.BranchName(7), "main", "feature"); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "pr create")) == 0 {
		t.Error("ship must still call CreatePR to resolve the canonical URL for the label swap")
	}
	prComments := 0
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "pull/99") {
			prComments++
		}
	}
	if prComments != 0 {
		t.Error("ship must not re-post the PR-link comment when hasPR is already true")
	}
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-done") {
		t.Errorf("want the wip->done swap to still run, got: %v", swap)
	}
}

// TestProcessOnceFeatureOpensPRAfterSpecStage is the spec's required
// end-to-end check: a full feature-pipeline run (brainstorm -> spec -> plan
// -> execute -> ship) must have a PR open, and commented, right after the
// spec stage — before plan or execute run at all — and only ONE "🤖 PR:"
// comment must exist on the issue by the time the whole run finishes (ship
// must not re-announce the PR the spec stage already announced).
func TestProcessOnceFeatureOpensPRAfterSpecStage(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.Handler
	var prCreatedBeforePlan bool
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "claude" && strings.HasPrefix(c.Stdin, "Handle this GitHub issue") {
			writeSpecFile(t, shared.WorktreePath(env.wtDir, 7))
			return testkit.ClaudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		}
		if c.Name == "claude" && strings.Contains(c.Stdin, "writing-plans") {
			for _, call := range env.f.Calls {
				if call.Name == "gh" && strings.Contains(strings.Join(call.Args, " "), "pr create") {
					prCreatedBeforePlan = true
				}
			}
			writePlanFile(t, shared.WorktreePath(env.wtDir, 7))
			return testkit.ClaudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		}
		if c.Name == "claude" && strings.Contains(c.Stdin, "executing-plans") {
			return testkit.ClaudeJSON("Executed.", "exec-1"), "", nil
		}
		return base(c)
	}
	o := env.orchestrator()
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !prCreatedBeforePlan {
		t.Error("the PR was not created before the plan session ran")
	}
	prComments := 0
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "pull/99") {
			prComments++
		}
	}
	if prComments != 1 {
		t.Errorf("want exactly one PR-link comment across the whole run, got %d", prComments)
	}
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-done") {
		t.Errorf("want a single ai-wip->ai-done swap, got: %v", swap)
	}
}

// TestHandleIssueRoutesCodeReviewStageToShip: a persisted codereview-stage
// session (review parked, or a restart mid-review) re-enters through ship —
// never through the pipelines — so the finished execute/debug session is not
// re-resumed, no duplicate PR comment appears, and the review loop continues
// its recorded session to ai-done.
func TestHandleIssueRoutesCodeReviewStageToShip(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	o.cfg.Models.CodeReview = &shared.CodeReviewConfig{ModelConfig: shared.ModelConfig{Model: "sonnet"}, Rounds: 1}
	logDir := o.issueLogDir(7)
	shared.RecordPR(logDir, "https://github.com/org/repo/pull/99")
	shared.RecordCheckpoint(logDir, shared.SessionInfo{SessionID: "cr-parked", Kind: "feature", Stage: shared.StageCodeReview})
	if err := os.MkdirAll(shared.WorktreePath(env.wtDir, 7), 0o755); err != nil {
		t.Fatal(err)
	}
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "claude" && (strings.HasPrefix(c.Stdin, "Handle this GitHub issue") ||
			strings.Contains(c.Stdin, "/superpowers:writing-plans") || strings.Contains(c.Stdin, "/superpowers:executing-plans")) {
			t.Errorf("a codereview-stage re-entry must never run a pipeline session, got prompt: %.60s", c.Stdin)
		}
		return base(c)
	}
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	resumed := false
	for _, call := range env.f.Calls {
		if call.Name == "claude" && testkit.ArgAfter(call.Args, "--resume") == "cr-parked" {
			resumed = true
			if call.Stdin != "continue" {
				t.Errorf("resumed review prompt = %q, want \"continue\"", call.Stdin)
			}
		}
	}
	if !resumed {
		t.Error("want the parked codereview session resumed via --resume cr-parked")
	}
	prComments := 0
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "pull/99") {
			prComments++
		}
	}
	if prComments != 0 {
		t.Errorf("re-entry must not re-post the PR comment, got %d", prComments)
	}
	if env.readLocalState(7) != "ai-done" {
		t.Errorf("state = %q, want ai-done after the resumed review finishes", env.readLocalState(7))
	}
}
