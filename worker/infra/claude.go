package infra

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
	"time"

	"github.com/ngthluu/loope/worker/shared"
)

// Claude invokes the claude CLI headlessly — the shared.Agent adapter. logDir,
// when set, receives the raw JSON output of every call as NNN-<label>.json for
// postmortems. configDir, when set, is passed to claude as CLAUDE_CONFIG_DIR so
// the loop can run under a dedicated profile (e.g. ~/.claude-personal) instead
// of the default ~/.claude.
//
// One *Claude is shared by the concurrent sessions of a single pipeline (the UAT
// session runs alongside plan/execute), so mu guards the mutable state: the log
// sequence counter. Everything else is per-call locals or distinctly-named files.
type Claude struct {
	runner    shared.Runner
	logDir    string
	configDir string

	mu  sync.Mutex
	seq int
}

var _ shared.Agent = (*Claude)(nil)

// NewClaude builds the claude CLI adapter for one issue's log dir.
func NewClaude(r shared.Runner, logDir, configDir string) *Claude {
	return &Claude{runner: r, logDir: logDir, configDir: configDir}
}

// LogDir is the issue log directory this agent writes its artifacts to.
func (c *Claude) LogDir() string { return c.logDir }

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

func (c *Claude) Call(ctx context.Context, call shared.ClaudeCall) (*shared.ClaudeResult, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if call.Model.Model != "" {
		args = append(args, "--model", call.Model.Model)
	}
	args = append(args, effortArgs(call.Model.Effort)...)
	if call.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args, "--disallowedTools", strings.Join(append(append([]string{}, call.DisallowedTools...), noBackgroundTools...), ","))
	if call.JSONSchema != "" {
		args = append(args, "--json-schema", call.JSONSchema)
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

	env := append([]string{}, noBackgroundEnv...)
	if c.configDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+c.configDir)
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
	salvage := func() *shared.ClaudeResult {
		if ckpt == nil {
			return nil
		}
		ckpt.flush()
		if ckpt.id == "" {
			return nil
		}
		return &shared.ClaudeResult{SessionID: ckpt.id}
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
		return &res, fmt.Errorf("claude %s: %s", call.Label, res.FailureSummary())
	}
	return &res, nil
}

// A loope session is one-shot: the moment the model ends its turn the CLI
// forces the structured output and exits, and `claude -p` kills every
// background task shortly after. There is no later turn for a "you'll be
// notified when it finishes" to land in. A model that backgrounds a long test
// run and ends its turn to wait for the notification therefore reports
// "incomplete" with the work killed under it — and does the same again on
// every resume. Rather than teach this in prose (prompt-taught rules get
// forgotten on resumed turns), every call removes the ability mechanically:
//
//   - noBackgroundEnv carries the CLI's own switches. DISABLE_BACKGROUND_TASKS
//     drops run_in_background from the Bash and Agent tool schemas (and the
//     prompt text advertising it) and turns off auto-backgrounding of a
//     foreground command that outlives its timeout — so "just run it in the
//     foreground" is the only option the model is ever shown. DISABLE_CRON
//     removes the CronCreate/CronDelete/CronList tools, which "schedule a
//     prompt to run at a future time" — a future that a one-shot session does
//     not have.
//   - noBackgroundTools denies the two tools neither switch removes, whose
//     sole purpose is "end the turn and get woken later": Monitor and
//     ScheduleWakeup. Denied on every call in addition to the caller's own
//     list, so no stage can drift back into having them.
//
// Verified against claude 2.1.237: the env vars alone leave Monitor and
// ScheduleWakeup listed; --disallowedTools alone leaves run_in_background on
// Bash and the Cron tools in place. Together the model has no way to wait for,
// or be woken by, anything after its turn ends.
var noBackgroundEnv = []string{"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1", "CLAUDE_CODE_DISABLE_CRON=1"}

var noBackgroundTools = []string{"Monitor", "ScheduleWakeup"}

func effortArgs(effort string) []string {
	if effort == "" {
		return nil
	}
	return []string{"--effort", effort}
}

// nextSeq allocates the shared sequence number for a call's log files, seeding
// it from the highest NNN- prefix already present in logDir so numbering
// continues across process restarts. It scans every entry, not just .json
// postmortems: a call that failed before parseStreamResult leaves only
// NNN-label.prompt.md / .stream.jsonl behind, and seeding from the .json count
// would hand the same number out again after a restart and overwrite that
// failed call's transcript. Held under mu: concurrent sessions on one *Claude
// must not race on the counter, nor be handed the same number and overwrite
// each other's logs.
func (c *Claude) nextSeq() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seq == 0 && c.logDir != "" {
		if entries, err := os.ReadDir(c.logDir); err == nil {
			for _, e := range entries {
				if n, ok := logSeqPrefix(e.Name()); ok && n > c.seq {
					c.seq = n
				}
			}
		}
	}
	c.seq++
	return c.seq
}

// logSeqPrefix parses the leading "NNN-" of a log file name as written by
// writeLog/streamFile (%03d, so three or more digits before the first dash).
func logSeqPrefix(name string) (int, bool) {
	i := strings.IndexByte(name, '-')
	if i < 3 {
		return 0, false
	}
	n, err := strconv.Atoi(name[:i])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
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

// RecordCheckpoint writes the legacy single-record session file — see
// shared.RecordCheckpoint. Kept as a method so the adapter owns every write
// into its log dir.
func (c *Claude) RecordCheckpoint(si shared.SessionInfo) {
	shared.RecordCheckpoint(c.logDir, si)
}

// recordChainNode appends this session to the issue's lineage chain and keeps
// the legacy session file in step (the dashboard reads it). Called by Call the
// moment a checkpointed session's id streams past. A resumed round of the same
// session at the same stage is deduplicated — one session id, one checkpoint.
func (c *Claude) recordChainNode(cp shared.CallCheckpoint, id string) {
	if c.logDir == "" || id == "" {
		return
	}
	if head, ok := shared.HeadSession(c.logDir); !ok || head.ID != id || head.Stage != cp.Stage {
		shared.AppendSessionNode(c.logDir, shared.SessionNode{
			ID: id, Parent: shared.LastRealSessionID(c.logDir), Kind: cp.Kind,
			Stage: cp.Stage, Artifact: cp.Artifact, At: time.Now(),
		})
	}
	c.RecordCheckpoint(shared.SessionInfo{SessionID: id, Kind: cp.Kind, Stage: cp.Stage})
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
	shared.AppendSessionNode(c.logDir, shared.SessionNode{
		Parent: shared.LastRealSessionID(c.logDir), Kind: kind,
		Stage: stage, Artifact: artifact, At: time.Now(),
	})
}

// SetKind stamps the resolved pipeline kind onto the chain head — see
// shared.SetHeadKind.
func (c *Claude) SetKind(kind string) {
	shared.SetHeadKind(c.logDir, kind)
}

// RecordSnapshot writes the issue content this call site read to
// <logDir>/issue-snapshot — see shared.RecordSnapshot.
func (c *Claude) RecordSnapshot(content string) {
	shared.RecordSnapshot(c.logDir, content)
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
func parseStreamResult(raw string) (shared.ClaudeResult, string, error) {
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
			var res shared.ClaudeResult
			if err := json.Unmarshal([]byte(line), &res); err != nil {
				return shared.ClaudeResult{}, "", err
			}
			return res, line, nil
		}
	}
	if lastNonEmpty == "" {
		return shared.ClaudeResult{}, "", fmt.Errorf("empty stream output")
	}
	// No typed result event. This is either the single-object payload (its own
	// result, no "type") or a malformed tail; decode the last line so the former
	// works and the latter surfaces its parse error.
	var res shared.ClaudeResult
	if err := json.Unmarshal([]byte(lastNonEmpty), &res); err != nil {
		return shared.ClaudeResult{}, "", err
	}
	return res, lastNonEmpty, nil
}

// streamDetail renders a failed claude invocation's captured output for the
// error message: both streams, each labelled and explicitly marked when empty.
// claude writes its diagnostics to stdout as often as to stderr, and a bare
// "(stderr: )" told the operator nothing at all.
func streamDetail(stderr, stdout string) string {
	return "stderr: " + orEmpty(shared.Clip(strings.TrimSpace(stderr), 2000)) +
		"\nstdout: " + orEmpty(shared.Clip(strings.TrimSpace(stdout), 2000))
}

func orEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
