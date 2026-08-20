package shared

import (
	"os"
	"path/filepath"
)

// This file holds the per-issue state markers the loop drops in an issue's log
// dir and the dashboard reads back on every disk scan. They are shared domain
// state, not infrastructure: every writer is best-effort (a failed marker
// write must never derail the transition it is only mirroring).

// StateFile is the local marker the orchestrator drops in an issue's log dir
// the instant it changes the issue's state label. It holds the raw label string
// (e.g. "ai-done"). The dashboard reads it on every disk scan so loop-driven
// transitions (picked up → WIP, shipped → Done, parked → Rework) show up live
// without re-polling GitHub, which is fetched only once.
const StateFile = "state"

// RecordState writes the issue's current state label to <logDir>/state,
// creating the dir if needed. Best-effort, like the other log-writers: a no-op
// on empty inputs and errors are swallowed.
func RecordState(logDir, label string) {
	writeMarker(logDir, StateFile, label)
}

// ReadState returns the issue's local state marker (e.g. "ai-needs-info"), or
// "" if none is recorded. Used by handleIssue to learn what state an issue was
// last in — and so which resume-prompt strategy applies (spec §4) — BEFORE it
// overwrites the marker with ai-wip for the new attempt.
func ReadState(logDir string) string {
	return readMarker(logDir, StateFile)
}

// ClearState removes the local state marker, returning the issue to whatever
// state GitHub reports (typically back to eligible). Used when the loop backs an
// issue out to be re-picked. Best-effort.
func ClearState(logDir string) {
	removeMarker(logDir, StateFile)
}

// TitleFile holds the issue's GitHub title, mirrored to disk the moment the
// loop (or the dashboard) learns it. Without it the title lives only in the
// label-scoped `gh issue list` the dashboard runs, so any issue that drops out
// of that query — a human editing its labels after it finished, a >100-result
// repo, GitHub unreachable on a fresh start — renders forever as the
// "awaiting GitHub title" placeholder even though the run is long done.
const TitleFile = "title"

// RecordTitle writes the issue's GitHub title to <logDir>/title. Best-effort,
// matching the other log-writers.
func RecordTitle(logDir, title string) {
	writeMarker(logDir, TitleFile, title)
}

// PRFile holds the issue's PR URL so the dashboard can link to it without a gh
// call.
const PRFile = "pr"

// RecordPR writes the issue's PR URL to <logDir>/pr. Best-effort, matching the
// other log-writers.
func RecordPR(logDir, url string) {
	writeMarker(logDir, PRFile, url)
}

// HasPR reports whether RecordPR has already written a PR URL for this issue,
// so ship (engine/loop.go) can tell a PR the spec stage already opened apart
// from one it still needs to create itself, and skip re-posting the PR-link
// comment.
func HasPR(logDir string) bool {
	if logDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(logDir, PRFile))
	return err == nil
}

// ParkCauseFile holds the failure text that parked the issue as ai-rework. It is
// a diagnostic left next to the logs for whoever inspects the workDir: nothing in
// the daemon reads it back, because a parked issue only moves when a human
// removes the label. It is cleared when the issue leaves the parked state, so a
// stale cause can't outlive the failure it describes.
const ParkCauseFile = "park-cause"

// RecordParkCause writes the park cause to <logDir>/park-cause. Best-effort,
// like the other log-writers.
func RecordParkCause(logDir, msg string) {
	writeMarker(logDir, ParkCauseFile, msg)
}

// ClearParkCause removes the park cause when the issue leaves the parked state.
func ClearParkCause(logDir string) {
	removeMarker(logDir, ParkCauseFile)
}

// ReadParkCause returns the recorded park cause, or "" when none exists.
func ReadParkCause(logDir string) string {
	return readMarker(logDir, ParkCauseFile)
}

func writeMarker(logDir, name, content string) {
	if logDir == "" || content == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, name), []byte(content), 0o644)
}

func readMarker(logDir, name string) string {
	b, err := os.ReadFile(filepath.Join(logDir, name))
	if err != nil {
		return ""
	}
	return string(b)
}

func removeMarker(logDir, name string) {
	if logDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(logDir, name))
}
