package main

import (
	"context"
)

func RunBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent string) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "debug", Prompt: bugPrompt(issueContent),
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
	if reason, ok := parseAlreadyDone(res.Result); ok {
		return &alreadyDoneError{reason: reason}
	}
	return nil
}

func bugPrompt(issue string) string {
	d := promptData()
	d["Issue"] = issue
	return mustRender("debug.md.tmpl", d)
}
