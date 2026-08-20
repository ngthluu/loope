package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
	// Checkpoint, when set, marks this call as a primary pipeline session: the
	// moment its session id appears in the stream, Call appends a SessionNode
	// to the issue's chain (and keeps the legacy session file in step), so ANY
	// death mode — usage limit, network drop, SIGKILL, daemon crash — leaves a
	// resumable checkpoint pointing at THIS session and stage. Ephemeral calls
	// (answerer, UAT, triage, merge-resolve) leave it nil and never checkpoint.
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

// checkpointWriter tees the stream and, on the first event carrying a
// session_id, hands the id to onID exactly once. Lines may arrive split
// across Write calls, so incomplete tails are buffered; flush treats
// whatever remains as a final line (a single-object payload has no newline).
type checkpointWriter struct {
	w    io.Writer
	buf  []byte
	done bool
	id   string
	onID func(id string)
}

func (cw *checkpointWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	if cw.done {
		return n, err
	}
	cw.buf = append(cw.buf, p[:n]...)
	for {
		i := bytes.IndexByte(cw.buf, '\n')
		if i < 0 {
			break
		}
		line := cw.buf[:i]
		cw.buf = cw.buf[i+1:]
		if cw.scanLine(line) {
			return n, err
		}
	}
	return n, err
}

// flush scans the buffered final line (no trailing newline) after the stream
// ends. Safe to call once RunStream has returned on every exit path.
func (cw *checkpointWriter) flush() {
	if cw.done || len(cw.buf) == 0 {
		return
	}
	cw.scanLine(cw.buf)
}

func (cw *checkpointWriter) scanLine(line []byte) bool {
	var ev struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil || ev.SessionID == "" {
		return false
	}
	cw.done, cw.buf, cw.id = true, nil, ev.SessionID
	cw.onID(ev.SessionID)
	return true
}

func (c *Claude) Call(ctx context.Context, call ClaudeCall) (*ClaudeResult, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if call.Model.Model != "" {
		args = append(args, "--model", call.Model.Model)
	}
	args = append(args, effortArgs(call.Model.Effort)...)
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
	var ckpt *checkpointWriter
	if call.Checkpoint != nil {
		// Checkpoint in-flight: the session becomes resumable the moment its
		// id streams past, not only if the CLI survives to its result event.
		ckpt = &checkpointWriter{w: sink, onID: func(id string) { c.recordChainNode(*call.Checkpoint, id) }}
		sink = ckpt
	}
	// salvage returns the partial result a failed call can still hand back:
	// the session id seen in the stream, so callers (and the park comment)
	// keep hold of the very session the checkpoint just recorded.
	salvage := func() *ClaudeResult {
		if ckpt == nil {
			return nil
		}
		ckpt.flush()
		if ckpt.id == "" {
			return nil
		}
		return &ClaudeResult{SessionID: ckpt.id}
	}
	stderr, err := c.runner.RunStream(ctx, call.Dir, env, call.Prompt, sink, "claude", args...)
	if err != nil {
		return salvage(), fmt.Errorf("claude %s: %w\n%s", call.Label, err, streamDetail(stderr, buf.String()))
	}
	if ckpt != nil {
		ckpt.flush() // single-object payloads carry no newline for Write to scan
	}
	res, terminal, perr := parseStreamResult(buf.String())
	if perr != nil {
		return salvage(), fmt.Errorf("claude %s: parse output: %w\n%s", call.Label, perr, streamDetail(stderr, buf.String()))
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
	// SpecPath/PlanPath (worktree-relative) are set only by the pre-call stage
	// checkpoints: a stagePlan record carrying SpecPath, or a stageExecute
	// record carrying PlanPath, means that stage's session was started but
	// never returned — resume must re-run it fresh from the recorded artifact
	// instead of resuming SessionID (which belongs to the PREVIOUS stage).
	SpecPath string `json:"specPath,omitempty"`
	PlanPath string `json:"planPath,omitempty"`
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

// RecordCheckpoint writes the legacy single-record session file. The session
// CHAIN (sessions.jsonl, see sessions.go) is the primary resume source now:
// this file is kept in step by recordChainNode purely for the dashboard, and
// read back only as resumePoint's fallback for workdirs that predate the
// chain. Best-effort, like the other log-writers; skips empty session ids so
// a stray write never clobbers a recorded session.
func (c *Claude) RecordCheckpoint(si SessionInfo) {
	if c.logDir == "" || si.SessionID == "" {
		return
	}
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(si)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.logDir, sessionFile), b, 0o644)
}

// recordChainNode appends this session to the issue's lineage chain and keeps
// the legacy session file in step (the dashboard reads it). Called by Call the
// moment a checkpointed session's id streams past. A resumed round of the same
// session at the same stage is deduplicated — one session id, one checkpoint.
func (c *Claude) recordChainNode(cp CallCheckpoint, id string) {
	if c.logDir == "" || id == "" {
		return
	}
	if head, ok := headSession(c.logDir); !ok || head.ID != id || head.Stage != cp.Stage {
		appendSessionNode(c.logDir, SessionNode{
			ID: id, Parent: lastRealSessionID(c.logDir), Kind: cp.Kind,
			Stage: cp.Stage, Artifact: cp.Artifact, At: time.Now(),
		})
	}
	c.RecordCheckpoint(SessionInfo{SessionID: id, Kind: cp.Kind, Stage: cp.Stage})
}

// CheckpointStage appends a PENDING node: a stage about to start whose session
// doesn't exist yet. It covers the gap between one stage returning and the
// next session's id streaming past (a window that includes pushes and GitHub
// comments) — a crash there resumes as a fresh run of the pending stage on its
// artifact, never by re-entering the completed previous session.
func (c *Claude) CheckpointStage(kind, stage, artifact string) {
	if c.logDir == "" {
		return
	}
	appendSessionNode(c.logDir, SessionNode{
		Parent: lastRealSessionID(c.logDir), Kind: kind,
		Stage: stage, Artifact: artifact, At: time.Now(),
	})
}

// lastRealSessionID walks the chain backwards for the newest node with a real
// session id — the parent a freshly spawned session links to. Pending nodes
// (no id) are skipped: they mark intent, not a session.
func lastRealSessionID(logDir string) string {
	chain := readSessionChain(logDir)
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].ID != "" {
			return chain[i].ID
		}
	}
	return ""
}

// readSession reads the legacy SessionInfo written by RecordCheckpoint.
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
// the other log-writers: a no-op on an empty logDir or content.
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
