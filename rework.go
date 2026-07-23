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
// with the worktree intact, so it can be run again.
func (o *Orchestrator) Rework(ctx context.Context, n int) error {
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
	base, err := o.wt.DefaultBranch(ctx)
	if err != nil {
		return err
	}
	title, err := o.gh.IssueTitle(ctx, n)
	if err != nil {
		return err
	}

	c := &Claude{runner: o.runner, logDir: logDir, configDir: o.cfg.ClaudeConfigDir}
	res, err := c.Call(ctx, commits(ClaudeCall{
		Dir: wtPath, Label: "rework", Prompt: reworkPrompt(), Resume: si.SessionID,
		Model: o.cfg.Models.Architect,
	}))
	// Record before the error check so a rework that fails again (e.g. a fresh
	// 429) still advances the saved session to the latest one for the next run.
	if res != nil {
		c.RecordSession(res.SessionID, si.Kind)
	}
	if err != nil {
		return o.park(ctx, n, o.cfg.StateLabels.Rework, err)
	}

	branch := branchName(n)
	if reason, ok := parseAlreadyDone(res.Result); ok {
		return o.finishDone(ctx, n, wtPath, branch, o.cfg.StateLabels.Rework, reason)
	}
	return o.ship(ctx, Issue{Number: n, Title: title}, wtPath, branch, base, si.Kind, o.cfg.StateLabels.Rework)
}

func reworkPrompt() string {
	return fmt.Sprintf(`Continue the work on this issue where the previous session left off.
Complete the remaining implementation, make the full test suite pass, and commit
all changes. HEADLESS: do not ask questions; make reasonable calls and note them
in commit messages.

If you find the work is already fully implemented, do not fabricate changes:
print %s <one-sentence reason> on its own line and stop.`, alreadyDoneSentinel)
}
