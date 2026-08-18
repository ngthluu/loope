package main

import "strings"

// loadResumableSession reports whether logDir holds a resumable session: a
// readable, non-empty SessionInfo. A missing or corrupt session file is never
// a hard error — it just means this is a first attempt, so handleIssue falls
// through to the fresh pipeline.
func loadResumableSession(logDir string) (SessionInfo, bool) {
	si, err := readSession(logDir)
	if err != nil || si.SessionID == "" {
		return SessionInfo{}, false
	}
	return si, true
}

// diffAddedLines returns the lines in newText that aren't in oldText, in
// newText's order, joined by newlines. It's a multiset line diff, not a real
// text diff: comments are append-only from the daemon's perspective, so a new
// comment shows up as its added lines; an edited issue body is rare enough
// that treating the whole new body as "added" (because none of its lines
// matched the old body byte-for-byte) is an acceptable approximation, per the
// design doc.
func diffAddedLines(oldText, newText string) string {
	remaining := map[string]int{}
	for _, l := range strings.Split(oldText, "\n") {
		remaining[l]++
	}
	var added []string
	for _, l := range strings.Split(newText, "\n") {
		if remaining[l] > 0 {
			remaining[l]--
			continue
		}
		added = append(added, l)
	}
	return strings.Join(added, "\n")
}

// resumePrompt builds the prompt for a resumed pipeline call (spec §4). An
// issue whose local state marker was ai-needs-info immediately before this
// re-entry gets the added-lines diff of the freshly fetched content against
// the snapshot the paused session last saw; every other re-entry (rework
// removed, dashboard Continue, orphan sweep) — and a needs-info re-entry with
// nothing new to show — gets the literal "continue".
func resumePrompt(logDir, priorState, needsInfoLabel, content string) string {
	if priorState != needsInfoLabel {
		return "continue"
	}
	old, err := readSnapshot(logDir)
	if err != nil {
		return "continue"
	}
	if diff := strings.TrimSpace(diffAddedLines(old, content)); diff != "" {
		return diffAddedLines(old, content)
	}
	return "continue"
}
