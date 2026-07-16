package main

import "strings"

// alreadyDoneSentinel is printed by an architect on its own line when it
// determines the issue's work is already present in the codebase.
const alreadyDoneSentinel = "PIPELINE_ALREADY_DONE:"

// alreadyDoneError signals that a pipeline concluded the issue is already
// implemented. The orchestrator closes the issue instead of shipping a PR.
type alreadyDoneError struct{ reason string }

func (e *alreadyDoneError) Error() string { return "already implemented: " + e.reason }

// parseAlreadyDone extracts the reason following alreadyDoneSentinel. ok is
// false only when the sentinel is absent; an empty reason still counts.
func parseAlreadyDone(s string) (string, bool) {
	i := strings.Index(s, alreadyDoneSentinel)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(alreadyDoneSentinel):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest), true
}
