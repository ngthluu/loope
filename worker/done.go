package main

// alreadyDoneSentinel is printed by an architect on its own line when it
// determines the issue's work is already present in the codebase.
const alreadyDoneSentinel = "PIPELINE_ALREADY_DONE:"

// alreadyDoneError signals that a pipeline concluded the issue is already
// implemented. The orchestrator closes the issue instead of shipping a PR.
type alreadyDoneError struct{ reason string }

func (e *alreadyDoneError) Error() string { return "already implemented: " + e.reason }

// parseAlreadyDone extracts the reason following alreadyDoneSentinel. ok is
// false only when no line starts with the sentinel; an empty reason still
// counts. Line-anchored via parseSentinelLine so prose quoting the sentinel
// name mid-sentence can't close an issue as already-implemented (issue
// #73/#76).
func parseAlreadyDone(s string) (string, bool) {
	return parseSentinelLine(s, alreadyDoneSentinel)
}
