package main

import (
	"context"
	"fmt"
	"os"
)

// Rework resumes a parked (ai-rework) issue and ships it. It reads the preserved
// worktree and the saved Claude session, resumes that session headlessly with a
// "finish the job" prompt, then runs the shared ship step (swapping
// ai-rework->ai-done on success). Idempotent: a failure re-parks as ai-rework
// with the worktree intact, so it can be run again. It is the entry point for
// `-rework` and for ResumeParked's auto-resume.
//
// It refuses an issue under an operator hold. Without this check `loope -rework
// <N>` would drive a stopped ticket: resume honours the marker, but only once
// the multi-minute session it was meant to prevent has already been spent, and
// the ship/park that follows would swap from ai-rework — a label a stopped issue
// does not carry. Continue is the way back from stopped, and it is the one that
// lifts the hold.
func (o *Orchestrator) Rework(ctx context.Context, n int) error {
	state, err := o.currentStateLabel(ctx, n)
	if err != nil {
		return err
	}
	if state == o.cfg.StateLabels.Stopped {
		return fmt.Errorf("#%d is stopped — resume it with `loope -continue %d`", n, n)
	}
	return o.resume(ctx, n, o.cfg.StateLabels.Rework)
}

// resume resumes issue n's persisted Claude session in its preserved worktree,
// then ships. fromLabel is the state label the issue currently carries, which
// ship swaps to Done and park swaps to Rework — ai-rework for a rework, ai-wip
// for a continue. Like handleIssue it registers the run so a stop can cancel it,
// and finishes as stopped rather than parked when a stop marker is present.
func (o *Orchestrator) resume(ctx context.Context, n int, fromLabel string) error {
	wtPath := worktreePath(o.cfg.WorkDir, n)
	if _, err := os.Stat(wtPath); err != nil {
		return fmt.Errorf("issue #%d: no preserved worktree at %s to resume (remove the %s label to re-queue from scratch): %w",
			n, wtPath, o.cfg.StateLabels.Rework, err)
	}
	logDir := o.issueLogDir(n)
	si, err := readSession(logDir)
	if err != nil {
		return fmt.Errorf("issue #%d: no saved session to resume (remove the %s label to re-queue from scratch): %w",
			n, o.cfg.StateLabels.Rework, err)
	}
	if si.SessionID == "" {
		return fmt.Errorf("issue #%d: saved session file has no session id", n)
	}

	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The claim spans processes, so this also refuses a worktree a daemon (or
	// another shell's -rework) is already driving — the check prepareContinue
	// makes before it ever gets here, which rework had no equivalent of.
	if !o.registry.register(n, logDir, cancel) {
		return fmt.Errorf("#%d is already running", n)
	}
	defer o.releaseClaim(ictx, n, logDir)
	// Claim, then check for a hold — the same order as handleIssue, and for the
	// same reason (see Stop). A stop that landed while this resume was starting
	// is honoured before the Claude session, not after it.
	if stopRequested(logDir) {
		return o.finishStopped(ictx, n, fromLabel)
	}

	base, err := o.wt.DefaultBranch(ictx)
	if err != nil {
		return err
	}
	title, err := o.gh.IssueTitle(ictx, n)
	if err != nil {
		return err
	}

	c := &Claude{runner: o.runner, logDir: logDir, configDir: o.cfg.ClaudeConfigDir}
	res, err := c.Call(ictx, ClaudeCall{
		Dir: wtPath, Label: "rework", Prompt: reworkPrompt(), Resume: si.SessionID,
		Model:           o.cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
		Kind:            si.Kind,
	})
	// Record before the error check so a rework that fails again (e.g. a fresh
	// 429) still advances the saved session to the latest one for the next run.
	if res != nil {
		c.RecordSession(res.SessionID, si.Kind)
	}
	if err != nil {
		// park honours a pending stop for every caller, so a cancelled resume
		// finishes as stopped rather than parked without re-checking here.
		return o.park(ictx, n, fromLabel, err)
	}

	branch := branchName(n)
	if reason, ok := parseAlreadyDone(res.Result); ok {
		return o.finishDone(ictx, n, wtPath, branch, fromLabel, reason)
	}
	return o.ship(ictx, Issue{Number: n, Title: title}, wtPath, branch, base, si.Kind, fromLabel)
}

func reworkPrompt() string {
	return fmt.Sprintf(`Continue the work on this issue where the previous session left off.
Complete the remaining implementation, make the full test suite pass, and commit
all changes. HEADLESS: do not ask questions; make reasonable calls and note them
in commit messages.

If you find the work is already fully implemented, do not fabricate changes:
print %s <one-sentence reason> on its own line and stop.`, alreadyDoneSentinel)
}
