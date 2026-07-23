package main

import (
	"context"
)

func RunBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent string) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "debug", Prompt: bugPrompt(issueContent, cfg.ConfidenceThreshold),
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	// Record before the error check: an errored call (e.g. a 429 session limit)
	// still returns a session id, and preserving it lets `loop -rework` resume.
	if res != nil {
		c.RecordSession(res.SessionID, "bug")
	}
	if err != nil {
		return err
	}
	// Confidence gate, shared with the feature route: same threshold, sentinel,
	// parser and terminal outcome. It runs before the already-done check on
	// purpose — a session too unsure to fix the bug must not get to close the
	// issue as already implemented instead. A threshold <= 0 disables it, and an
	// unparseable score fails open so a session that forgot the sentinel but
	// fixed the bug still ships.
	if cfg.ConfidenceThreshold > 0 {
		if score, ok := parseConfidence(res.Result); ok && score < cfg.ConfidenceThreshold {
			return &lowConfidenceError{score: score, feedback: stripConfidenceLine(res.Result)}
		}
	}
	if reason, ok := parseAlreadyDone(res.Result); ok {
		return &alreadyDoneError{reason: reason}
	}
	return nil
}

func bugPrompt(issue string, threshold int) string {
	d := promptData()
	d["Issue"] = issue
	d["Threshold"] = threshold
	return mustRender("debug.md.tmpl", d)
}
