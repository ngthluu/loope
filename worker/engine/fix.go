package engine

// fixCommittedSentinel is printed by the merged entry session on its own line
// when it decides the issue is a small, well-scoped defect it fixed directly
// rather than escalating to a spec — the signal that derives pipeline kind
// "bug" for the issue (see entryLoop / afterFix).
const fixCommittedSentinel = "FIX_COMMITTED:"

// parseFixCommitted extracts the summary following fixCommittedSentinel. ok is
// false only when no line leads with the sentinel; an empty summary still
// counts. Line-anchored via parseSentinelLine so prose quoting the sentinel
// name mid-sentence can't terminate the entry loop (issue #73/#76).
func parseFixCommitted(s string) (string, bool) {
	return parseSentinelLine(s, fixCommittedSentinel)
}
