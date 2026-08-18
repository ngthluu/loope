package main

import (
	"context"
)

// RunBugPipeline drives one systematic-debugging session, gated on confidence
// and the already-done claim. base is the base branch: on the outcome where a
// fix was actually produced, the non-blocking UAT step diffs against
// origin/<base> to build a human-verifiable checklist for the issue body.
func RunBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT) error {
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
	return afterDebug(ctx, c, cfg, wtPath, issueContent, base, uat, res.Result)
}

// ResumeBugPipeline re-enters a persisted debug session with --resume and the
// trigger prompt instead of the fresh bugPrompt (spec §2). "debug" is the only
// stage a bug pipeline ever records, so there's no stage switch here — an
// unrecognized SessionInfo.Stage can't reach this function (handleIssue only
// calls it for session.Kind == "bug", and every bug-pipeline RecordSession call
// uses stageDebug).
func ResumeBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT, session SessionInfo, prompt string) error {
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
	return afterDebug(ctx, c, cfg, wtPath, issueContent, base, uat, res.Result)
}

// afterDebug runs the confidence gate, already-done check, and (if a fix was
// actually produced) the UAT step against a debug session's output — shared by
// the fresh and resumed entry points.
func afterDebug(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT, output string) error {
	// Confidence gate, shared with the feature route: same threshold, sentinel,
	// parser and terminal outcome. It runs before the already-done check on
	// purpose — a session too unsure to fix the bug must not get to close the
	// issue as already implemented instead. A threshold <= 0 disables it, and an
	// unparseable score fails open so a session that forgot the sentinel but
	// fixed the bug still ships.
	if cfg.ConfidenceThreshold > 0 {
		if score, ok := parseConfidence(output); ok && score < cfg.ConfidenceThreshold {
			return &lowConfidenceError{score: score, feedback: sanitizeFeedback(output)}
		}
	}
	if reason, ok := parseAlreadyDone(output); ok {
		return &alreadyDoneError{reason: reason}
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
