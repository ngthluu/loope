package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// failureSummary describes why an is_error result terminated, for the wrapped
// error, park comments, and logs. It leads with the terminal reason and API
// status (present even when Result is empty, as on a max_turns cutoff), adds the
// run's accounting (turns, cost, wall time — how far the session got before it
// died), then appends the result text when there is one.
//
// It is the text a human reads on the parked issue, so it errs towards saying
// too much: the result is clipped, not truncated, so a long transcript keeps
// both its opening context and its closing error message.
func (r ClaudeResult) failureSummary() string {
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
		parts = append(parts, duration(r.DurationMS))
	}
	if msg := clip(strings.TrimSpace(r.Result), 4000); msg != "" {
		parts = append(parts, msg)
	}
	if len(parts) == 0 {
		return "session error"
	}
	return strings.Join(parts, "; ")
}

// Claude invokes the claude CLI headlessly. logDir, when set, receives the raw
// JSON output of every call as NNN-<label>.json for postmortems. configDir, when
// set, is passed to claude as CLAUDE_CONFIG_DIR so the loop can run under a
// dedicated profile (e.g. ~/.claude-personal) instead of the default ~/.claude.
//
// One *Claude is shared by the concurrent sessions of a single pipeline (the UAT
// session runs alongside plan/execute), so mu guards the mutable state: the log
// sequence counter. Everything else is per-call locals or distinctly-named files.
type Claude struct {
	runner    Runner
	logDir    string
	configDir string

	mu  sync.Mutex
	seq int
}

type ClaudeCall struct {
	Dir             string
	Label           string
	Prompt          string
	Model           ModelConfig
	Resume          string
	DisallowedTools []string
	SkipPermissions bool
}

func (c *Claude) Call(ctx context.Context, call ClaudeCall) (*ClaudeResult, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if call.Model.Model != "" {
		args = append(args, "--model", call.Model.Model)
	}
	args = append(args, effortArgs(call.Model.Effort)...)
	if call.Model.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(call.Model.MaxBudgetUSD, 'f', -1, 64))
	}
	if call.Model.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(call.Model.MaxTurns))
	}
	if call.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if len(call.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(call.DisallowedTools, ","))
	}
	if call.Resume != "" {
		args = append(args, "--resume", call.Resume)
	}
	// The prompt is fed on stdin, not as a positional argument: claude's
	// --disallowedTools is variadic and would otherwise swallow the prompt
	// word-by-word as bogus deny rules, and long issue bodies could exceed
	// ARG_MAX. The same bytes are persisted to <seq>-<label>.prompt.md.
	seq := c.nextSeq()
	c.savePrompt(seq, call.Label, call.Prompt)

	var env []string
	if c.configDir != "" {
		env = []string{"CLAUDE_CONFIG_DIR=" + c.configDir}
	}
	var buf bytes.Buffer
	sink := io.Writer(&buf)
	if f := c.streamFile(seq, call.Label); f != nil {
		defer f.Close()
		sink = io.MultiWriter(f, &buf)
	}
	stderr, err := c.runner.RunStream(ctx, call.Dir, env, call.Prompt, sink, "claude", args...)
	if err != nil {
		return nil, fmt.Errorf("claude %s: %w\n%s", call.Label, err, streamDetail(stderr, buf.String()))
	}
	res, terminal, perr := parseStreamResult(buf.String())
	if perr != nil {
		return nil, fmt.Errorf("claude %s: parse output: %w\n%s", call.Label, perr, streamDetail(stderr, buf.String()))
	}
	c.saveLog(seq, call.Label, terminal)
	c.saveOutput(seq, call.Label, res.Result)
	if res.IsError {
		// The JSON parsed and carries a session id, so hand the result back
		// alongside the error: a session/rate limit (HTTP 429) is exactly when a
		// caller wants to persist the session for the dashboard.
		return &res, fmt.Errorf("claude %s: %s", call.Label, res.failureSummary())
	}
	return &res, nil
}

func effortArgs(effort string) []string {
	if effort == "" {
		return nil
	}
	return []string{"--effort", effort}
}

// nextSeq allocates the shared sequence number for a call's log files, seeding
// it from the count of existing .json postmortems so numbering continues across
// process restarts. Held under mu: concurrent sessions on one *Claude must not
// race on the counter, nor be handed the same number and overwrite each other's
// logs.
func (c *Claude) nextSeq() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seq == 0 && c.logDir != "" {
		if entries, err := os.ReadDir(c.logDir); err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					c.seq++
				}
			}
		}
	}
	c.seq++
	return c.seq
}

// savePrompt persists the exact prompt fed to claude as <seq>-<label>.prompt.md
// for postmortems.
func (c *Claude) savePrompt(seq int, label, prompt string) {
	c.writeLog(seq, label, "prompt.md", prompt)
}

func (c *Claude) saveLog(seq int, label, raw string) {
	c.writeLog(seq, label, "json", raw)
}

// streamFile creates <seq>-<label>.stream.jsonl for the live transcript and
// returns it open for writing, or nil when logging is off / creation fails
// (Call then streams to its buffer only, exactly as a logless call always has).
func (c *Claude) streamFile(seq int, label string) *os.File {
	if c.logDir == "" {
		return nil
	}
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		return nil
	}
	name := fmt.Sprintf("%03d-%s.stream.jsonl", seq, label)
	f, err := os.Create(filepath.Join(c.logDir, name))
	if err != nil {
		return nil
	}
	return f
}

// saveOutput persists the model's result text as <seq>-<label>.output.md, the
// readable companion to the raw <seq>-<label>.json postmortem.
func (c *Claude) saveOutput(seq int, label, result string) {
	c.writeLog(seq, label, "output.md", result)
}

func (c *Claude) writeLog(seq int, label, ext, content string) {
	if c.logDir == "" || content == "" {
		return
	}
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		return
	}
	name := fmt.Sprintf("%03d-%s.%s", seq, label, ext)
	_ = os.WriteFile(filepath.Join(c.logDir, name), []byte(content), 0o644)
}

// sessionFile holds the SessionInfo JSON. The dashboard reads it back on every
// disk scan; nothing else in the daemon does, since a session id is only ever
// surfaced, never resumed.
const sessionFile = "session"

// SessionInfo is persisted to <logDir>/session so the dashboard can show which
// Claude session did the work, and so a re-entry into the pipeline can resume
// it. It holds the latest primary session for the issue and the pipeline stage
// that session belongs to.
type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
	Stage     string `json:"stage"`
}

// Recognized SessionInfo.Stage values — the pipeline entry point a persisted
// session resumes into. Every recorded stage is a real Claude.Call site with a
// natural resume point (see Resume*Pipeline); there is no stage with none.
const (
	stageBrainstorm = "brainstorm"
	stagePlan       = "plan"
	stageExecute    = "execute"
	stageDebug      = "debug"
	// stageCodeReview marks the post-ship review-and-fix loop's latest session
	// (codereview.go). Unlike the pipeline stages it does not resume into a
	// pipeline: handleIssue routes it straight back to ship, whose review loop
	// resumes the recorded session at the round it was cut short in.
	stageCodeReview = "codereview"
)

// RecordSession writes the latest primary working session id, pipeline kind,
// and pipeline stage for this issue to <logDir>/session. Best-effort, like the
// other log-writers: a no-op when logDir or id is empty, so an ephemeral
// answerer call (empty here because callers only invoke it for
// architect/debug/execute sessions) or a logless Claude never clobbers a
// recorded session.
func (c *Claude) RecordSession(id, kind, stage string) {
	if c.logDir == "" || id == "" {
		return
	}
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(SessionInfo{SessionID: id, Kind: kind, Stage: stage})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.logDir, sessionFile), b, 0o644)
}

// readSession reads the SessionInfo written by RecordSession from logDir.
func readSession(logDir string) (SessionInfo, error) {
	data, err := os.ReadFile(filepath.Join(logDir, sessionFile))
	if err != nil {
		return SessionInfo{}, err
	}
	var s SessionInfo
	if err := json.Unmarshal(data, &s); err != nil {
		return SessionInfo{}, err
	}
	return s, nil
}

// snapshotFile holds the exact issue content (title + body + non-bot comments,
// as FetchIssueContent produces it) the pipeline last read. It lets a resumed
// session's prompt be built from what's NEW since the paused session saw the
// issue, rather than a bare "continue" — see resumePrompt in resume.go.
const snapshotFile = "issue-snapshot"

// RecordSnapshot writes the issue content this call site read to
// <logDir>/issue-snapshot, overwriting whatever was there. Best-effort, like
// RecordSession: a no-op on an empty logDir or content.
func (c *Claude) RecordSnapshot(content string) {
	if c.logDir == "" || content == "" {
		return
	}
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.logDir, snapshotFile), []byte(content), 0o644)
}

// readSnapshot reads the issue content written by RecordSnapshot from logDir.
func readSnapshot(logDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(logDir, snapshotFile))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// parseStreamResult extracts the terminal result event from a stream-json
// transcript and returns its raw line so the caller can persist it as the .json
// postmortem in the same shape --output-format json used to produce.
//
// It scans from the end for the `{"type":"result"}` event rather than trusting
// the last non-empty line: an async hook (e.g. a SessionStart hook configured
// with async:true) emits its `{"type":"system","subtype":"hook_response"}`
// event *after* the result event, and that trailing line decodes into a
// ClaudeResult with an empty Result and is_error=false — which previously made
// a perfectly good run look like an empty, non-error success (the "no JSON
// object in output" triage failure). A single-object payload (no streaming)
// carries no "type" field and is its own result, so it falls through to the
// last-line fallback below.
func parseStreamResult(raw string) (ClaudeResult, string, error) {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	var lastNonEmpty string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if lastNonEmpty == "" {
			lastNonEmpty = line
		}
		var meta struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &meta) == nil && meta.Type == "result" {
			var res ClaudeResult
			if err := json.Unmarshal([]byte(line), &res); err != nil {
				return ClaudeResult{}, "", err
			}
			return res, line, nil
		}
	}
	if lastNonEmpty == "" {
		return ClaudeResult{}, "", fmt.Errorf("empty stream output")
	}
	// No typed result event. This is either the single-object payload (its own
	// result, no "type") or a malformed tail; decode the last line so the former
	// works and the latter surfaces its parse error.
	var res ClaudeResult
	if err := json.Unmarshal([]byte(lastNonEmpty), &res); err != nil {
		return ClaudeResult{}, "", err
	}
	return res, lastNonEmpty, nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// clip shortens s to at most max runes while keeping BOTH ends, noting how much
// was dropped. A failure's head names the failing step and its tail carries the
// API's own message, so tail() alone threw away half the diagnosis — which is
// how a parked issue ended up commented with a generic, contextless snippet.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max || max <= 0 {
		return s
	}
	head := max / 2
	tailN := max - head
	return string(r[:head]) + fmt.Sprintf("\n…[%d chars omitted]…\n", len(r)-max) + string(r[len(r)-tailN:])
}

// streamDetail renders a failed claude invocation's captured output for the
// error message: both streams, each labelled and explicitly marked when empty.
// claude writes its diagnostics to stdout as often as to stderr, and a bare
// "(stderr: )" told the operator nothing at all.
func streamDetail(stderr, stdout string) string {
	return "stderr: " + orEmpty(clip(strings.TrimSpace(stderr), 2000)) +
		"\nstdout: " + orEmpty(clip(strings.TrimSpace(stdout), 2000))
}

func orEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
