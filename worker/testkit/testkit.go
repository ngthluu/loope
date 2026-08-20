// Package testkit holds the fake port implementations and helpers shared by
// the test suites of every worker package. It is imported by _test.go files
// only — production code never depends on it.
package testkit

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/ngthluu/loope/worker/shared"
)

// RCall records one process invocation a FakeRunner received.
type RCall struct {
	Dir   string
	Env   []string
	Stdin string
	Name  string
	Args  []string
}

// RResp is one queued response a FakeRunner hands back.
type RResp struct {
	Stdout string
	Stderr string
	Err    error
}

// FakeRunner is the shared.Runner test double: it records every call.
// Responses come from Handler if set, otherwise popped from Queue in order
// (an empty queue returns success).
type FakeRunner struct {
	mu      sync.Mutex
	Calls   []RCall
	Queue   []RResp
	Handler func(RCall) (string, string, error)
}

var _ shared.Runner = (*FakeRunner)(nil)

func (f *FakeRunner) Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := RCall{Dir: dir, Env: env, Stdin: stdin, Name: name, Args: args}
	f.Calls = append(f.Calls, c)
	if f.Handler != nil {
		return f.Handler(c)
	}
	if len(f.Queue) == 0 {
		return "", "", nil
	}
	r := f.Queue[0]
	f.Queue = f.Queue[1:]
	return r.Stdout, r.Stderr, r.Err
}

// RunStream mirrors Run for the streaming seam: it records the call and pulls a
// response from Handler/Queue exactly as Run does, then writes that response's
// stdout to w so callers parsing a stream (Claude.Call) see it. This lets
// tests keep queueing a single ClaudeJSON payload — it becomes the stream's
// one (terminal) line.
func (f *FakeRunner) RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := RCall{Dir: dir, Env: env, Stdin: stdin, Name: name, Args: args}
	f.Calls = append(f.Calls, c)
	var out, stderr string
	var err error
	if f.Handler != nil {
		out, stderr, err = f.Handler(c)
	} else if len(f.Queue) > 0 {
		r := f.Queue[0]
		f.Queue = f.Queue[1:]
		out, stderr, err = r.Stdout, r.Stderr, r.Err
	}
	if out != "" {
		_, _ = io.WriteString(w, out)
	}
	return stderr, err
}

// HasArg reports whether args contains want.
func HasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// ArgAfter returns the argument following flag, or "".
func ArgAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// ClaudeJSON builds a fake `claude -p --output-format json` stdout payload.
func ClaudeJSON(result, session string) string {
	b, _ := json.Marshal(map[string]any{
		"result": result, "session_id": session, "is_error": false, "total_cost_usd": 0.5,
	})
	return string(b)
}

// ClaudeStructured builds a fake claude payload whose terminal event carries a
// schema-validated structured_output object, as a --json-schema call produces.
// result mirrors the real CLI, whose result text is the JSON-encoded object.
func ClaudeStructured(session string, structured map[string]any) string {
	so, _ := json.Marshal(structured)
	b, _ := json.Marshal(map[string]any{
		"result": string(so), "session_id": session, "is_error": false, "total_cost_usd": 0.5,
		"structured_output": structured,
	})
	return string(b)
}

// ClaudeEntry builds a fake entry-turn payload: outcome plus optional detail.
func ClaudeEntry(session, outcome, detail string) string {
	return ClaudeStructured(session, map[string]any{"outcome": outcome, "detail": detail})
}

// ClaudeEntryConfidence is ClaudeEntry with the opening turn's confidence score.
func ClaudeEntryConfidence(session, outcome, detail string, confidence int) string {
	return ClaudeStructured(session, map[string]any{"outcome": outcome, "detail": detail, "confidence": confidence})
}

// ClaudeSpecReady builds a fake entry-turn payload reporting a committed spec.
func ClaudeSpecReady(session, path string) string {
	return ClaudeStructured(session, map[string]any{"outcome": "spec_ready", "detail": "spec committed", "spec_path": path})
}

// ClaudePlanReady builds a fake plan-session payload reporting the plan ready.
func ClaudePlanReady(session string) string {
	return ClaudeStructured(session, map[string]any{"status": "ready"})
}

// ClaudeExecuteComplete builds a fake execute-session payload reporting every
// plan task implemented and committed.
func ClaudeExecuteComplete(session string) string {
	return ClaudeStructured(session, map[string]any{"status": "complete"})
}

// ClaudeAnswer builds a fake answerer payload carrying a reply.
func ClaudeAnswer(session, answer string) string {
	return ClaudeStructured(session, map[string]any{"has_answer": true, "answer": answer})
}

// ClaudeNothingToAnswer builds a fake answerer payload for a status update.
func ClaudeNothingToAnswer(session string) string {
	return ClaudeStructured(session, map[string]any{"has_answer": false})
}

// ClaudeEphemeral builds a catch-all payload for the ephemeral sessions a flow
// test does not script individually (answerer, UAT). It carries every field
// those parsers read, so whichever one receives it decodes cleanly.
func ClaudeEphemeral(session, answer string) string {
	return ClaudeStructured(session, map[string]any{"has_answer": true, "answer": answer, "checklist": ""})
}

// ClaudeDoneConfirm builds a fake done-confirm payload: a confirmation when
// objection is "", an objection otherwise.
func ClaudeDoneConfirm(session, objection string) string {
	return ClaudeStructured(session, map[string]any{"confirmed": objection == "", "objection": objection})
}

// ClaudeErrorJSON builds a fake claude payload that reports an error but still
// carries a valid session id — e.g. a session/rate limit (HTTP 429). This is
// exactly the case where we most want to preserve the session for -rework.
func ClaudeErrorJSON(result, session string) string {
	b, _ := json.Marshal(map[string]any{
		"result": result, "session_id": session, "is_error": true, "total_cost_usd": 0.5,
	})
	return string(b)
}

// TestRetry is a bounded, near-instant policy so retry-exercising tests never
// sleep and never loop forever.
var TestRetry = shared.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Microsecond, MaxDelay: time.Microsecond}

// Snapshot returns a copy of the recorded calls, safe to read while other
// goroutines keep invoking the runner.
func (f *FakeRunner) Snapshot() []RCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RCall(nil), f.Calls...)
}
