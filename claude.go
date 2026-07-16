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
// status (present even when Result is empty, as on a max_turns cutoff), then
// appends the result tail when there is one.
func (r ClaudeResult) failureSummary() string {
	var parts []string
	if r.TerminalReason != "" {
		parts = append(parts, "terminated: "+r.TerminalReason)
	}
	if r.APIErrorStatus != 0 {
		parts = append(parts, fmt.Sprintf("api status %d", r.APIErrorStatus))
	}
	if msg := tail(strings.TrimSpace(r.Result), 500); msg != "" {
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
type Claude struct {
	runner    Runner
	logDir    string
	configDir string
	seq       int
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
		return nil, fmt.Errorf("claude %s: %w (stderr: %s)", call.Label, err, tail(stderr, 500))
	}
	res, terminal, perr := parseStreamResult(buf.String())
	if perr != nil {
		return nil, fmt.Errorf("claude %s: parse output: %w (stdout: %s)", call.Label, perr, tail(buf.String(), 500))
	}
	c.saveLog(seq, call.Label, terminal)
	c.saveOutput(seq, call.Label, res.Result)
	if res.IsError {
		// The JSON parsed and carries a session id, so hand the result back
		// alongside the error: a session/rate limit (HTTP 429) is exactly when a
		// caller wants to persist the session so `loop -rework` can resume it.
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
// process restarts.
func (c *Claude) nextSeq() int {
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

// SessionInfo is persisted to <logDir>/session so a failed run can be resumed
// with `loop -rework`. It holds the latest primary working session for the issue.
type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
}

// RecordSession writes the latest primary working session id (and pipeline kind)
// for this issue to <logDir>/session. Best-effort, like the other log-writers:
// a no-op when logDir or id is empty, so an ephemeral answerer call (empty here
// because callers only invoke it for architect/debug/execute sessions) or a
// logless Claude never clobbers a recorded session.
func (c *Claude) RecordSession(id, kind string) {
	if c.logDir == "" || id == "" {
		return
	}
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(SessionInfo{SessionID: id, Kind: kind})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.logDir, "session"), b, 0o644)
}

// readSession reads the SessionInfo written by RecordSession from logDir.
func readSession(logDir string) (SessionInfo, error) {
	data, err := os.ReadFile(filepath.Join(logDir, "session"))
	if err != nil {
		return SessionInfo{}, err
	}
	var s SessionInfo
	if err := json.Unmarshal(data, &s); err != nil {
		return SessionInfo{}, err
	}
	return s, nil
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
