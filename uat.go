package main

import (
	"context"
	"log"
	"strings"
)

const (
	// uatMarker is the idempotency marker. It is an HTML comment, so it is
	// invisible in rendered markdown while still being greppable in the raw body.
	uatMarker = "<!-- loope:uat -->"
	// uatBeginSentinel / uatEndSentinel fence the checklist inside the session's
	// result text. Injected into the prompts via promptData(), never hardcoded in
	// a template.
	uatBeginSentinel = "UAT_BEGIN"
	uatEndSentinel   = "UAT_END"
	// uatLabel is the Claude call label, and so the <seq>-uat.* log file prefix.
	uatLabel = "uat"
	// maxUATChars caps the checklist itself, keeping the comment well clear of
	// GitHub's 65536-character limit.
	maxUATChars = 8000
)

// UATTarget is the GitHub surface the UAT step reads from and publishes to.
// *GitHub satisfies it; tests substitute a fake.
type UATTarget interface {
	// UATSurfaces returns every text on the issue that could carry uatMarker:
	// the comments, plus the body, where loope published the checklist before it
	// moved to a comment.
	UATSurfaces(ctx context.Context, n int) ([]string, error)
	Comment(ctx context.Context, n int, body string) error
}

// UAT publishes a human-verifiable acceptance checklist as a new issue comment,
// leaving the issue's own body — the human's report — untouched. A nil *UAT (or
// one with no Target) disables the step entirely, so callers never need a nil
// guard.
type UAT struct {
	Target UATTarget
	Num    int
}

// RunFeature publishes the checklist for the feature route, from the committed
// spec. It returns nothing: every failure path logs and continues, because a
// missing checklist must never cost a shipped feature.
func (u *UAT) RunFeature(ctx context.Context, c *Claude, cfg *Config, wtPath, specPath string) {
	if u == nil || u.Target == nil {
		return
	}
	u.run(ctx, c, cfg, wtPath, uatLabel, uatFeaturePrompt(specPath))
}

// StartFeature runs RunFeature in the background and returns the func that waits
// for it. The checklist is a side errand on a spec that is already committed:
// nothing downstream reads it, so the plan session must not sit behind a whole
// UAT session before it starts.
//
// The wait func is mandatory, not optional — the caller must call it before
// returning, because the UAT session reads the worktree and the pipeline's
// context, both of which the caller's caller tears down on return. A disabled
// UAT starts no goroutine and returns a no-op wait, so callers never need a nil
// guard.
func (u *UAT) StartFeature(ctx context.Context, c *Claude, cfg *Config, wtPath, specPath string) (wait func()) {
	if u == nil || u.Target == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		u.RunFeature(ctx, c, cfg, wtPath, specPath)
	}()
	return func() { <-done }
}

// RunBug publishes the checklist for the bug route, from the issue content plus
// the diff the fix produced. Same non-blocking contract as RunFeature.
func (u *UAT) RunBug(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string) {
	if u == nil || u.Target == nil {
		return
	}
	u.run(ctx, c, cfg, wtPath, uatLabel, uatBugPrompt(issueContent, base))
}

// run is the whole sequence, shared by both routes: idempotency check, session,
// extract, size guard, comment. Every early return logs the issue number and the
// reason, so a missing checklist is diagnosable from the daemon log alone.
func (u *UAT) run(ctx context.Context, c *Claude, cfg *Config, wtPath, label, prompt string) {
	if u == nil || u.Target == nil {
		return
	}
	// Check before spending a session. A failed fetch skips too: publishing a
	// second UAT comment is worse than publishing none, and the next run on this
	// issue gets another chance.
	surfaces, err := u.Target.UATSurfaces(ctx, u.Num)
	if err != nil {
		log.Printf("issue #%d: UAT skipped, issue fetch failed: %v", u.Num, err)
		return
	}
	for _, s := range surfaces {
		if strings.Contains(s, uatMarker) {
			log.Printf("issue #%d: UAT already present, skipping", u.Num)
			return
		}
	}

	// No RecordSession: the UAT session is ephemeral and must never overwrite the
	// resumable primary session that `loop -rework` resumes.
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: label, Prompt: prompt,
		Model:           cfg.Models.UAT,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion", "Write", "Edit", "NotebookEdit"},
	})
	if err != nil {
		log.Printf("issue #%d: UAT skipped, session failed: %v", u.Num, err)
		return
	}

	checklist, ok := parseUAT(res.Result)
	if !ok {
		log.Printf("issue #%d: UAT skipped, session produced no checklist", u.Num)
		return
	}
	if len(checklist) > maxUATChars {
		// ToValidUTF8 drops the partial rune a byte-offset cut can leave behind:
		// an invalid-UTF-8 body would be rejected by the API.
		checklist = strings.ToValidUTF8(checklist[:maxUATChars], "")
		log.Printf("issue #%d: UAT checklist truncated to %d chars", u.Num, maxUATChars)
	}

	if err := u.Target.Comment(ctx, u.Num, uatSection(checklist)); err != nil {
		log.Printf("issue #%d: UAT comment failed: %v", u.Num, err)
		return
	}
	log.Printf("issue #%d: UAT checklist published as an issue comment", u.Num)
}

// parseUAT extracts the checklist from a UAT session's result: the text after
// uatBeginSentinel, up to uatEndSentinel if present or to the end of the result
// if the session omitted it. ok is false when the begin sentinel is absent or
// the content between the sentinels is blank — both mean "nothing to publish",
// which is also how the bug route self-skips a branch with no commits.
func parseUAT(s string) (string, bool) {
	i := strings.Index(s, uatBeginSentinel)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(uatBeginSentinel):]
	if j := strings.Index(rest, uatEndSentinel); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

// uatSection renders the outbound comment: the marker, the heading,
// and the checklist. It lives with the other human-facing outbound text in
// ai/prompts/comments.md.tmpl.
func uatSection(checklist string) string {
	d := promptData()
	d["Checklist"] = checklist
	return mustRender("uat-section", d)
}

// uatFeaturePrompt drives the feature route's UAT session from the committed
// spec: the checklist describes the behavior the spec promises, and is published
// before any code exists.
func uatFeaturePrompt(specPath string) string {
	d := promptData()
	d["SpecPath"] = specPath
	return mustRender("uat-feature.md.tmpl", d)
}

// uatBugPrompt drives the bug route's UAT session from the issue plus the diff
// the fix actually produced. An empty diff means the session prints nothing,
// which parseUAT reads as "nothing to publish" — that is how the step self-skips
// a branch with no commits, with no commit-count plumbing in the pipeline.
func uatBugPrompt(issue, base string) string {
	d := promptData()
	d["Issue"] = issue
	d["Base"] = base
	return mustRender("uat-bug.md.tmpl", d)
}
