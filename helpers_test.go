package main

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"
)

type rcall struct {
	dir   string
	env   []string
	stdin string
	name  string
	args  []string
}

type rresp struct {
	stdout string
	stderr string
	err    error
}

// fakeRunner records every call. Responses come from handler if set,
// otherwise popped from queue in order (empty queue returns success).
type fakeRunner struct {
	mu      sync.Mutex
	calls   []rcall
	queue   []rresp
	handler func(rcall) (string, string, error)
}

func (f *fakeRunner) Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := rcall{dir: dir, env: env, stdin: stdin, name: name, args: args}
	f.calls = append(f.calls, c)
	if f.handler != nil {
		return f.handler(c)
	}
	if len(f.queue) == 0 {
		return "", "", nil
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r.stdout, r.stderr, r.err
}

// RunStream mirrors Run for the streaming seam: it records the call and pulls a
// response from handler/queue exactly as Run does, then writes that response's
// stdout to w so callers parsing a stream (Claude.Call) see it. This lets
// existing tests keep queueing a single claudeJSON payload — it becomes the
// stream's one (terminal) line.
func (f *fakeRunner) RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := rcall{dir: dir, env: env, stdin: stdin, name: name, args: args}
	f.calls = append(f.calls, c)
	var out, stderr string
	var err error
	if f.handler != nil {
		out, stderr, err = f.handler(c)
	} else if len(f.queue) > 0 {
		r := f.queue[0]
		f.queue = f.queue[1:]
		out, stderr, err = r.stdout, r.stderr, r.err
	}
	if out != "" {
		_, _ = io.WriteString(w, out)
	}
	return stderr, err
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// claudeJSON builds a fake `claude -p --output-format json` stdout payload.
func claudeJSON(result, session string) string {
	b, _ := json.Marshal(map[string]any{
		"result": result, "session_id": session, "is_error": false, "total_cost_usd": 0.5,
	})
	return string(b)
}

// claudeErrorJSON builds a fake claude payload that reports an error but still
// carries a valid session id — e.g. a session/rate limit (HTTP 429). This is
// exactly the case where we most want to preserve the session for -rework.
func claudeErrorJSON(result, session string) string {
	b, _ := json.Marshal(map[string]any{
		"result": result, "session_id": session, "is_error": true, "total_cost_usd": 0.5,
	})
	return string(b)
}

// testRetry is a bounded, near-instant policy so retry-exercising tests never
// sleep and never loop forever.
var testRetry = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Microsecond, MaxDelay: time.Microsecond}
