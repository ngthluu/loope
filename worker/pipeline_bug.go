package main

import (
	"context"
)

// RunBugPipeline drives one systematic-debugging session, gated on confidence
// and the already-done claim. base is the base branch: on the outcome where a
// fix was actually produced, the non-blocking UAT step diffs against
// origin/<base> to build a human-verifiable checklist for the issue body.
func RunBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT, wt *Worktree) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "debug", Prompt: bugPrompt(issueContent, cfg.ConfidenceThreshold),
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	// Record before the error check: an errored call (e.g. a 429 session limit)
	// still returns a session id, and the dashboard shows it on the parked ticket.
	if res != nil {
		c.RecordSession(res.SessionID, "bug", stageDebug)
		c.RecordSnapshot(issueContent)
	}
	if err != nil {
		return err
	}
	return afterDebug(ctx, c, cfg, wtPath, issueContent, base, uat, wt, res.Result)
}

// ResumeBugPipeline re-enters a persisted debug session with --resume and the
// trigger prompt instead of the fresh bugPrompt (spec §2). "debug" is the only
// stage a bug pipeline ever records, so there's no stage switch here — an
// unrecognized SessionInfo.Stage can't reach this function (handleIssue only
// calls it for session.Kind == "bug", and every bug-pipeline RecordSession call
// uses stageDebug).
func ResumeBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT, wt *Worktree, session SessionInfo, prompt string) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "debug-resume", Prompt: prompt, Resume: session.SessionID,
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if res != nil {
		c.RecordSession(res.SessionID, "bug", stageDebug)
		c.RecordSnapshot(issueContent)
	}
	if err != nil {
		return err
	}
	return afterDebug(ctx, c, cfg, wtPath, issueContent, base, uat, wt, res.Result)
}

// afterDebug runs the confidence gate, already-done check, and (if a fix was
// actually produced) the UAT step against a debug session's output — shared by
// the fresh and resumed entry points.
func afterDebug(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT, wt *Worktree, output string) error {
	// Confidence gate, shared with the feature route: same threshold, sentinel,
	// parser and terminal outcome. It runs before the already-done check on
	// purpose — a session too unsure to fix the bug must not get to close the
	// issue as already implemented instead. An unparseable score fails open so
	// a session that forgot the sentinel but fixed the bug still ships.
	if err := confidenceGate(cfg, output); err != nil {
		return err
	}
	if reason, ok := parseAlreadyDone(output); ok {
		return &alreadyDoneError{reason: reason}
	}
	// A debug session can also end without either sentinel — e.g. it
	// investigates and stops to ask a clarifying question instead of committing
	// a fix, despite the HEADLESS instruction not to (observed on issues #70
	// and #83, where resumed sessions re-asked their needs-info questions
	// without re-printing the CONFIDENCE sentinel). Neither gate above catches
	// that, so check the worktree directly: zero commits ahead of base means no
	// fix exists — letting that fall through to ship() parks the ticket as
	// "produced no commits" with the session's questions buried in the log.
	// Escalate to needs-info instead, with the session's output as the public
	// comment, so a human sees the questions and their answer resumes this
	// session. Fail open on a CommitCount error, same as the confidence gate.
	if wt != nil {
		if n, err := wt.CommitCount(ctx, wtPath, base); err == nil && n == 0 {
			return &lowConfidenceError{score: noConfidenceScore, feedback: sanitizeFeedback(output)}
		}
	}
	if uat == nil || uat.Target == nil {
		return nil
	}
	// Only this outcome produced a fix — neither the needs-info nor the
	// already-done return above reaches here, so neither publishes a checklist.
	uat.RunBug(ctx, c, cfg, wtPath, issueContent, base)
	return nil
}

func bugPrompt(issue string, threshold int) string {
	d := promptData()
	d["Issue"] = issue
	d["Threshold"] = threshold
	return mustRender("debug.md.tmpl", d)
}
