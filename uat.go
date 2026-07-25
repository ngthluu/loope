package main

import (
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
