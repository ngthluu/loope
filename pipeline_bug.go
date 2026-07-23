package main

import (
	"context"
	"fmt"
)

func RunBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent string) error {
	res, err := c.Call(ctx, unattended(ClaudeCall{
		Dir: wtPath, Label: "debug", Prompt: bugPrompt(issueContent),
		Model: cfg.Models.Architect,
	}))
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
	return fmt.Sprintf(`/superpowers:systematic-debugging %s

Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
%s <one-sentence reason> on its own line and stop.`, issue, alreadyDoneSentinel)
}
