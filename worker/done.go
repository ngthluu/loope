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
// false only when no line starts with the sentinel; an empty reason still
// counts.
//
// The sentinel must lead its own line (per the prompt contract, matched here
// against the trimmed line rather than a bare substring search): an
// architect's explanation of a real fix can otherwise quote the sentinel name
// mid-sentence (e.g. "...gated on `CONFIDENCE:`/`PIPELINE_ALREADY_DONE:`
// sentinels..." while describing the gate it just fixed), which a substring
// search anywhere in the output would misread as the terminal signal and
// close the issue as already-implemented despite real commits existing (see
// issue #73/#76).
func parseAlreadyDone(s string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), alreadyDoneSentinel); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}
