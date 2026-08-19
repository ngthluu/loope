// Package shared holds the telemetry wire contract between the loope worker
// (which POSTs pushes) and the telemetry server (which ingests and renders
// them). It is the only code both binaries compile in.
package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IssueDirName and ParseIssueDirName are the "issue-N" naming contract for
// per-issue directories under <WorkDir>/logs (and the matching worktree
// dirs): the worker writes them, and both the worker's tracker and the
// telemetry dashboard parse them back.
func IssueDirName(n int) string { return fmt.Sprintf("issue-%d", n) }

// ParseIssueDirName extracts N from an "issue-N" directory name; ok is false
// for any other name (e.g. the shared "triage" dir).
func ParseIssueDirName(name string) (int, bool) {
	rest, found := strings.CutPrefix(name, "issue-")
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// DefaultPushIntervalSec is the push interval assumed when a worker's
// Resource never carried one (older workers), and the default the worker's
// config applies when the telemetry block omits it.
const DefaultPushIntervalSec = 15

// UsageStaleAfter is how old a UsageSnapshot may be before both sides treat
// it as unknown: the worker stops sending it, the server stops rendering it.
const UsageStaleAfter = 30 * time.Minute

// Resource identifies the worker that produced a push: its project grouping,
// a stable machine identity, build metadata, and the push interval it is
// currently using (so the server's online-threshold calc can use "the
// interval the worker last reported" per worker, per the design doc).
type Resource struct {
	RepoSlug        string `json:"repoSlug"`
	MachineID       string `json:"machineID"`
	Hostname        string `json:"hostname"`
	WorkDir         string `json:"workDir"`
	Version         string `json:"version"`
	PushIntervalSec int    `json:"pushIntervalSec"`
}

// LogRecord is one line of the worker's daemon log.
type LogRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Body      string    `json:"body"`
}

// UsageSnapshot is the worker's most recently captured Claude Code
// rate-limit usage, read from the file the claude-usage-hook subcommand
// writes.
type UsageSnapshot struct {
	FiveHourUsedPct float64   `json:"fiveHourUsedPct"`
	FiveHourResetAt time.Time `json:"fiveHourResetAt"`
	SevenDayUsedPct float64   `json:"sevenDayUsedPct"`
	SevenDayResetAt time.Time `json:"sevenDayResetAt"`
	CapturedAt      time.Time `json:"capturedAt"`
}

// IssueLogFile is one persisted file from a worker's logs/<dir> tree.
type IssueLogFile struct {
	Name    string    `json:"name"` // e.g. "003-answer-1.output.md", "state", "session"
	Content string    `json:"content"`
	ModTime time.Time `json:"modTime"`
}

// IssueLogDir is one directory under <WorkDir>/logs — one issue's pipeline
// run ("issue-42") or the shared "triage" dir.
type IssueLogDir struct {
	Name  string         `json:"name"` // dir name as on disk: "issue-42", "triage"
	Files []IssueLogFile `json:"files"`
}

// PushRequest is the body of POST /v1/push. Usage is nil when unavailable or
// stale, so the dashboard renders "usage: unknown" instead of a fabricated
// number.
type PushRequest struct {
	Resource  Resource       `json:"resource"`
	Logs      []LogRecord    `json:"logs"`
	Usage     *UsageSnapshot `json:"usage"`
	IssueLogs []IssueLogDir  `json:"issueLogs"`
	SentAt    time.Time      `json:"sentAt"`
}

// MachineID is a stable per-(hostname,workDir) identity: sha256(hostname +
// workDir), hex-encoded and truncated to 12 characters. It survives restarts
// of the same daemon (same host, same workDir) but distinguishes two daemons
// on one host watching different repos.
func MachineID(hostname, workDir string) string {
	sum := sha256.Sum256([]byte(hostname + workDir))
	return hex.EncodeToString(sum[:])[:12]
}
