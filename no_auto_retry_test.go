package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A failed run must park as ai-rework and STOP. Retrying a usage/rate limit (or
// any other failure) every cycle is what burned tokens on the same issue over
// and over; the human removes the label when they want another attempt.
func TestUsageLimitParkIsNotAutoResumed(t *testing.T) {
	env := newFakeEnv(t)
	prepParked(t, env, "claude debug: terminated: api_error; api status 429; You've hit your usage limit")
	env.f.handler = reworkHandler(env)
	if err := resumeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := env.callsMatching("claude", ""); len(got) != 0 {
		t.Errorf("a failed run must not be retried automatically, got claude calls %v", got)
	}
}

func TestBudgetAndNetworkParksAreNotAutoResumed(t *testing.T) {
	for _, cause := range []string{
		"claude execute: terminated: max_turns",
		"claude execute: exec: could not resolve host api.anthropic.com",
	} {
		t.Run(cause, func(t *testing.T) {
			env := newFakeEnv(t)
			prepParked(t, env, cause)
			env.f.handler = reworkHandler(env)
			if err := resumeCycle(env.orchestrator()); err != nil {
				t.Fatal(err)
			}
			if got := env.callsMatching("claude", ""); len(got) != 0 {
				t.Errorf("cause %q must wait for a human, got claude calls %v", cause, got)
			}
		})
	}
}

// The one automatic resume that survives is the deliberate hand-off: a daemon
// restart mid-run (SweepOrphans) and the dashboard's Continue button both park
// with interruptedCause, which is not a failure.
func TestInterruptedRunIsStillAutoResumed(t *testing.T) {
	env := newFakeEnv(t)
	prepParked(t, env, interruptedCause)
	env.f.handler = reworkHandler(env)
	if err := resumeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("claude", "--resume s1")) == 0 {
		t.Error("an interrupted run must still resume its preserved session")
	}
}

// Every park comment tells the human exactly how to get another attempt: remove
// the rework label. Nothing happens until they do.
func TestParkCommentTellsUserToRemoveTheLabel(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	_ = o.park(context.Background(), 7, o.cfg.StateLabels.WIP, fmt.Errorf("claude execute: terminated: max_turns"))
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
	_ = o.park(context.Background(), 7, o.cfg.StateLabels.WIP, cause)
	joined := strings.Join(env.callsMatching("gh", "issue comment"), "\n")
	for _, want := range []string{"HEAD-MARKER", "TAIL-MARKER", "```"} {
		if !strings.Contains(joined, want) {
			t.Errorf("park comment missing %q: %s", want, joined)
		}
	}
}

// A repeated failure is new information (a different error, or the same one on a
// later attempt), so it always comments — the old silent re-park existed only to
// keep the backoff-driven auto-resume from spamming the issue.
func TestReparkAlwaysComments(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	_ = o.park(context.Background(), 7, o.cfg.StateLabels.Rework, fmt.Errorf("usage limit reached again"))
	if got := env.callsMatching("gh", "issue comment"); len(got) == 0 {
		t.Error("a re-park must still report the new error on the issue")
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
