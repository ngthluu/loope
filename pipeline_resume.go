package main

import "context"

// RunResumePipeline drives a rework pickup: a single Claude turn that resumes
// the exact session a prior run left off in, in its preserved worktree,
// instead of starting a fresh session that only sees the worktree's current
// contents and can mistake partial progress for a finished feature. Unlike
// RunBugPipeline and RunFeaturePipeline, it never checks for an
// already-implemented claim — that check exists to let a FRESH session bail
// out of duplicate work, and is meaningless (and actively harmful: it would
// close the issue with no PR) applied to a session that is itself mid-way
// through implementing the issue.
func RunResumePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, kind, sessionID string) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "resume", Prompt: resumePrompt(), Resume: sessionID,
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	// Record before the error check: an errored call (e.g. a 429 session limit)
	// still returns a session id, so the next rework pickup resumes THAT one
	// instead of the now-stale id this run started from.
	if res != nil {
		c.RecordSession(res.SessionID, kind)
	}
	return err
}

func resumePrompt() string {
	return mustRender("resume.md.tmpl", promptData())
}
