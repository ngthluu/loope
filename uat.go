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
	// maxUATChars caps the checklist itself. maxIssueBodyChars keeps the resulting
	// body clear of GitHub's 65536-character issue body limit: past it, the step
	// skips rather than risk a rejected edit.
	maxUATChars       = 8000
	maxIssueBodyChars = 60000
)

// UATTarget is the GitHub surface the UAT step reads from and publishes to.
// *GitHub satisfies it; tests substitute a fake.
type UATTarget interface {
	IssueBody(ctx context.Context, n int) (string, error)
	AppendIssueBody(ctx context.Context, n int, text string) error
}

// UAT publishes a human-verifiable acceptance checklist onto the issue body.
// A nil *UAT (or one with no Target) disables the step entirely, so callers
// never need a nil guard.
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

// RunBug publishes the checklist for the bug route, from the issue content plus
// the diff the fix produced. Same non-blocking contract as RunFeature.
func (u *UAT) RunBug(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string) {
	if u == nil || u.Target == nil {
		return
	}
	u.run(ctx, c, cfg, wtPath, uatLabel, uatBugPrompt(issueContent, base))
}

// run is the whole sequence, shared by both routes: idempotency check, session,
// extract, size guard, append. Every early return logs the issue number and the
// reason, so a missing checklist is diagnosable from the daemon log alone.
func (u *UAT) run(ctx context.Context, c *Claude, cfg *Config, wtPath, label, prompt string) {
	if u == nil || u.Target == nil {
		return
	}
	// Check before spending a session. A failed fetch skips too: publishing a
	// second UAT section is worse than publishing none, and the next run on this
	// issue gets another chance.
	body, err := u.Target.IssueBody(ctx, u.Num)
	if err != nil {
		log.Printf("issue #%d: UAT skipped, issue body fetch failed: %v", u.Num, err)
		return
	}
	if strings.Contains(body, uatMarker) {
		log.Printf("issue #%d: UAT already present, skipping", u.Num)
		return
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

	section := uatSection(checklist)
	if len(body)+len(section) > maxIssueBodyChars {
		log.Printf("issue #%d: UAT skipped, the resulting issue body would exceed %d chars", u.Num, maxIssueBodyChars)
		return
	}
	if err := u.Target.AppendIssueBody(ctx, u.Num, section); err != nil {
		log.Printf("issue #%d: UAT append failed: %v", u.Num, err)
		return
	}
	log.Printf("issue #%d: UAT checklist published to the issue body", u.Num)
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

// uatSection renders the outbound issue-body section: the marker, the heading,
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
	d["UATCoverage"] = "every behavior the spec describes"
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
	d["UATCoverage"] = "the reported bug and every behavior the fix touches"
	return mustRender("uat-bug.md.tmpl", d)
}
