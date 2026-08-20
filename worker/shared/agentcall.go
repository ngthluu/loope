package shared

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Usage is the token accounting claude emits in its result JSON. The counts are
// summed across every turn in the call, so they measure total tokens processed,
// not the size of any single turn's context window.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	OutputTokens        int `json:"output_tokens"`
}

type ClaudeResult struct {
	Result     string  `json:"result"`
	SessionID  string  `json:"session_id"`
	IsError    bool    `json:"is_error"`
	CostUSD    float64 `json:"total_cost_usd"`
	NumTurns   int     `json:"num_turns"`
	DurationMS int     `json:"duration_ms"`
	Usage      Usage   `json:"usage"`
	// TerminalReason / APIErrorStatus explain why an is_error run stopped. They
	// are populated even when Result is empty (e.g. a max_turns cutoff), so they
	// are the reliable basis for classifying a parked issue.
	TerminalReason string `json:"terminal_reason"`
	APIErrorStatus int    `json:"api_error_status"`
}

// FailureSummary describes why an is_error result terminated, for the wrapped
// error, park comments, and logs. It leads with the terminal reason and API
// status (present even when Result is empty, as on a max_turns cutoff), adds the
// run's accounting (turns, cost, wall time — how far the session got before it
// died), then appends the result text when there is one.
//
// It is the text a human reads on the parked issue, so it errs towards saying
// too much: the result is clipped, not truncated, so a long transcript keeps
// both its opening context and its closing error message.
func (r ClaudeResult) FailureSummary() string {
	var parts []string
	if r.TerminalReason != "" {
		parts = append(parts, "terminated: "+r.TerminalReason)
	}
	if r.APIErrorStatus != 0 {
		parts = append(parts, fmt.Sprintf("api status %d", r.APIErrorStatus))
	}
	if r.NumTurns > 0 {
		parts = append(parts, fmt.Sprintf("%d turns", r.NumTurns))
	}
	if r.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", r.CostUSD))
	}
	if r.DurationMS > 0 {
		parts = append(parts, Duration(r.DurationMS))
	}
	if msg := Clip(strings.TrimSpace(r.Result), 4000); msg != "" {
		parts = append(parts, msg)
	}
	if len(parts) == 0 {
		return "session error"
	}
	return strings.Join(parts, "; ")
}

type ClaudeCall struct {
	Dir             string
	Label           string
	Prompt          string
	Model           ModelConfig
	Resume          string
	DisallowedTools []string
	SkipPermissions bool
	// Checkpoint, when set, marks this call as a primary pipeline session: the
	// moment its session id appears in the stream, Call appends a SessionNode
	// to the issue's chain (and keeps the legacy session file in step), so ANY
	// death mode — usage limit, network drop, SIGKILL, daemon crash — leaves a
	// resumable checkpoint pointing at THIS session and stage. Ephemeral calls
	// (answerer, UAT, merge-resolve) leave it nil and never checkpoint.
	Checkpoint *CallCheckpoint
}

// CallCheckpoint is the lineage metadata a primary call records: the pipeline
// kind, the stage this session IS, and the worktree-relative artifact the
// stage builds from (spec for plan, plan for execute) — the fallback a resume
// re-runs the stage from when the session itself can't be resumed.
type CallCheckpoint struct {
	Kind     string
	Stage    string
	Artifact string
}

// SessionFile holds the SessionInfo JSON. The dashboard reads it back on every
// disk scan; nothing else in the daemon does, since a session id is only ever
// surfaced, never resumed.
const SessionFile = "session"

// SessionInfo is persisted to <logDir>/session so the dashboard can show which
// Claude session did the work, and so a re-entry into the pipeline can resume
// it. It holds the latest primary session for the issue and the pipeline stage
// that session belongs to.
type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
	Stage     string `json:"stage"`
	// SpecPath/PlanPath (worktree-relative) are set only by the pre-call stage
	// checkpoints: a StagePlan record carrying SpecPath, or a StageExecute
	// record carrying PlanPath, means that stage's session was started but
	// never returned — resume must re-run it fresh from the recorded artifact
	// instead of resuming SessionID (which belongs to the PREVIOUS stage).
	SpecPath string `json:"specPath,omitempty"`
	PlanPath string `json:"planPath,omitempty"`
}

// Recognized SessionInfo.Stage values — the pipeline entry point a persisted
// session resumes into. Every recorded stage is a real Agent.Call site with a
// natural resume point (see Resume*Pipeline); there is no stage with none.
const (
	// StageEntry marks the merged entry session (engine/pipeline_entry.go): the
	// single fresh-run entry point that investigates the issue and either fixes
	// it directly (bug outcome) or writes a spec (feature outcome). Its kind is
	// unknown at checkpoint time — the engine stamps the resolved kind onto the
	// chain head (SetHeadKind) once the session's outcome sentinel is parsed.
	StageEntry = "entry"
	// StageBrainstorm and StageDebug are LEGACY: no fresh pipeline writes them
	// any more (the merged entry stage replaced both), but in-flight chains
	// checkpointed before the merge still resume through them on their original
	// routes (see engine.ResumePipeline).
	StageBrainstorm = "brainstorm"
	StageDebug      = "debug"
	StagePlan       = "plan"
	StageExecute    = "execute"
	// StageCodeReview marks the post-ship review-and-fix loop's latest session
	// (engine/codereview.go). Unlike the pipeline stages it does not resume into
	// a pipeline: handleIssue routes it straight back to ship, whose review loop
	// resumes the recorded session at the round it was cut short in.
	StageCodeReview = "codereview"
)

// RecordCheckpoint writes the legacy single-record session file. The session
// CHAIN (sessions.jsonl, see sessions.go) is the primary resume source now:
// this file is kept in step purely for the dashboard, and read back only as
// ResumePoint's fallback for workdirs that predate the chain. Best-effort,
// like the other log-writers; skips empty session ids so a stray write never
// clobbers a recorded session.
func RecordCheckpoint(logDir string, si SessionInfo) {
	if logDir == "" || si.SessionID == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(si)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, SessionFile), b, 0o644)
}

// ReadSession reads the legacy SessionInfo written by RecordCheckpoint.
func ReadSession(logDir string) (SessionInfo, error) {
	data, err := os.ReadFile(filepath.Join(logDir, SessionFile))
	if err != nil {
		return SessionInfo{}, err
	}
	var s SessionInfo
	if err := json.Unmarshal(data, &s); err != nil {
		return SessionInfo{}, err
	}
	return s, nil
}

// LoadResumableSession reports whether logDir holds a resumable session: a
// readable, non-empty SessionInfo. A missing or corrupt session file is never
// a hard error — it just means this is a first attempt, so handleIssue falls
// through to the fresh pipeline.
func LoadResumableSession(logDir string) (SessionInfo, bool) {
	si, err := ReadSession(logDir)
	if err != nil || si.SessionID == "" {
		return SessionInfo{}, false
	}
	return si, true
}

// SnapshotFile holds the exact issue content (title + body + non-bot comments,
// as FetchIssueContent produces it) the pipeline last read. It lets a resumed
// session's prompt be built from what's NEW since the paused session saw the
// issue, rather than a bare "continue" — see resumePrompt in engine/resume.go.
const SnapshotFile = "issue-snapshot"

// RecordSnapshot writes the issue content this call site read to
// <logDir>/issue-snapshot, overwriting whatever was there. Best-effort, like
// the other log-writers: a no-op on an empty logDir or content.
func RecordSnapshot(logDir, content string) {
	if logDir == "" || content == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, SnapshotFile), []byte(content), 0o644)
}

// ReadSnapshot reads the issue content written by RecordSnapshot from logDir.
func ReadSnapshot(logDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(logDir, SnapshotFile))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
