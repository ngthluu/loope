package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ai-rework is a TERMINAL state: the daemon must not even look at the label. A
// parked issue moves only when a human removes it, which puts it back in the
// eligible queue the normal cycle already reads. Retrying failures automatically
// is what re-ran the whole pipeline on one broken issue every cycle, burning
// tokens on work nobody was watching.
func TestDaemonNeverScansOrResumesParkedIssues(t *testing.T) {
	env := newFakeEnv(t)
	prepParked(t, env, "claude debug: terminated: api_error; api status 429; You've hit your usage limit")
	shipped := make(chan struct{}, 1)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		out, errOut, err := base(c)
		if c.name == "gh" && strings.HasPrefix(strings.Join(c.args, " "), "pr create") {
			select {
			case shipped <- struct{}{}:
			default:
			}
		}
		return out, errOut, err
	}
	o := env.orchestrator()
	// A long poll interval keeps the loop to a single cycle, so cancellation is
	// the only wake-up after that cycle's work is done.
	o.cfg.PollIntervalSec = 3600

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runLoop(ctx, o, o.cfg, true /* sweep */)
		close(done)
	}()
	select {
	case <-shipped:
	case <-time.After(10 * time.Second):
		t.Fatal("the cycle never got as far as shipping")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not return after pipelines drained")
	}

	if got := env.callsMatching("gh", "--label ai-rework"); len(got) != 0 {
		t.Errorf("a cycle must never scan the rework label, got %v", got)
	}
	if got := env.callsMatching("claude", "--resume"); len(got) != 0 {
		t.Errorf("a cycle must never resume a saved session, got %v", got)
	}
}

// The human's way forward: remove ai-rework and the issue is eligible again. The
// next run must build on the parked run's worktree rather than starting over —
// git refuses to re-add the worktree/branch, and Worktree.Create reuses what is
// on the path instead of reclaiming it.
func TestRequeuedIssueRunsInThePreservedWorktree(t *testing.T) {
	env := newFakeEnv(t)
	wtPath := worktreePath(env.wtDir, 7)
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "worktree add") {
			return "", "fatal: a branch named 'ai/issue-7' already exists", fmt.Errorf("exit 128")
		}
		return base(c)
	}

	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v", err)
	}
	// The pipeline must run in the preserved worktree, and nothing may reclaim it
	// on the way there. (ship removes it afterwards, as it does for any run that
	// lands a PR, so only removals BEFORE the pipeline count as reclaiming.)
	ranInWorktree := false
	for _, c := range env.f.calls {
		joined := strings.Join(c.args, " ")
		if c.name == "git" && strings.Contains(joined, "worktree remove") && !ranInWorktree {
			t.Fatalf("a re-queued run must reuse the preserved worktree, not reclaim it: %s", joined)
		}
		if c.name == "claude" && c.dir == wtPath {
			ranInWorktree = true
		}
	}
	if !ranInWorktree {
		t.Errorf("pipeline must run in the preserved worktree %s", wtPath)
	}
	if len(env.callsMatching("gh", "pr create")) == 0 {
		t.Error("the re-queued run must ship from the reused worktree")
	}
}

// Every park comment tells the human exactly how to get another attempt: remove
// the rework label. Nothing happens until they do.
func TestParkCommentTellsUserToRemoveTheLabel(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	_ = o.park(context.Background(), 7, fmt.Errorf("claude execute: terminated: max_turns"))
	joined := strings.Join(env.callsMatching("gh", "issue comment"), "\n")
	if !strings.Contains(joined, o.cfg.StateLabels.Rework) {
		t.Errorf("park comment must name the %s label to remove: %s", o.cfg.StateLabels.Rework, joined)
	}
	if strings.Contains(strings.ToLower(joined), "auto-resume") {
		t.Errorf("park comment must not promise an automatic resume: %s", joined)
	}
}

// The comment must carry the actual error, not a truncated generic blurb: both
// the head (which names the failing step) and the tail (which usually carries
// the API's own message) have to survive.
func TestParkCommentCarriesFullErrorDetail(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	cause := fmt.Errorf("claude execute: HEAD-MARKER\n%s\nTAIL-MARKER", strings.Repeat("detail line\n", 60))
	_ = o.park(context.Background(), 7, cause)
	joined := strings.Join(env.callsMatching("gh", "issue comment"), "\n")
	for _, want := range []string{"HEAD-MARKER", "TAIL-MARKER", "```"} {
		if !strings.Contains(joined, want) {
			t.Errorf("park comment missing %q: %s", want, joined)
		}
	}
}

// A tooling failure (git/gh) used to strip ai-wip and leave the issue eligible,
// so every cycle re-triaged and re-attempted it — the same unattended retry loop
// as a parked failure, only louder about spending triage tokens. It parks too.
func TestToolingFailureParksInsteadOfStayingEligible(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "worktree add") {
			return "", "fatal: invalid reference: origin/main", fmt.Errorf("exit 128")
		}
		return base(c)
	}
	o := env.orchestrator()
	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v", err)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) == 0 {
		t.Error("a tooling failure must park the issue as ai-rework")
	}
	joined := strings.Join(env.callsMatching("gh", "issue comment"), "\n")
	if !strings.Contains(joined, "invalid reference") {
		t.Errorf("park comment must carry the git error: %s", joined)
	}
	if got := env.readLocalState(7); got != "ai-rework" {
		t.Errorf("local state = %q, want ai-rework", got)
	}
}
