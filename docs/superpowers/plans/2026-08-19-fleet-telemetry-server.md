# Fleet Telemetry Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `loope telemetry-server` subcommand that N opt-in `loope` daemons push their online status, daemon-log tail, and Claude Code rate-limit usage to, rendered as a live fleet dashboard grouped by `repoSlug`.

**Architecture:** Plain JSON-over-HTTP push, no OTel dependency. Workers gain an opt-in exporter goroutine that tails a new rotating `daemon.log` file and a separately-written Claude usage snapshot, and POSTs both plus resource identity to the server every `pushIntervalSec`. The server holds everything in an in-memory, mutex-guarded map (mirroring `serve.go`'s `Server.ghIssues` cache pattern) and renders it with the same embedded-template + htmx-poll-fragment approach as the existing per-repo dashboard.

**Tech Stack:** Go 1.25 standard library only (`net/http`, `html/template`, `embed`, `encoding/json`) — no new dependencies, matching the existing `loope` codebase.

**Spec:** `docs/superpowers/specs/2026-08-19-fleet-telemetry-server-design.md`

## Global Constraints

- No new OTel dependency — plain JSON over HTTP, Genkit-*inspired* data shape only (resource attributes + structured log records).
- Transport is push (worker → server); the server never needs inbound reachability to a worker.
- Logs captured are the daemon's own log output in full — not OS process/CPU/mem metrics (explicitly out of scope).
- Usage captured is the Claude Code account's 5-hour/7-day rolling rate-limit usage and reset time — not per-pipeline-step token/cost accounting (that already exists via `tracker.go`).
- Grouping key is the existing `repoSlug` from `loope.json`.
- Worker participation is opt-in via a `telemetry` block in `loope.json`; when absent, no exporter goroutine starts and no existing daemon behavior changes.
- `pushIntervalSec` defaults to 15 when the `telemetry` block is present but the field is zero.
- Auth is a single shared bearer token (`Authorization: Bearer <token>`); a mismatch is `401`. No per-worker tokens or OAuth.
- Server storage is in-memory only — no persistence to disk/DB. A restart goes blank until workers' next push repopulates it.
- A worker is online when `now - LastPushAt < 3 * pushIntervalSec` (default 15s when never seen).
- The server's log ring buffer is capped at 2000 lines per worker, dropping the oldest first.
- A usage snapshot whose `CapturedAt` is older than 30 minutes renders as "unknown", never a stale or fabricated number.
- The daemon log rotates at 10MB, keeping one previous generation, using no new dependency (hand-rolled, not a logging library).
- The exporter batches at most 500 new log lines per push.
- loope never silently rewrites the user's `~/.claude/settings.json` — the usage-capture mechanism is a separate opt-in helper subcommand the user wires in manually.
- Out of scope (do not implement): OS-level process/CPU/mem metrics, telemetry-server disk/DB persistence, per-worker auth tokens or multi-tenant access control, automatic edits to `~/.claude/settings.json`.

## Assumptions (headless mode — no spec author available to ask)

1. **`Resource` gains a `PushIntervalSec` field**, not present in the spec's illustrative Go struct. The design text requires the online threshold to use "the interval the worker last reported" — the only way the server learns that per-worker is if it rides along on `Resource`, which is resent with every push. Without this field there would be no way to implement the documented behavior.
2. **`--data-dir` is accepted but unused for storage.** The deployment example (`loope telemetry-server --addr :9090 --data-dir ~/.loope/telemetry`) implies a data directory, but the Storage and Out-of-scope sections explicitly rule out disk/DB persistence. This plan accepts and validates the flag (creating the directory, failing fast if unwritable) but stores nothing in it — reserved for future use, not implemented here.
3. **The usage-hook file is overwritten, not append-only.** The spec says the hook "appends the incoming `rate_limits` JSON to `~/.claude/loope-usage.json`", but only the single most-recent snapshot is ever read (30-minute staleness window). This plan replaces the file's contents each invocation, which is simpler and behaviorally equivalent for every reader in this design.
4. **Wire JSON uses lowerCamelCase field tags.** The spec's pseudocode doesn't specify tags; this plan picks the idiomatic Go JSON convention.
5. **`loope claude-usage-hook` writes nothing to stdout.** A `statusLine` command's stdout is what Claude Code displays, so this hook is meant to be tee'd alongside a user's real statusline script (e.g. `sh -c 'tee >(loope claude-usage-hook) | real-statusline.sh'`), not run as a standalone replacement. The spec calls it something the user "chains into their own statusLine command" without pinning the exact composition; this is the shape that doesn't clobber the user's status line.
6. **The "does headless `claude -p` trigger statusLine" risk is left unresolved**, exactly as the spec anticipates. This plan implements and tests the documented degraded path (missing/stale snapshot → `Usage: nil` → dashboard shows "usage: unknown"), so the feature is correct either way; empirically confirming whether headless runs populate the hook file is a follow-up, not a blocker for this plan.

---

## File Structure

| File | Responsibility |
|---|---|
| `telemetry.go` | Wire data model shared by worker and server: `Resource`, `LogRecord`, `UsageSnapshot`, `PushRequest`, `machineID()`. |
| `telemetry_ring.go` | `LogRingBuffer` — bounded FIFO of log lines, oldest dropped first. |
| `telemetry_server.go` | `TelemetryServer`, `WorkerState`, auth check, `POST /v1/push` handler. |
| `telemetry_web.go` | Dashboard render types/handlers for the telemetry server (`GET /`, `/rail`, `/detail`). |
| `telemetry/templates/*.html` | Embedded templates for the fleet dashboard. |
| `telemetry_cmd.go` | `loope telemetry-server` subcommand: flag parsing, HTTP server lifecycle. |
| `logrotate.go` | `RotatingFile` — size-capped `io.Writer` for the daemon log. |
| `telemetry_tailer.go` | `LogTailer` — incremental, rotation-aware reader of new log lines. |
| `telemetry_usage_hook.go` | `loope claude-usage-hook` subcommand: parses `statusLine` JSON, writes the usage snapshot file. |
| `telemetry_exporter.go` | `TelemetryExporter` — worker-side goroutine that assembles and POSTs `PushRequest`s. |
| `config.go` (modify) | Add `TelemetryConfig` and `Config.Telemetry`. |
| `main.go` (modify) | Subcommand dispatch; wire daemon log rotation and the exporter goroutine into the daemon startup path. |
| `docs/telemetry.md` | User-facing guide: running the server, opting a worker in, wiring the usage hook. |
| `README.md` (modify) | One row added to the documentation table. |

---

### Task 1: Telemetry data model

**Files:**
- Create: `telemetry.go`
- Test: `telemetry_test.go`

**Interfaces:**
- Produces: `Resource{RepoSlug, MachineID, Hostname, WorkDir, Version, PushIntervalSec string/int}`, `LogRecord{Timestamp time.Time, Body string}`, `UsageSnapshot{FiveHourUsedPct, FiveHourResetAt, SevenDayUsedPct, SevenDayResetAt, CapturedAt}`, `PushRequest{Resource, Logs []LogRecord, Usage *UsageSnapshot, SentAt time.Time}`, `machineID(hostname, workDir string) string`.

- [ ] **Step 1: Write the failing test**

```go
// telemetry_test.go
package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMachineIDStableAndDistinct(t *testing.T) {
	a := machineID("host1", "/work/a")
	b := machineID("host1", "/work/a")
	if a != b {
		t.Fatalf("machineID not stable: %q != %q", a, b)
	}
	if len(a) != 12 {
		t.Fatalf("machineID length = %d, want 12", len(a))
	}
	c := machineID("host1", "/work/b")
	if a == c {
		t.Fatalf("machineID for a different workDir must differ")
	}
}

func TestPushRequestRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	want := PushRequest{
		Resource: Resource{RepoSlug: "o/r", MachineID: "abc123def456", Hostname: "host1", WorkDir: "/work", Version: "dev", PushIntervalSec: 15},
		Logs:     []LogRecord{{Timestamp: now, Body: "hello"}},
		Usage: &UsageSnapshot{
			FiveHourUsedPct: 12.5, FiveHourResetAt: now,
			SevenDayUsedPct: 40, SevenDayResetAt: now,
			CapturedAt: now,
		},
		SentAt: now,
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got PushRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Resource != want.Resource {
		t.Fatalf("resource round-trip = %+v, want %+v", got.Resource, want.Resource)
	}
	if len(got.Logs) != 1 || got.Logs[0].Body != "hello" || !got.Logs[0].Timestamp.Equal(now) {
		t.Fatalf("logs round-trip = %+v", got.Logs)
	}
	if got.Usage == nil || *got.Usage != *want.Usage {
		t.Fatalf("usage round-trip = %+v, want %+v", got.Usage, want.Usage)
	}
}

func TestPushRequestNilUsageRoundTrip(t *testing.T) {
	data, err := json.Marshal(PushRequest{Resource: Resource{RepoSlug: "o/r"}})
	if err != nil {
		t.Fatal(err)
	}
	var got PushRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage != nil {
		t.Fatalf("usage = %+v, want nil", got.Usage)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestMachineID|TestPushRequest' -v`
Expected: FAIL — `undefined: machineID`, `undefined: PushRequest`, etc. (the package won't even build).

- [ ] **Step 3: Write the implementation**

```go
// telemetry.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

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

// PushRequest is the body of POST /v1/push. Usage is nil when unavailable or
// stale, so the dashboard renders "usage: unknown" instead of a fabricated
// number.
type PushRequest struct {
	Resource Resource       `json:"resource"`
	Logs     []LogRecord    `json:"logs"`
	Usage    *UsageSnapshot `json:"usage"`
	SentAt   time.Time      `json:"sentAt"`
}

// machineID is a stable per-(hostname,workDir) identity: sha256(hostname +
// workDir), hex-encoded and truncated to 12 characters. It survives restarts
// of the same daemon (same host, same workDir) but distinguishes two daemons
// on one host watching different repos.
func machineID(hostname, workDir string) string {
	sum := sha256.Sum256([]byte(hostname + workDir))
	return hex.EncodeToString(sum[:])[:12]
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestMachineID|TestPushRequest' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telemetry.go telemetry_test.go
git commit -m "feat(telemetry): add wire data model and machine identity"
```

---

### Task 2: Bounded log ring buffer

**Files:**
- Create: `telemetry_ring.go`
- Test: `telemetry_ring_test.go`

**Interfaces:**
- Produces: `LogRingBuffer`, `NewLogRingBuffer(cap int) *LogRingBuffer`, `(*LogRingBuffer).Add(lines ...string)`, `(*LogRingBuffer).Lines() []string`.

- [ ] **Step 1: Write the failing test**

```go
// telemetry_ring_test.go
package main

import "testing"

func TestLogRingBufferAddAndLines(t *testing.T) {
	b := NewLogRingBuffer(5)
	b.Add("a", "b")
	b.Add("c")
	got := b.Lines()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lines() = %v, want %v", got, want)
		}
	}
}

func TestLogRingBufferDropsOldestPastCapacity(t *testing.T) {
	b := NewLogRingBuffer(3)
	b.Add("1", "2", "3", "4", "5")
	got := b.Lines()
	want := []string{"3", "4", "5"}
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lines() = %v, want %v", got, want)
		}
	}
}

func TestLogRingBufferLinesReturnsIndependentCopy(t *testing.T) {
	b := NewLogRingBuffer(5)
	b.Add("a")
	got := b.Lines()
	got[0] = "mutated"
	if b.Lines()[0] != "a" {
		t.Fatalf("mutating the returned slice must not affect the buffer")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestLogRingBuffer -v`
Expected: FAIL — `undefined: NewLogRingBuffer`

- [ ] **Step 3: Write the implementation**

```go
// telemetry_ring.go
package main

// LogRingBuffer is a bounded FIFO of log lines: appending past capacity
// drops the oldest lines first. Not safe for concurrent use — callers
// serialize access (TelemetryServer guards it with its own mutex).
type LogRingBuffer struct {
	cap   int
	lines []string
}

// NewLogRingBuffer returns a buffer that holds at most cap lines.
func NewLogRingBuffer(cap int) *LogRingBuffer {
	return &LogRingBuffer{cap: cap}
}

// Add appends lines, dropping the oldest entries first if the total exceeds
// the buffer's capacity.
func (b *LogRingBuffer) Add(lines ...string) {
	b.lines = append(b.lines, lines...)
	if over := len(b.lines) - b.cap; over > 0 {
		b.lines = b.lines[over:]
	}
}

// Lines returns the buffered lines, oldest first. The returned slice is a
// fresh copy, safe for the caller to retain across further Add calls.
func (b *LogRingBuffer) Lines() []string {
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestLogRingBuffer -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telemetry_ring.go telemetry_ring_test.go
git commit -m "feat(telemetry): add bounded log ring buffer"
```

---

### Task 3: Telemetry server core — auth, push ingest, online/staleness logic

**Files:**
- Create: `telemetry_server.go`
- Test: `telemetry_server_test.go`

**Interfaces:**
- Consumes: `Resource`, `LogRecord`, `UsageSnapshot`, `PushRequest` (Task 1); `LogRingBuffer`, `NewLogRingBuffer` (Task 2).
- Produces: `WorkerState{Resource, LastPushAt, Usage, Logs}`, `(*WorkerState).online(now time.Time) bool`, `(*WorkerState).usableUsage(now time.Time) *UsageSnapshot`, `TelemetryServer{token, now, mu, workers}`, `NewTelemetryServer(token string) *TelemetryServer`, `(*TelemetryServer).Handler() http.Handler`, constants `defaultPushIntervalSec = 15`, `maxLogLinesPerWorker = 2000`, `usageStaleAfter = 30 * time.Minute`. (Task 9 will change `NewTelemetryServer`'s signature to `(*TelemetryServer, error)` once it adds template parsing — later tasks depending on it are written against that final signature.)

- [ ] **Step 1: Write the failing test**

```go
// telemetry_server_test.go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func doPush(t *testing.T, h http.Handler, token string, req PushRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/push", bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestHandlePushAuth(t *testing.T) {
	s := NewTelemetryServer("secret")
	h := s.Handler()
	req := PushRequest{Resource: Resource{MachineID: "m1"}}

	if rec := doPush(t, h, "", req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", rec.Code)
	}
	if rec := doPush(t, h, "wrong", req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}
	if rec := doPush(t, h, "secret", req); rec.Code != http.StatusNoContent {
		t.Fatalf("correct token: status = %d, want 204", rec.Code)
	}
}

func TestHandlePushRequiresMachineID(t *testing.T) {
	s := NewTelemetryServer("secret")
	rec := doPush(t, s.Handler(), "secret", PushRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandlePushStoresWorkerAndAppendsLogs(t *testing.T) {
	s := NewTelemetryServer("secret")
	h := s.Handler()
	req1 := PushRequest{
		Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		Logs:     []LogRecord{{Body: "line1"}, {Body: "line2"}},
	}
	if rec := doPush(t, h, "secret", req1); rec.Code != http.StatusNoContent {
		t.Fatalf("push 1 status = %d", rec.Code)
	}
	req2 := PushRequest{
		Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		Logs:     []LogRecord{{Body: "line3"}},
	}
	if rec := doPush(t, h, "secret", req2); rec.Code != http.StatusNoContent {
		t.Fatalf("push 2 status = %d", rec.Code)
	}

	s.mu.Lock()
	ws := s.workers["m1"]
	s.mu.Unlock()
	if ws == nil {
		t.Fatal("worker m1 not stored")
	}
	got := ws.Logs.Lines()
	want := []string{"line1", "line2", "line3"}
	if len(got) != len(want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %v, want %v", got, want)
		}
	}
}

func TestWorkerStateOnlineOfflineTransition(t *testing.T) {
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	ws := &WorkerState{Resource: Resource{PushIntervalSec: 15}, LastPushAt: base}

	if !ws.online(base.Add(44 * time.Second)) {
		t.Fatal("expected online just under 3x the 15s interval (45s)")
	}
	if ws.online(base.Add(46 * time.Second)) {
		t.Fatal("expected offline just over 3x the 15s interval (45s)")
	}
}

func TestWorkerStateOnlineDefaultsIntervalWhenUnset(t *testing.T) {
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	ws := &WorkerState{LastPushAt: base} // PushIntervalSec zero-value
	if !ws.online(base.Add(44 * time.Second)) {
		t.Fatal("expected the 15s default to apply when PushIntervalSec is unset")
	}
}

func TestWorkerStateUsableUsageStaleness(t *testing.T) {
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	fresh := &UsageSnapshot{FiveHourUsedPct: 10, CapturedAt: base.Add(-29 * time.Minute)}
	stale := &UsageSnapshot{FiveHourUsedPct: 10, CapturedAt: base.Add(-31 * time.Minute)}

	ws := &WorkerState{Usage: fresh}
	if ws.usableUsage(base) == nil {
		t.Fatal("29-minute-old usage must still be usable")
	}
	ws.Usage = stale
	if ws.usableUsage(base) != nil {
		t.Fatal("31-minute-old usage must render as unknown")
	}
	ws.Usage = nil
	if ws.usableUsage(base) != nil {
		t.Fatal("nil usage must stay nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestHandlePush|TestWorkerState' -v`
Expected: FAIL — `undefined: NewTelemetryServer`

- [ ] **Step 3: Write the implementation**

```go
// telemetry_server.go
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultPushIntervalSec = 15
	maxLogLinesPerWorker    = 2000
	usageStaleAfter         = 30 * time.Minute
)

// WorkerState is the server's last-known view of one worker, keyed by
// Resource.MachineID.
type WorkerState struct {
	Resource   Resource
	LastPushAt time.Time
	Usage      *UsageSnapshot
	Logs       *LogRingBuffer
}

// online reports whether the worker has pushed recently enough to be
// considered live: at most 3 push intervals have elapsed since its last
// push. A worker whose Resource never carried a PushIntervalSec uses
// defaultPushIntervalSec.
func (w *WorkerState) online(now time.Time) bool {
	interval := w.Resource.PushIntervalSec
	if interval <= 0 {
		interval = defaultPushIntervalSec
	}
	return now.Sub(w.LastPushAt) < time.Duration(3*interval)*time.Second
}

// usableUsage returns w.Usage if it is fresh enough to show, else nil — a
// snapshot whose CapturedAt is older than usageStaleAfter renders as
// "unknown" rather than a stale number.
func (w *WorkerState) usableUsage(now time.Time) *UsageSnapshot {
	if w.Usage == nil || now.Sub(w.Usage.CapturedAt) > usageStaleAfter {
		return nil
	}
	return w.Usage
}

// TelemetryServer receives worker pushes and renders the fleet dashboard.
// All state is in-memory: a restart goes blank until workers' next push
// repopulates it (see the design doc's storage section) — this is a live
// view, not a system of record.
type TelemetryServer struct {
	token string
	now   func() time.Time

	mu      sync.Mutex
	workers map[string]*WorkerState // keyed by Resource.MachineID
}

// NewTelemetryServer returns a server that authenticates pushes against
// token.
func NewTelemetryServer(token string) *TelemetryServer {
	return &TelemetryServer{token: token, now: time.Now, workers: map[string]*WorkerState{}}
}

// Handler returns the telemetry server's HTTP routes: POST /v1/push (worker
// ingest).
func (s *TelemetryServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/push", s.handlePush)
	return mux
}

// checkAuth reports whether the request carries the correct bearer token.
func checkAuth(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, prefix) && strings.TrimPrefix(h, prefix) == token
}

// handlePush ingests one worker's PushRequest. New log lines are appended to
// the worker's ring buffer; Usage replaces the prior snapshot outright — the
// exporter always sends its latest read (nil when unavailable), so there is
// nothing to merge.
func (s *TelemetryServer) handlePush(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r, s.token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Resource.MachineID == "" {
		http.Error(w, "resource.machineID is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.workers[req.Resource.MachineID]
	if ws == nil {
		ws = &WorkerState{Logs: NewLogRingBuffer(maxLogLinesPerWorker)}
		s.workers[req.Resource.MachineID] = ws
	}
	ws.Resource = req.Resource
	ws.LastPushAt = s.now()
	ws.Usage = req.Usage
	lines := make([]string, len(req.Logs))
	for i, l := range req.Logs {
		lines[i] = l.Body
	}
	ws.Logs.Add(lines...)

	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestHandlePush|TestWorkerState' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telemetry_server.go telemetry_server_test.go
git commit -m "feat(telemetry): add push-ingest server with auth and online/staleness rules"
```

---

### Task 4: Rotating daemon log file

**Files:**
- Create: `logrotate.go`
- Test: `logrotate_test.go`

**Interfaces:**
- Produces: `RotatingFile`, `NewRotatingFile(path string, maxBytes int64) (*RotatingFile, error)`, `(*RotatingFile).Write(p []byte) (int, error)` (satisfies `io.Writer`), `(*RotatingFile).Close() error`, constant `rotatingFileMaxBytes = 10 * 1024 * 1024`.

- [ ] **Step 1: Write the failing test**

```go
// logrotate_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingFileWritesUnderThresholdDoNotRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	rf, err := NewRotatingFile(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	if _, err := rf.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("no rotation expected under the threshold")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("content = %q", data)
	}
}

func TestRotatingFileRotatesAtThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	rf, err := NewRotatingFile(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	if _, err := rf.Write([]byte("0123456789")); err != nil { // exactly fills to 10 bytes
		t.Fatal(err)
	}
	if _, err := rf.Write([]byte("next")); err != nil { // would push past 10: must rotate first
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected a rotated backup file: %v", err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "0123456789" {
		t.Fatalf("backup content = %q", backup)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "next" {
		t.Fatalf("current content = %q, want the post-rotation write only", current)
	}
}

func TestRotatingFileReopensExistingFileWithoutLosingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	rf1, err := NewRotatingFile(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf1.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	rf1.Close()

	rf2, err := NewRotatingFile(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer rf2.Close()
	if _, err := rf2.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first\nsecond\n" {
		t.Fatalf("content = %q, want appended content preserved across a reopen", data)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestRotatingFile -v`
Expected: FAIL — `undefined: NewRotatingFile`

- [ ] **Step 3: Write the implementation**

```go
// logrotate.go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotatingFileMaxBytes is the size threshold at which the daemon log
// rotates.
const rotatingFileMaxBytes = 10 * 1024 * 1024 // 10MB

// RotatingFile is an io.Writer over a size-capped log file: once the current
// file would exceed maxBytes, it is renamed to a ".1" sibling (overwriting
// any previous one) and a fresh file is opened — so the daemon log never
// grows unbounded while keeping one previous generation around. Safe for
// concurrent use.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	maxBytes int64
}

// NewRotatingFile opens (creating if needed) path for appending and returns
// a RotatingFile that rotates it at maxBytes.
func NewRotatingFile(path string, maxBytes int64) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &RotatingFile{path: path, f: f, size: info.Size(), maxBytes: maxBytes}, nil
}

// Write appends p to the current file, rotating first if p would push the
// file past maxBytes. A single Write call is never split across the
// rotation boundary, so a reader tailing the file never sees a line cut in
// half by rotation.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames the current file to its ".1" sibling (overwriting a prior
// one) and opens a fresh file at path. Caller must hold r.mu.
func (r *RotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	backup := r.path + ".1"
	if err := os.Rename(r.path, backup); err != nil {
		return fmt.Errorf("rotate %s: %w", r.path, err)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	r.f = f
	r.size = 0
	return nil
}

// Close closes the underlying file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestRotatingFile -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add logrotate.go logrotate_test.go
git commit -m "feat(telemetry): add size-based rotating file writer for the daemon log"
```

---

### Task 5: Incremental, rotation-aware log tailer

**Files:**
- Create: `telemetry_tailer.go`
- Test: `telemetry_tailer_test.go`

**Interfaces:**
- Produces: `LogTailer{path, offset}`, `NewLogTailer(path string) *LogTailer`, `(*LogTailer).Next(maxLines int) ([]string, error)`.

- [ ] **Step 1: Write the failing test**

```go
// telemetry_tailer_test.go
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogTailerMissingFileReturnsNoLines(t *testing.T) {
	tl := NewLogTailer(filepath.Join(t.TempDir(), "nope.log"))
	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("lines = %v, want none", lines)
	}
}

func TestLogTailerReadsNewLinesSinceLastCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("old1\nold2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := NewLogTailer(path) // starts at current EOF: does not resend history

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new1\nnew2\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new1", "new2"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines = %v, want %v", lines, want)
		}
	}
}

func TestLogTailerLeavesPartialLineForNextCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := &LogTailer{path: path} // starts at offset 0

	if err := os.WriteFile(path, []byte("complete\npartial-no-newline-yet"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "complete" {
		t.Fatalf("lines = %v, want [complete] (the partial line must wait)", lines)
	}

	if err := os.WriteFile(path, []byte("complete\npartial-no-newline-yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err = tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "partial-no-newline-yet" {
		t.Fatalf("lines = %v, want [partial-no-newline-yet] once its newline arrives", lines)
	}
}

func TestLogTailerCapsAtMaxLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	content := strings.Repeat("line\n", 10)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := &LogTailer{path: path}

	lines, err := tl.Next(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	rest, err := tl.Next(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 7 {
		t.Fatalf("rest = %d, want the remaining 7 lines", len(rest))
	}
}

func TestLogTailerHandlesRotationBySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("old\n", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := &LogTailer{path: path}
	if _, err := tl.Next(10); err != nil { // advance offset partway into the "old" file
		t.Fatal(err)
	}

	// Simulate RotatingFile.rotate(): the same path now holds a much
	// smaller, freshly-started file.
	if err := os.WriteFile(path, []byte("rotated1\nrotated2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rotated1", "rotated2"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v (tailer must restart from 0 after rotation)", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines = %v, want %v", lines, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestLogTailer -v`
Expected: FAIL — `undefined: NewLogTailer`

- [ ] **Step 3: Write the implementation**

```go
// telemetry_tailer.go
package main

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// LogTailer incrementally reads new, complete lines appended to a file since
// its last call. It copes with the file being rotated out from under it
// (same path, shorter size — see RotatingFile.rotate) by starting over from
// the beginning of the new file. A trailing partial line (the writer is
// mid-write) is left unread until the next call, so tailing never ships a
// half-written line.
type LogTailer struct {
	path   string
	offset int64
}

// NewLogTailer returns a tailer starting from the current end of path, so a
// freshly started exporter does not resend the daemon's entire log history
// on its first push. A missing file starts at offset 0, so lines written
// after this call are picked up once the file exists.
func NewLogTailer(path string) *LogTailer {
	t := &LogTailer{path: path}
	if info, err := os.Stat(path); err == nil {
		t.offset = info.Size()
	}
	return t
}

// Next returns up to maxLines complete lines appended since the last call
// (or since NewLogTailer, on the first call), advancing the tailer's offset
// past them. It detects rotation by the file being shorter than the last
// known offset and restarts from the beginning in that case.
func (t *LogTailer) Next(maxLines int) ([]string, error) {
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < t.offset {
		t.offset = 0 // rotated out from under us: start over
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, err
	}

	var lines []string
	r := bufio.NewReader(f)
	for len(lines) < maxLines {
		line, err := r.ReadString('\n')
		if err != nil {
			break // no complete line yet (EOF mid-line): leave it for next call
		}
		lines = append(lines, strings.TrimSuffix(line, "\n"))
		t.offset += int64(len(line))
	}
	return lines, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestLogTailer -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telemetry_tailer.go telemetry_tailer_test.go
git commit -m "feat(telemetry): add rotation-aware incremental log tailer"
```

---

### Task 6: Claude usage capture hook

**Files:**
- Create: `telemetry_usage_hook.go`
- Test: `telemetry_usage_hook_test.go`

**Interfaces:**
- Produces: `usageHookFile() (string, error)`, `parseClaudeStatusLine(data []byte, capturedAt time.Time) (UsageSnapshot, error)`, `runClaudeUsageHookCmd(stdin io.Reader) int`.

- [ ] **Step 1: Write the failing test**

```go
// telemetry_usage_hook_test.go
package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeStatusLine(t *testing.T) {
	in := []byte(`{"rate_limits":{"five_hour":{"used_percentage":12.5,"resets_at":"2026-08-19T15:00:00Z"},"seven_day":{"used_percentage":40,"resets_at":"2026-08-22T00:00:00Z"}}}`)
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	u, err := parseClaudeStatusLine(in, now)
	if err != nil {
		t.Fatal(err)
	}
	if u.FiveHourUsedPct != 12.5 || u.SevenDayUsedPct != 40 {
		t.Fatalf("usage = %+v", u)
	}
	if !u.CapturedAt.Equal(now) {
		t.Fatalf("CapturedAt = %v, want %v", u.CapturedAt, now)
	}
	wantReset := time.Date(2026, 8, 19, 15, 0, 0, 0, time.UTC)
	if !u.FiveHourResetAt.Equal(wantReset) {
		t.Fatalf("FiveHourResetAt = %v, want %v", u.FiveHourResetAt, wantReset)
	}
}

func TestParseClaudeStatusLineMalformedJSON(t *testing.T) {
	if _, err := parseClaudeStatusLine([]byte("not json"), time.Now()); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestParseClaudeStatusLineUnparsableResetLeftZero(t *testing.T) {
	in := []byte(`{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":"garbage"}}}`)
	u, err := parseClaudeStatusLine(in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !u.FiveHourResetAt.IsZero() {
		t.Fatalf("FiveHourResetAt = %v, want zero value for an unparsable timestamp", u.FiveHourResetAt)
	}
}

func TestRunClaudeUsageHookCmdWritesFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	input := `{"rate_limits":{"five_hour":{"used_percentage":5},"seven_day":{"used_percentage":9}}}`
	if code := runClaudeUsageHookCmd(strings.NewReader(input)); code != 0 {
		t.Fatalf("code = %d", code)
	}
	path, err := usageHookFile()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got UsageSnapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.FiveHourUsedPct != 5 || got.SevenDayUsedPct != 9 {
		t.Fatalf("got = %+v", got)
	}
}

func TestRunClaudeUsageHookCmdInvalidInputReturnsNonZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if code := runClaudeUsageHookCmd(strings.NewReader("not json")); code == 0 {
		t.Fatal("expected a non-zero exit code for malformed input")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestParseClaudeStatusLine|TestRunClaudeUsageHookCmd' -v`
Expected: FAIL — `undefined: parseClaudeStatusLine`

- [ ] **Step 3: Write the implementation**

```go
// telemetry_usage_hook.go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// claudeStatusLineInput is the subset of the JSON Claude Code feeds to a
// configured statusLine command that this hook cares about; other fields
// (workspace, model, etc.) are ignored.
type claudeStatusLineInput struct {
	RateLimits struct {
		FiveHour struct {
			UsedPercentage float64 `json:"used_percentage"`
			ResetsAt       string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay struct {
			UsedPercentage float64 `json:"used_percentage"`
			ResetsAt       string  `json:"resets_at"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

// usageHookFile is where `loope claude-usage-hook` writes the latest
// rate-limit snapshot, and where the telemetry exporter reads it back.
func usageHookFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "loope-usage.json"), nil
}

// parseClaudeStatusLine decodes the statusLine hook's JSON into a
// UsageSnapshot stamped with capturedAt. A reset timestamp that fails to
// parse is left zero rather than failing the whole snapshot — a partial
// usage read is still worth recording.
func parseClaudeStatusLine(data []byte, capturedAt time.Time) (UsageSnapshot, error) {
	var in claudeStatusLineInput
	if err := json.Unmarshal(data, &in); err != nil {
		return UsageSnapshot{}, err
	}
	u := UsageSnapshot{
		FiveHourUsedPct: in.RateLimits.FiveHour.UsedPercentage,
		SevenDayUsedPct: in.RateLimits.SevenDay.UsedPercentage,
		CapturedAt:      capturedAt,
	}
	if t, err := time.Parse(time.RFC3339, in.RateLimits.FiveHour.ResetsAt); err == nil {
		u.FiveHourResetAt = t
	}
	if t, err := time.Parse(time.RFC3339, in.RateLimits.SevenDay.ResetsAt); err == nil {
		u.SevenDayResetAt = t
	}
	return u, nil
}

// runClaudeUsageHookCmd implements `loope claude-usage-hook`: it reads the
// statusLine JSON from stdin and overwrites ~/.claude/loope-usage.json with
// the parsed rate-limit snapshot. It prints nothing to stdout, so a user can
// tee the same stdin into it alongside their real statusLine command without
// affecting what Claude Code displays.
func runClaudeUsageHookCmd(stdin io.Reader) int {
	data, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: read stdin: %v\n", err)
		return 1
	}
	usage, err := parseClaudeStatusLine(data, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: parse: %v\n", err)
		return 1
	}
	path, err := usageHookFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	out, err := json.Marshal(usage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: write %s: %v\n", path, err)
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestParseClaudeStatusLine|TestRunClaudeUsageHookCmd' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telemetry_usage_hook.go telemetry_usage_hook_test.go
git commit -m "feat(telemetry): add claude-usage-hook subcommand for statusLine capture"
```

---

### Task 7: Worker opt-in config block

**Files:**
- Modify: `config.go`
- Test: `config_test.go` (append)

**Interfaces:**
- Produces: `TelemetryConfig{ServerURL, Token string, PushIntervalSec int}`, `Config.Telemetry *TelemetryConfig`.

- [ ] **Step 1: Write the failing test**

Append to `config_test.go`:

```go
func TestLoadConfigTelemetryAbsentByDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath":"/r","repoSlug":"o/r","workDir":"/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry != nil {
		t.Fatalf("Telemetry = %+v, want nil when the block is absent", cfg.Telemetry)
	}
}

func TestLoadConfigTelemetryPushIntervalDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath":"/r","repoSlug":"o/r","workDir":"/w","telemetry":{"serverURL":"http://host:9090","token":"secret"}}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry == nil {
		t.Fatal("Telemetry = nil, want a populated block")
	}
	if cfg.Telemetry.PushIntervalSec != 15 {
		t.Fatalf("PushIntervalSec = %d, want the 15s default", cfg.Telemetry.PushIntervalSec)
	}
	if cfg.Telemetry.ServerURL != "http://host:9090" || cfg.Telemetry.Token != "secret" {
		t.Fatalf("Telemetry = %+v", cfg.Telemetry)
	}
}

func TestLoadConfigTelemetryPushIntervalOverride(t *testing.T) {
	p := writeTemp(t, `{"repoPath":"/r","repoSlug":"o/r","workDir":"/w","telemetry":{"serverURL":"http://host:9090","token":"secret","pushIntervalSec":30}}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.PushIntervalSec != 30 {
		t.Fatalf("PushIntervalSec = %d, want 30", cfg.Telemetry.PushIntervalSec)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestLoadConfigTelemetry -v`
Expected: FAIL — `cfg.Telemetry undefined (type *Config has no field or method Telemetry)`

- [ ] **Step 3: Write the implementation**

In `config.go`, add the new type above `type Config struct`:

```go
// TelemetryConfig opts this daemon in to pushing its status, log tail, and
// Claude usage to a `loope telemetry-server`. Absent from the config, no
// exporter goroutine starts and nothing about daemon behavior changes.
type TelemetryConfig struct {
	ServerURL       string `json:"serverURL"`
	Token           string `json:"token"`
	PushIntervalSec int    `json:"pushIntervalSec"`
}
```

Add the field to `Config` (after `Models Models \`json:"models"\``):

```go
	Models              Models           `json:"models"`
	// Telemetry is nil unless the config file has a "telemetry" block —
	// participation is opt-in.
	Telemetry           *TelemetryConfig `json:"telemetry"`
```

In `LoadConfig`, after the existing `cfg.ClaudeConfigDir = expandHome(cfg.ClaudeConfigDir)` line and before `return cfg, nil`, add the default-fill:

```go
	if cfg.Telemetry != nil && cfg.Telemetry.PushIntervalSec == 0 {
		cfg.Telemetry.PushIntervalSec = 15
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestLoadConfigTelemetry -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat(telemetry): add opt-in telemetry config block"
```

---

### Task 8: Worker-side exporter

**Files:**
- Create: `telemetry_exporter.go`
- Test: `telemetry_exporter_test.go`

**Interfaces:**
- Consumes: `Config.Telemetry` (Task 7); `LogTailer`, `NewLogTailer`, `(*LogTailer).Next` (Task 5); `usageHookFile` (Task 6); `Resource`, `LogRecord`, `UsageSnapshot`, `PushRequest`, `machineID` (Task 1); `usageStaleAfter` (Task 3); package-level `var version string` (`main.go`).
- Produces: `TelemetryExporter{cfg, client, tailer, usagePath}`, `NewTelemetryExporter(cfg *Config, logPath string) *TelemetryExporter`, `(*TelemetryExporter).Run(ctx context.Context)`, `(*TelemetryExporter).pushOnce(ctx context.Context) error`, `readUsageSnapshot(path string, now time.Time) (*UsageSnapshot, error)`.

- [ ] **Step 1: Write the failing test**

```go
// telemetry_exporter_test.go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExporterPushOnceSendsLogsAndAuth(t *testing.T) {
	var gotReq PushRequest
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	logPath := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(logPath, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{RepoSlug: "o/r", WorkDir: "/work", Telemetry: &TelemetryConfig{ServerURL: srv.URL, Token: "secret", PushIntervalSec: 15}}
	e := &TelemetryExporter{cfg: cfg, client: srv.Client(), tailer: &LogTailer{path: logPath}}

	if err := e.pushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotReq.Resource.RepoSlug != "o/r" || gotReq.Resource.WorkDir != "/work" || gotReq.Resource.PushIntervalSec != 15 {
		t.Fatalf("resource = %+v", gotReq.Resource)
	}
	if len(gotReq.Logs) != 2 || gotReq.Logs[0].Body != "line1" || gotReq.Logs[1].Body != "line2" {
		t.Fatalf("logs = %+v", gotReq.Logs)
	}
	if gotReq.Usage != nil {
		t.Fatalf("usage = %+v, want nil (no usage file configured)", gotReq.Usage)
	}
}

func TestExporterPushOnceIncludesFreshUsage(t *testing.T) {
	var gotReq PushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	usagePath := filepath.Join(t.TempDir(), "loope-usage.json")
	snap := UsageSnapshot{FiveHourUsedPct: 42, CapturedAt: time.Now()}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usagePath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{RepoSlug: "o/r", WorkDir: "/work", Telemetry: &TelemetryConfig{ServerURL: srv.URL, Token: "secret", PushIntervalSec: 15}}
	e := &TelemetryExporter{
		cfg:       cfg,
		client:    srv.Client(),
		tailer:    &LogTailer{path: filepath.Join(t.TempDir(), "daemon.log")},
		usagePath: usagePath,
	}

	if err := e.pushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotReq.Usage == nil || gotReq.Usage.FiveHourUsedPct != 42 {
		t.Fatalf("usage = %+v, want FiveHourUsedPct=42", gotReq.Usage)
	}
}

func TestExporterPushOnceReturnsErrorOnNonNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := &Config{RepoSlug: "o/r", WorkDir: "/work", Telemetry: &TelemetryConfig{ServerURL: srv.URL, Token: "wrong", PushIntervalSec: 15}}
	e := &TelemetryExporter{cfg: cfg, client: srv.Client(), tailer: &LogTailer{path: filepath.Join(t.TempDir(), "daemon.log")}}

	if err := e.pushOnce(context.Background()); err == nil {
		t.Fatal("expected an error on a non-204 response")
	}
}

func TestReadUsageSnapshotStaleReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	stale := UsageSnapshot{CapturedAt: time.Now().Add(-31 * time.Minute)}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readUsageSnapshot(path, time.Now())
	if err != nil || got != nil {
		t.Fatalf("got = %v, err = %v, want nil, nil", got, err)
	}
}

func TestReadUsageSnapshotMissingFileReturnsNil(t *testing.T) {
	got, err := readUsageSnapshot(filepath.Join(t.TempDir(), "nope.json"), time.Now())
	if err != nil || got != nil {
		t.Fatalf("got = %v, err = %v, want nil, nil", got, err)
	}
}

func TestReadUsageSnapshotEmptyPathReturnsNil(t *testing.T) {
	got, err := readUsageSnapshot("", time.Now())
	if err != nil || got != nil {
		t.Fatalf("got = %v, err = %v, want nil, nil", got, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestExporter|TestReadUsageSnapshot' -v`
Expected: FAIL — `undefined: TelemetryExporter`

- [ ] **Step 3: Write the implementation**

```go
// telemetry_exporter.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// maxLogLinesPerPush caps how many new daemon-log lines one push carries, so
// a worker that was offline a while (or just started against a large
// existing log) does not send one enormous request; the remainder catches
// up over the next few push cycles.
const maxLogLinesPerPush = 500

// TelemetryExporter pushes this daemon's log tail and Claude usage to a
// telemetry-server on a fixed interval. It is opt-in: nothing constructs or
// runs it when cfg.Telemetry is nil.
type TelemetryExporter struct {
	cfg       *Config
	client    *http.Client
	tailer    *LogTailer
	usagePath string // "" if the user's home directory could not be resolved
}

// NewTelemetryExporter builds an exporter for cfg.Telemetry, tailing the
// daemon log at logPath (the same path main.go points the rotating log
// writer at).
func NewTelemetryExporter(cfg *Config, logPath string) *TelemetryExporter {
	usagePath, _ := usageHookFile() // empty on error: readUsageSnapshot then always reports "unavailable"
	return &TelemetryExporter{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, tailer: NewLogTailer(logPath), usagePath: usagePath}
}

// Run pushes on cfg.Telemetry.PushIntervalSec until ctx is cancelled. A push
// failure is logged and retried next tick — a slow or unreachable server
// never blocks the daemon's own work, since this runs in its own goroutine.
func (e *TelemetryExporter) Run(ctx context.Context) {
	interval := time.Duration(e.cfg.Telemetry.PushIntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := e.pushOnce(ctx); err != nil {
			log.Printf("telemetry: push failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pushOnce assembles and sends one PushRequest.
func (e *TelemetryExporter) pushOnce(ctx context.Context) error {
	lines, err := e.tailer.Next(maxLogLinesPerPush)
	if err != nil {
		log.Printf("telemetry: read log tail: %v", err)
	}
	now := time.Now()
	logs := make([]LogRecord, len(lines))
	for i, l := range lines {
		logs[i] = LogRecord{Timestamp: now, Body: l}
	}

	usage, err := readUsageSnapshot(e.usagePath, now)
	if err != nil {
		log.Printf("telemetry: read usage snapshot: %v", err)
	}

	hostname, _ := os.Hostname()
	req := PushRequest{
		Resource: Resource{
			RepoSlug:        e.cfg.RepoSlug,
			MachineID:       machineID(hostname, e.cfg.WorkDir),
			Hostname:        hostname,
			WorkDir:         e.cfg.WorkDir,
			Version:         version,
			PushIntervalSec: e.cfg.Telemetry.PushIntervalSec,
		},
		Logs:   logs,
		Usage:  usage,
		SentAt: now,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Telemetry.ServerURL+"/v1/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+e.cfg.Telemetry.Token)
	resp, err := e.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("push: server returned %s", resp.Status)
	}
	return nil
}

// readUsageSnapshot reads the usage-hook file at path, returning nil when
// path is empty, the file is missing, or its CapturedAt is older than
// usageStaleAfter — the dashboard then renders "usage: unknown" rather than
// a stale or fabricated number.
func readUsageSnapshot(path string, now time.Time) (*UsageSnapshot, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var u UsageSnapshot
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	if now.Sub(u.CapturedAt) > usageStaleAfter {
		return nil, nil
	}
	return &u, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestExporter|TestReadUsageSnapshot' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telemetry_exporter.go telemetry_exporter_test.go
git commit -m "feat(telemetry): add worker-side push exporter"
```

---

### Task 9: Fleet dashboard — templates and render handlers

**Files:**
- Create: `telemetry_web.go`
- Create: `telemetry/templates/page.html`
- Create: `telemetry/templates/rail.html`
- Create: `telemetry/templates/detail.html`
- Modify: `telemetry_server.go` (constructor gains template parsing)
- Modify: `telemetry_server_test.go` (constructor call sites)
- Test: `telemetry_web_test.go`

**Interfaces:**
- Consumes: `TelemetryServer`, `WorkerState`, `(*WorkerState).online`, `(*WorkerState).usableUsage` (Task 3); `staticHandler()` (`web.go`, existing); `isClientDisconnect` (`serve.go`, existing).
- Produces: `telemetryWorkerView`, `telemetryGroup`, `telemetryView`, `buildTelemetryWorkerView(ws *WorkerState, now time.Time) telemetryWorkerView`, `(*TelemetryServer).buildTelemetryView(selectedID string) telemetryView`, `(*TelemetryServer).registerWebHandlers(mux *http.ServeMux)`. Changes `NewTelemetryServer(token string)` to return `(*TelemetryServer, error)` and adds a `(*TelemetryServer).Handler()` route set that also serves the dashboard.

- [ ] **Step 1: Write the failing test**

```go
// telemetry_web_test.go
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestTelemetryServer(t *testing.T) *TelemetryServer {
	t.Helper()
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pushWorker(t *testing.T, s *TelemetryServer, req PushRequest) {
	t.Helper()
	rec := doPush(t, s.Handler(), "secret", req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("push status = %d", rec.Code)
	}
}

func TestTelemetryIndexGroupsByRepoSlugAndShowsWorkers(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, PushRequest{Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15}})
	pushWorker(t, s, PushRequest{Resource: Resource{MachineID: "m2", RepoSlug: "o/other", Hostname: "host2", PushIntervalSec: 15}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"o/r", "o/other", "host1", "host2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("index body missing %q:\n%s", want, body)
		}
	}
}

func TestTelemetryDetailShowsUsageUnknownWhenAbsent(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, PushRequest{Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15}})

	req := httptest.NewRequest(http.MethodGet, "/detail?worker=m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "unknown") {
		t.Fatalf("detail body missing 'unknown':\n%s", rec.Body.String())
	}
}

func TestTelemetryDetailShowsFreshUsagePercentage(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, PushRequest{
		Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		Usage:    &UsageSnapshot{FiveHourUsedPct: 33.3, CapturedAt: time.Now()},
	})

	req := httptest.NewRequest(http.MethodGet, "/detail?worker=m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "33.3%") {
		t.Fatalf("detail body missing usage percentage:\n%s", rec.Body.String())
	}
}

func TestTelemetryDetailShowsLogTail(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, PushRequest{
		Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		Logs:     []LogRecord{{Body: "watching o/r for label ai-agent"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/detail?worker=m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "watching o/r for label ai-agent") {
		t.Fatalf("detail body missing log line:\n%s", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go build ./... && go test ./... -run TestTelemetry -v`
Expected: FAIL to build — `too many return values` at existing `NewTelemetryServer(...)` call sites, `undefined: telemetryView` etc.

- [ ] **Step 3: Write the implementation**

Update `telemetry_server_test.go`'s three `NewTelemetryServer("secret")` call sites (`TestHandlePushAuth`, `TestHandlePushRequiresMachineID`, `TestHandlePushStoresWorkerAndAppendsLogs`) to handle the new error return:

```go
func TestHandlePushAuth(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	req := PushRequest{Resource: Resource{MachineID: "m1"}}

	if rec := doPush(t, h, "", req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", rec.Code)
	}
	if rec := doPush(t, h, "wrong", req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}
	if rec := doPush(t, h, "secret", req); rec.Code != http.StatusNoContent {
		t.Fatalf("correct token: status = %d, want 204", rec.Code)
	}
}

func TestHandlePushRequiresMachineID(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	rec := doPush(t, s.Handler(), "secret", PushRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandlePushStoresWorkerAndAppendsLogs(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	req1 := PushRequest{
		Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		Logs:     []LogRecord{{Body: "line1"}, {Body: "line2"}},
	}
	if rec := doPush(t, h, "secret", req1); rec.Code != http.StatusNoContent {
		t.Fatalf("push 1 status = %d", rec.Code)
	}
	req2 := PushRequest{
		Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		Logs:     []LogRecord{{Body: "line3"}},
	}
	if rec := doPush(t, h, "secret", req2); rec.Code != http.StatusNoContent {
		t.Fatalf("push 2 status = %d", rec.Code)
	}

	s.mu.Lock()
	ws := s.workers["m1"]
	s.mu.Unlock()
	if ws == nil {
		t.Fatal("worker m1 not stored")
	}
	got := ws.Logs.Lines()
	want := []string{"line1", "line2", "line3"}
	if len(got) != len(want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %v, want %v", got, want)
		}
	}
}
```

In `telemetry_server.go`, add a `tmpl *template.Template` field to `TelemetryServer` and change the constructor:

```go
type TelemetryServer struct {
	token string
	now   func() time.Time
	tmpl  *template.Template

	mu      sync.Mutex
	workers map[string]*WorkerState // keyed by Resource.MachineID
}

// NewTelemetryServer returns a server that authenticates pushes against
// token and has its dashboard templates parsed and ready. It errors only if
// the embedded templates fail to parse, which a passing build already rules
// out — kept as a returned error (rather than a panic) to match NewServer's
// convention in serve.go.
func NewTelemetryServer(token string) (*TelemetryServer, error) {
	tmpl, err := template.New("telemetry").ParseFS(telemetryFS, "telemetry/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &TelemetryServer{token: token, now: time.Now, tmpl: tmpl, workers: map[string]*WorkerState{}}, nil
}
```

Add `"html/template"` to `telemetry_server.go`'s import block.

Update `Handler()` in `telemetry_server.go` to also mount the dashboard:

```go
// Handler returns the telemetry server's HTTP routes: POST /v1/push (worker
// ingest) plus the dashboard routes registered by registerWebHandlers.
func (s *TelemetryServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/push", s.handlePush)
	s.registerWebHandlers(mux)
	return mux
}
```

Create the templates:

```html
<!-- telemetry/templates/page.html -->
{{define "tpage"}}<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>loope // fleet telemetry</title>
<link rel="stylesheet" href="/static/app.css">
<script src="/static/htmx.min.js"></script>
<script src="/static/idiomorph-ext.min.js"></script>
</head>
<body class="font-sans text-text antialiased" hx-ext="morph">
<div class="flex h-screen flex-col">
 <header class="flex shrink-0 items-center gap-3 border-b border-line bg-panel px-5 py-3">
  <span class="font-mono text-[15px] font-semibold tracking-tight text-text">loope<span class="text-live">&middot;</span>fleet</span>
 </header>
 <div class="flex min-h-0 flex-1">
  <nav id="rail" class="scroll w-[340px] shrink-0 overflow-y-auto border-r border-line bg-panel"
       hx-get="/rail?worker={{if .Selected}}{{.Selected.MachineID}}{{end}}" hx-trigger="every 3s" hx-swap="morph:innerHTML">{{template "trail" .}}</nav>
  <main id="main" class="scroll min-w-0 flex-1 overflow-y-auto"
        hx-get="/detail?worker={{if .Selected}}{{.Selected.MachineID}}{{end}}" hx-trigger="every 3s" hx-swap="morph:innerHTML">{{template "tdetail" .}}</main>
 </div>
</div>
</body></html>{{end}}
```

```html
<!-- telemetry/templates/rail.html -->
{{define "trail"}}<div class="p-3">
{{range .Groups}}
 <div class="mb-4">
  <div class="mb-1.5 px-1 font-mono text-[10px] font-semibold uppercase tracking-[0.15em] text-faint">{{.RepoSlug}}</div>
  {{range .Workers}}
   <a href="?worker={{.MachineID}}" hx-get="/detail?worker={{.MachineID}}" hx-target="#main" hx-swap="morph:innerHTML"
      class="mb-1 flex items-center gap-2 rounded border border-line2 bg-panel2 px-2.5 py-2 font-mono text-[11px] text-text hover:border-live/40">
    <span class="inline-block h-2 w-2 rounded-full {{if .Online}}bg-ok{{else}}bg-muted{{end}}"></span>
    <span class="truncate">{{.Hostname}}</span>
    {{if .UsageUnknown}}<span class="ml-auto text-faint">usage: unknown</span>{{else}}<span class="ml-auto tabular-nums">{{printf "%.0f%%" .FiveHourPct}} / 5h</span>{{end}}
   </a>
  {{end}}
 </div>
{{else}}
 <div class="p-4 font-mono text-[12px] text-faint">No workers have reported yet.</div>
{{end}}
</div>{{end}}
```

```html
<!-- telemetry/templates/detail.html -->
{{define "tdetail"}}<div class="max-w-[1000px] px-8 py-6">
{{with .Selected}}
 <div class="mb-4 flex items-center gap-2">
  <span class="inline-block h-2.5 w-2.5 rounded-full {{if .Online}}bg-ok{{else}}bg-muted{{end}}"></span>
  <h1 class="font-mono text-lg font-semibold text-text">{{.Hostname}}</h1>
  <span class="font-mono text-[11px] text-faint">{{.WorkDir}}</span>
  <span class="ml-auto font-mono text-[11px] text-faint">v{{.Version}}</span>
 </div>
 <dl class="mb-5 grid grid-cols-2 gap-px overflow-hidden rounded-md border border-line bg-line">
  <div class="bg-panel px-4 py-3">
   <dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">5-hour usage</dt>
   {{if .UsageUnknown}}<dd class="mt-1.5 font-mono text-sm text-faint">unknown</dd>
   {{else}}<dd class="mt-1.5 font-mono text-xl font-semibold tabular-nums text-text">{{printf "%.1f%%" .FiveHourPct}} <span class="text-[11px] font-normal text-faint">resets {{.FiveHourReset.Format "Jan 2 15:04"}}</span></dd>{{end}}
  </div>
  <div class="bg-panel px-4 py-3">
   <dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">7-day usage</dt>
   {{if .UsageUnknown}}<dd class="mt-1.5 font-mono text-sm text-faint">unknown</dd>
   {{else}}<dd class="mt-1.5 font-mono text-xl font-semibold tabular-nums text-text">{{printf "%.1f%%" .SevenDayPct}} <span class="text-[11px] font-normal text-faint">resets {{.SevenDayReset.Format "Jan 2 15:04"}}</span></dd>{{end}}
  </div>
 </dl>
 <div class="rounded-md border border-line bg-ink/40 p-3 font-mono text-[11px] leading-relaxed text-muted">
  {{range .Logs}}<div>{{.}}</div>{{else}}<div class="text-faint">No log lines yet.</div>{{end}}
 </div>
{{else}}<div class="flex h-full items-center justify-center py-20 font-mono text-[12px] text-faint">No worker selected.</div>{{end}}
</div>{{end}}
```

Create `telemetry_web.go`:

```go
// telemetry_web.go
package main

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"sort"
	"time"
)

//go:embed telemetry/templates
var telemetryFS embed.FS

// telemetryWorkerView is one worker's pre-formatted render data.
type telemetryWorkerView struct {
	MachineID     string
	Hostname      string
	WorkDir       string
	Version       string
	Online        bool
	LastPushAt    time.Time
	UsageUnknown  bool
	FiveHourPct   float64
	FiveHourReset time.Time
	SevenDayPct   float64
	SevenDayReset time.Time
	Logs          []string
}

// telemetryGroup is one repoSlug's workers, sorted by hostname.
type telemetryGroup struct {
	RepoSlug string
	Workers  []telemetryWorkerView
}

// telemetryView is the template payload for one render.
type telemetryView struct {
	Groups   []telemetryGroup
	Selected *telemetryWorkerView
}

// buildTelemetryWorkerView converts one worker's server-side state into the
// template's flat, pre-formatted shape as of now.
func buildTelemetryWorkerView(ws *WorkerState, now time.Time) telemetryWorkerView {
	v := telemetryWorkerView{
		MachineID:  ws.Resource.MachineID,
		Hostname:   ws.Resource.Hostname,
		WorkDir:    ws.Resource.WorkDir,
		Version:    ws.Resource.Version,
		Online:     ws.online(now),
		LastPushAt: ws.LastPushAt,
		Logs:       ws.Logs.Lines(),
	}
	if u := ws.usableUsage(now); u != nil {
		v.FiveHourPct, v.FiveHourReset = u.FiveHourUsedPct, u.FiveHourResetAt
		v.SevenDayPct, v.SevenDayReset = u.SevenDayUsedPct, u.SevenDayResetAt
	} else {
		v.UsageUnknown = true
	}
	return v
}

// buildTelemetryView groups all workers by RepoSlug (sorted), sorts each
// group's workers by hostname, and selects the worker named by selectedID —
// or the first worker of the first group when selectedID is empty or not
// found, or nil when there are no workers at all.
func (s *TelemetryServer) buildTelemetryView(selectedID string) telemetryView {
	now := s.now()
	s.mu.Lock()
	byRepo := map[string][]telemetryWorkerView{}
	for _, ws := range s.workers {
		byRepo[ws.Resource.RepoSlug] = append(byRepo[ws.Resource.RepoSlug], buildTelemetryWorkerView(ws, now))
	}
	s.mu.Unlock()

	var slugs []string
	for slug := range byRepo {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	view := telemetryView{}
	for _, slug := range slugs {
		workers := byRepo[slug]
		sort.Slice(workers, func(i, j int) bool { return workers[i].Hostname < workers[j].Hostname })
		view.Groups = append(view.Groups, telemetryGroup{RepoSlug: slug, Workers: workers})
		for i := range workers {
			if workers[i].MachineID == selectedID {
				view.Selected = &view.Groups[len(view.Groups)-1].Workers[i]
			}
		}
	}
	if view.Selected == nil && len(view.Groups) > 0 && len(view.Groups[0].Workers) > 0 {
		view.Selected = &view.Groups[0].Workers[0]
	}
	return view
}

// registerWebHandlers wires the dashboard routes onto mux: GET / (full
// page), GET /rail and GET /detail (htmx poll fragments), and the static
// assets shared with the per-repo dashboard (staticHandler, from web.go).
func (s *TelemetryServer) registerWebHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.handleTelemetryIndex)
	mux.HandleFunc("GET /rail", s.handleTelemetryRail)
	mux.HandleFunc("GET /detail", s.handleTelemetryDetail)
	mux.Handle("GET /static/", staticHandler())
}

func (s *TelemetryServer) handleTelemetryIndex(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryView(r.URL.Query().Get("worker"))
	renderTelemetryHTML(w, s.tmpl, "tpage", v)
}

func (s *TelemetryServer) handleTelemetryRail(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryView(r.URL.Query().Get("worker"))
	renderTelemetryHTML(w, s.tmpl, "trail", v)
}

func (s *TelemetryServer) handleTelemetryDetail(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryView(r.URL.Query().Get("worker"))
	renderTelemetryHTML(w, s.tmpl, "tdetail", v)
}

// renderTelemetryHTML mirrors serve.go's renderHTML: it buffers the render
// so a template error still yields a clean 500, and swallows client-
// disconnect write errors from the 3s poll instead of logging them as noise.
func renderTelemetryHTML(w http.ResponseWriter, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil && !isClientDisconnect(err) {
		_ = err // best-effort write; a disconnect is expected under the 3s poll
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go build ./... && go test ./... -run TestTelemetry -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telemetry_web.go telemetry/templates/page.html telemetry/templates/rail.html telemetry/templates/detail.html telemetry_server.go telemetry_server_test.go telemetry_web_test.go
git commit -m "feat(telemetry): add fleet dashboard templates and render handlers"
```

---

### Task 10: `loope telemetry-server` subcommand

**Files:**
- Create: `telemetry_cmd.go`
- Test: `telemetry_cmd_test.go`

**Interfaces:**
- Consumes: `NewTelemetryServer(token string) (*TelemetryServer, error)` (Task 9).
- Produces: `parseTelemetryServerFlags(args []string) (addr, token, dataDir string, err error)`, `runTelemetryServerCmd(args []string) int`.

- [ ] **Step 1: Write the failing test**

```go
// telemetry_cmd_test.go
package main

import "testing"

func TestParseTelemetryServerFlagsDefaults(t *testing.T) {
	addr, token, dataDir, err := parseTelemetryServerFlags([]string{"-token", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if addr != ":9090" {
		t.Fatalf("addr = %q, want the :9090 default", addr)
	}
	if token != "secret" {
		t.Fatalf("token = %q", token)
	}
	if dataDir != "" {
		t.Fatalf("dataDir = %q, want empty by default", dataDir)
	}
}

func TestParseTelemetryServerFlagsOverrides(t *testing.T) {
	addr, token, dataDir, err := parseTelemetryServerFlags([]string{"-addr", ":9999", "-token", "secret", "-data-dir", "/tmp/telemetry"})
	if err != nil {
		t.Fatal(err)
	}
	if addr != ":9999" || token != "secret" || dataDir != "/tmp/telemetry" {
		t.Fatalf("addr=%q token=%q dataDir=%q", addr, token, dataDir)
	}
}

func TestParseTelemetryServerFlagsRequiresToken(t *testing.T) {
	if _, _, _, err := parseTelemetryServerFlags([]string{}); err == nil {
		t.Fatal("expected an error when -token is not given")
	}
}

func TestRunTelemetryServerCmdReturns2WithoutToken(t *testing.T) {
	if code := runTelemetryServerCmd([]string{}); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestParseTelemetryServerFlags|TestRunTelemetryServerCmd' -v`
Expected: FAIL — `undefined: parseTelemetryServerFlags`

- [ ] **Step 3: Write the implementation**

```go
// telemetry_cmd.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// parseTelemetryServerFlags parses `loope telemetry-server`'s flags. token
// is required — an empty value is treated as a parse error so the caller
// exits before ever listening without auth configured.
func parseTelemetryServerFlags(args []string) (addr, token, dataDir string, err error) {
	fs := flag.NewFlagSet("telemetry-server", flag.ContinueOnError)
	a := fs.String("addr", ":9090", "address to listen on")
	t := fs.String("token", "", "shared bearer token workers authenticate with (required)")
	d := fs.String("data-dir", "", "reserved for future persistence; created if given, but unused today")
	if perr := fs.Parse(args); perr != nil {
		return "", "", "", perr
	}
	if *t == "" {
		return "", "", "", fmt.Errorf("-token is required")
	}
	return *a, *t, *d, nil
}

// runTelemetryServerCmd implements `loope telemetry-server`: parse flags,
// start the fleet dashboard/ingest HTTP server, and block until a shutdown
// signal. Returns the process exit code.
func runTelemetryServerCmd(args []string) int {
	addr, token, dataDir, err := parseTelemetryServerFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry-server: %v\n", err)
		return 2
	}
	if dataDir != "" {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "telemetry-server: data-dir: %v\n", err)
			return 1
		}
	}

	srv, err := NewTelemetryServer(token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry-server: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		httpSrv.Close()
	}()
	log.Printf("telemetry server on http://%s", addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "telemetry-server: %v\n", err)
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestParseTelemetryServerFlags|TestRunTelemetryServerCmd' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add telemetry_cmd.go telemetry_cmd_test.go
git commit -m "feat(telemetry): add loope telemetry-server subcommand"
```

---

### Task 11: Wire subcommands and the exporter into `main.go`

**Files:**
- Modify: `main.go`
- Test: `main_test.go` (append)

**Interfaces:**
- Consumes: `runTelemetryServerCmd` (Task 10), `runClaudeUsageHookCmd` (Task 6), `NewRotatingFile`, `rotatingFileMaxBytes` (Task 4), `NewTelemetryExporter` (Task 8), `Config.Telemetry` (Task 7).
- Produces: `dispatchSubcommand(args []string, stdin io.Reader) (code int, handled bool)`.

- [ ] **Step 1: Write the failing test**

Append to `main_test.go`:

```go
func TestDispatchSubcommandTelemetryServerRequiresToken(t *testing.T) {
	code, handled := dispatchSubcommand([]string{"loope", "telemetry-server"}, strings.NewReader(""))
	if !handled {
		t.Fatal("expected telemetry-server to be handled")
	}
	if code != 2 {
		t.Fatalf("code = %d, want 2 (missing -token)", code)
	}
}

func TestDispatchSubcommandClaudeUsageHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	input := `{"rate_limits":{"five_hour":{"used_percentage":10},"seven_day":{"used_percentage":20}}}`
	code, handled := dispatchSubcommand([]string{"loope", "claude-usage-hook"}, strings.NewReader(input))
	if !handled {
		t.Fatal("expected claude-usage-hook to be handled")
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
}

func TestDispatchSubcommandFallsThroughForDaemonInvocation(t *testing.T) {
	for _, args := range [][]string{{"loope"}, {"loope", "--config", "x.json"}, {"loope", "--version"}} {
		if _, handled := dispatchSubcommand(args, strings.NewReader("")); handled {
			t.Fatalf("args %v: expected handled=false", args)
		}
	}
}
```

Add `"strings"` to `main_test.go`'s imports if not already present (it is, per the existing `TestGuardConvertsPanicToError` file — verify with `go build ./...` after the edit).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestDispatchSubcommand -v`
Expected: FAIL — `undefined: dispatchSubcommand`

- [ ] **Step 3: Write the implementation**

In `main.go`, add `"path/filepath"` to the import block (it currently has `bytes, context, errors, flag, fmt, io, log, net/http, os, os/signal, runtime/debug, syscall, time` — `io` is already present).

Replace the start of `func main()`:

```go
func main() {
	fs := flag.NewFlagSet("loope", flag.ContinueOnError)
```

with:

```go
func main() {
	if code, handled := dispatchSubcommand(os.Args, os.Stdin); handled {
		os.Exit(code)
	}

	fs := flag.NewFlagSet("loope", flag.ContinueOnError)
```

Add the dispatch function right after `main`'s closing brace (or anywhere at package scope in `main.go`):

```go
// dispatchSubcommand handles the `telemetry-server` and `claude-usage-hook`
// subcommands, which take over the whole process instead of running the
// daemon. handled is false for every other invocation (the normal
// --config-driven daemon, bare --version/--help), so main falls through to
// its existing flag-based dispatch unchanged.
func dispatchSubcommand(args []string, stdin io.Reader) (code int, handled bool) {
	if len(args) < 2 {
		return 0, false
	}
	switch args[1] {
	case "telemetry-server":
		return runTelemetryServerCmd(args[2:]), true
	case "claude-usage-hook":
		return runClaudeUsageHookCmd(stdin), true
	}
	return 0, false
}
```

Wire the daemon's own log rotation and the exporter goroutine. Change:

```go
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
```

to:

```go
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	daemonLogPath := filepath.Join(cfg.WorkDir, "logs", "daemon.log")
	logFile, err := NewRotatingFile(daemonLogPath, rotatingFileMaxBytes)
	if err != nil {
		log.Fatalf("daemon log: %v", err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
```

Then, right after the dashboard's `go func() { ... }()` block that starts `httpSrv.ListenAndServe()` and before the final `runLoop(...)` call, add the opt-in exporter start:

```go
	if cfg.Telemetry != nil {
		exp := NewTelemetryExporter(cfg, daemonLogPath)
		go exp.Run(ctx)
	}

	runLoop(ctx, o, cfg, true /* sweep */)
```

(replacing the previous bare `runLoop(ctx, o, cfg, true /* sweep */)` line).

- [ ] **Step 4: Run test to verify it passes**

Run: `go build ./... && go test ./... -v`
Expected: PASS — the whole suite builds and passes, including every earlier task's tests.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(telemetry): wire subcommand dispatch, log rotation, and the exporter into main"
```

---

### Task 12: User-facing documentation

**Files:**
- Create: `docs/telemetry.md`
- Modify: `README.md`

**Interfaces:**
- None (docs only).

- [ ] **Step 1: Write the doc**

```markdown
<!-- docs/telemetry.md -->
# Fleet telemetry server

Running `loope` across several machines or repos normally means opening N
terminals or N per-repo dashboards, with no single place to see which workers
are online, what they're doing, or how close each is to its Claude Code usage
limit. `loope telemetry-server` is a central, opt-in view across all of them.

## Running the server

```bash
loope telemetry-server -addr :9090 -token your-shared-secret
```

`-token` is required — it is the shared bearer token every worker must send.
`-addr` defaults to `:9090`. `-data-dir` is accepted for forward compatibility
with future persistence but is not used today: the telemetry server's state is
entirely in-memory, so a restart shows a blank fleet until workers' next push
repopulates it (matching `loope`'s "state lives in GitHub / gets rebuilt"
philosophy for its per-repo dashboard).

Open `http://<host>:9090` for the fleet dashboard: workers are grouped by
`repoSlug`, each with an online/offline badge, current 5-hour/7-day Claude
usage, and a live tail of its daemon log.

## Opting a worker in

Add a `telemetry` block to that worker's `loope.json`:

```json
"telemetry": {
  "serverURL": "http://telemetry-host:9090",
  "token": "your-shared-secret",
  "pushIntervalSec": 15
}
```

`pushIntervalSec` defaults to 15 if omitted. When the block is absent
entirely, nothing changes: no exporter goroutine starts, and no extra network
calls are made. When present, the daemon also starts writing its own log
output to `<workDir>/logs/daemon.log` (in addition to stderr), rotating at
10MB with one previous generation kept — this is what the exporter tails and
pushes.

## Capturing Claude usage (optional)

The 5-hour/7-day usage numbers come from the JSON Claude Code feeds to a
configured `statusLine` command. `loope` does not modify your
`~/.claude/settings.json` automatically — wire it in yourself by teeing the
same stdin into `loope claude-usage-hook` alongside your existing statusline
script, e.g.:

```bash
# ~/.claude/settings.json
"statusLine": {
  "type": "command",
  "command": "sh -c 'tee >(loope claude-usage-hook) | /path/to/your/real-statusline.sh'"
}
```

`loope claude-usage-hook` writes the latest rate-limit snapshot to
`~/.claude/loope-usage.json` and prints nothing, so it never affects what your
real statusline displays. If this file is missing, or its capture is older
than 30 minutes, the dashboard shows "usage: unknown" for that worker rather
than a stale or fabricated number — whether headless `claude -p` runs (how
loope's own pipeline steps invoke Claude) trigger the statusLine hook the same
way interactive sessions do is unconfirmed; the degraded "unknown" state is
what you'll see until that's verified on your setup.
```

- [ ] **Step 2: Add the row to `README.md`'s documentation table**

In `README.md`, change:

```markdown
| [Dashboard](docs/dashboard.md)           | The live web dashboard and its embedded assets |
| [Operations](docs/operations.md)         | Always-on behavior, failure handling, running as a launchd service |
```

to:

```markdown
| [Dashboard](docs/dashboard.md)           | The live web dashboard and its embedded assets |
| [Fleet telemetry](docs/telemetry.md)     | `loope telemetry-server`, worker opt-in, and Claude usage capture |
| [Operations](docs/operations.md)         | Always-on behavior, failure handling, running as a launchd service |
```

- [ ] **Step 3: Verify the whole suite still passes**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS (docs-only change; this just confirms nothing else broke).

- [ ] **Step 4: Commit**

```bash
git add docs/telemetry.md README.md
git commit -m "docs: document the fleet telemetry server"
```

---

## Final verification

After Task 12, run the full suite once more from the repo root:

```bash
go build ./...
go vet ./...
go test ./...
```

All three must be clean. Also manually smoke-test the two new subcommands:

```bash
# Terminal 1
go run . telemetry-server -addr :9090 -token demo

# Terminal 2 — simulate a worker push
curl -i -X POST http://localhost:9090/v1/push \
  -H "Authorization: Bearer demo" -H "Content-Type: application/json" \
  -d '{"resource":{"repoSlug":"o/r","machineID":"abc123def456","hostname":"laptop","workDir":"/tmp/w","version":"dev","pushIntervalSec":15},"logs":[{"body":"hello from a worker"}]}'
# Expect: HTTP/1.1 204 No Content

# Then open http://localhost:9090 and confirm the o/r group, laptop worker,
# online badge, "usage: unknown", and the "hello from a worker" log line all render.
```
