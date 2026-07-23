# Stop / Continue a Ticket Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the operator manual `stop` and `continue` control over a ticket's lifecycle, from both the CLI and the dashboard, preserving all progress (worktree, branch, logs, Claude session id) across the stop.

**Architecture:** A new durable state `ai-stopped` plus an on-disk `logs/issue-<N>/stop` marker records the operator's hold; an in-process `runRegistry` maps issue number → `context.CancelFunc` so a live pipeline can be halted immediately, and a 2-second `watchStops` poller lets a stop issued from another process reach a pipeline this daemon owns. Continue re-uses the existing rework machinery: `Orchestrator.Rework` is split into a shared `resume` body that continue calls with `fromLabel = WIP`.

**Tech Stack:** Go 1.25 (stdlib only — no third-party deps; `go.mod` has no requires), `html/template` dashboard rendered server-side with Tailwind via CDN, `gh`/`git`/`claude` shelled through the `Runner` interface, table-driven tests with `fakeRunner`/`fakeEnv` from `helpers_test.go` and `loop_test.go`.

## Global Constraints

- **Package:** everything is `package main` in the repo root. No new directories, no new modules.
- **No new dependencies.** `go.mod` declares only `module loope`; keep it that way.
- **Stdlib only, Go 1.25+.**
- **Never delete state to recover** (`CLAUDE.md`): stop preserves worktree, branch, logs, and the session file. Recovery logic lives in error branches; the happy path is untouched.
- **Best-effort marker writers**: `recordStopRequest` / `clearStopRequest` swallow errors and no-op on empty input, exactly like `recordState` / `recordParkCause` in `tracker.go`.
- **Label constant:** `labelStopped = "ai-stopped"`, JSON key `stopped`, on `StateLabels`.
- **Marker filename:** `stop`, in `logs/issue-<N>/`, alongside `state` / `park-cause` / `session`.
- **CLI flags are flat** (no subcommands): `-stop <N>`, `-continue <N>`, matching `-rework <N>`.
- **Dashboard mutating routes are `POST`-only** and unauthenticated; `-addr` stays `localhost`-bound by default.
- **Exact user-facing strings** (copy verbatim):
  - stop comment: ``🤖 Stopped by request. Progress is preserved — continue with `loope -continue <N>` or the dashboard.``
  - `stopping #N (halting the running session)`
  - `stop requested for #N — the running daemon will halt it shortly`
  - `stopped #N`
  - `#N is already running`
  - `#N is not stopped`
- **Tests:** run with `go test ./...` from the repo root. Every task ends green.

## Assumptions (spec gaps resolved here)

1. **`Controller` vs `Orchestrator.Stop(ctx, n)`.** The spec gives `Orchestrator.Stop(ctx, n) error` *and* a `Controller` interface with `Stop(n int) error`. Both cannot be the same method. Resolution: `Orchestrator` keeps the context-taking `Stop(ctx, n)` / `Continue(ctx, n)`, and a tiny `orchestratorController` adapter (in `control.go`) implements `Controller` by supplying `o.baseCtx`. `main` passes the adapter to `NewServer`. This keeps every signature the spec names and keeps `serve.go` ignorant of pipelines.
2. **`watchStops` tick injection.** `watchStops(ctx context.Context, every time.Duration)`; `main` passes `2 * time.Second`, tests pass a millisecond tick.
3. **Continue's async split.** `Orchestrator.prepareContinue(ctx, n) (run func(context.Context) error, err error)` does all synchronous validation and label work and returns the (possibly nil) resume closure. `Continue` calls it and runs the closure inline; the controller runs it in a goroutine on `baseCtx`. This is how "validate synchronously, resume asynchronously" is satisfied without duplicating validation.
4. **`stateKind` for a stopped-but-unlabeled ticket.** The local `state` file is authoritative for the dashboard (see `overlayIssues`), so no extra plumbing is needed: `recordState(logDir, Stopped)` makes the pane show `stopped` immediately.
5. **Sniffer `Kind`.** `ClaudeCall.Kind` mirrors the kind already passed to `RecordSession` at each call site: `"bug"` in `pipeline_bug.go`, `"feature"` in `pipeline_feature.go`, `si.Kind` in the resume body. Triage / answerer / done-confirm calls leave it empty.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `control.go` | **create** | `runRegistry`, `watchStops`, `Stop`, `finishStopped`, `prepareContinue`, `Continue`, `orchestratorController` |
| `control_test.go` | **create** | Tests for all of the above |
| `config.go` | modify | `labelStopped`, `StateLabels.Stopped`, `defaultStateLabels` |
| `github.go` | modify | `hasStateLabel` recognises Stopped |
| `tracker.go` | modify | `stopFile` marker helpers; `trackedStateLabels`; `pickStateLabel` |
| `serve.go` | modify | `stateKind`/`stripeClass` stopped case, `Controller`, `NewServer` signature, POST routes, buttons |
| `runner.go` | modify | SIGTERM-then-SIGKILL cancellation |
| `claude.go` | modify | `ClaudeCall.Kind`, `sessionSniffer` |
| `rework.go` | modify | split `Rework` into shared `resume` |
| `loop.go` | modify | `Orchestrator.registry`/`baseCtx` fields, `handleIssue` wrapping + stop branch, `shouldResume`, `SweepOrphans` |
| `main.go` | modify | `-stop` / `-continue` flags, `baseCtx`, `watchStops` goroutine, controller wiring |
| `README.md` | modify | label table, `gh label create`, flag docs, localhost warning |
| `claude_test.go`, `loop_test.go`, `serve_test.go`, `config_test.go` | modify | extended coverage |

---

## Task 1: The `ai-stopped` state label

**Files:**
- Modify: `config.go:12-18` (label constants), `config.go:62-72` (`StateLabels`, `defaultStateLabels`)
- Modify: `github.go:97-105` (`hasStateLabel`)
- Modify: `tracker.go:481-483` (`trackedStateLabels`), `tracker.go:498-515` (`pickStateLabel`)
- Modify: `serve.go:405-436` (`stateKind`, `stripeClass`)
- Test: `config_test.go`, `tracker_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `labelStopped` constant (`"ai-stopped"`); `StateLabels.Stopped string` with JSON key `stopped`; `stateKind` returns `"stopped"` for it; `stripeClass` returns `"bg-muted/40"`.

- [ ] **Step 1: Write the failing tests**

Append to `config_test.go`:

```go
func TestDefaultStateLabelsIncludesStopped(t *testing.T) {
	if got := defaultStateLabels().Stopped; got != "ai-stopped" {
		t.Fatalf("default stopped label = %q, want ai-stopped", got)
	}
}

func TestLoadConfigStoppedLabelOverridable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loope.json")
	body := `{"repoPath":"/r","repoSlug":"o/r","workDir":"` + dir + `","stateLabels":{"stopped":"held"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateLabels.Stopped != "held" {
		t.Fatalf("stopped = %q, want held", cfg.StateLabels.Stopped)
	}
}

func TestHasStateLabelTreatsStoppedAsState(t *testing.T) {
	cfg := &Config{RepoSlug: "o/r", StateLabels: defaultStateLabels()}
	g := NewGitHub(&fakeRunner{}, cfg)
	is := Issue{Number: 7, Labels: []Label{{Name: "ai-agent"}, {Name: "ai-stopped"}}}
	if !g.hasStateLabel(is) {
		t.Fatal("ai-stopped must count as a state label so a stopped issue leaves the eligible queue")
	}
}
```

Append to `tracker_test.go`:

```go
func TestPickStateLabelStoppedBeatsReworkAndDone(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	labels := []Label{{Name: "ai-agent"}, {Name: "ai-rework"}, {Name: "ai-stopped"}}
	if got := pickStateLabel(labels, cfg); got != "ai-stopped" {
		t.Fatalf("pickStateLabel = %q, want ai-stopped", got)
	}
}

func TestPickStateLabelWIPBeatsStopped(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	labels := []Label{{Name: "ai-stopped"}, {Name: "ai-wip"}}
	if got := pickStateLabel(labels, cfg); got != "ai-wip" {
		t.Fatalf("pickStateLabel = %q, want ai-wip", got)
	}
}

func TestTrackedStateLabelsIncludesStopped(t *testing.T) {
	cfg := &Config{StateLabels: defaultStateLabels()}
	found := false
	for _, l := range trackedStateLabels(cfg) {
		if l == "ai-stopped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trackedStateLabels = %v, want it to include ai-stopped", trackedStateLabels(cfg))
	}
}

func TestStateKindAndStripeForStopped(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	if got := stateKind(cfg, "ai-stopped"); got != "stopped" {
		t.Fatalf("stateKind = %q, want stopped", got)
	}
	if got := stripeClass(cfg, "ai-stopped"); got != "bg-muted/40" {
		t.Fatalf("stripeClass = %q, want bg-muted/40", got)
	}
}
```

`config_test.go` must import `os` and `path/filepath` — check its existing import block and add only what is missing.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'Stopped|StateKindAndStripe' -v`
Expected: compile failure — `cfg.StateLabels.Stopped undefined`.

- [ ] **Step 3: Add the label constant and struct field**

In `config.go`, extend the const block:

```go
const (
	labelWIP       = "ai-wip"
	labelFailed    = "ai-failed"
	labelDone      = "ai-done"
	labelRework    = "ai-rework"
	labelNeedsInfo = "ai-needs-info"
	labelStopped   = "ai-stopped"
)
```

Extend `StateLabels` and its default:

```go
// StateLabels are the labels the loop applies to track issue state.
// Unset fields fall back to the ai-wip/ai-failed/ai-done defaults.
type StateLabels struct {
	WIP       string `json:"wip"`
	Failed    string `json:"failed"`
	Done      string `json:"done"`
	Rework    string `json:"rework"`
	NeedsInfo string `json:"needsInfo"`
	// Stopped is the operator-held state: work is halted and all progress
	// preserved, and only an explicit continue moves the issue out of it. It is
	// deliberately NOT ai-rework, which the daemon auto-resumes.
	Stopped string `json:"stopped"`
}

func defaultStateLabels() StateLabels {
	return StateLabels{WIP: labelWIP, Failed: labelFailed, Done: labelDone, Rework: labelRework, NeedsInfo: labelNeedsInfo, Stopped: labelStopped}
}
```

- [ ] **Step 4: Thread the label through every enumerator**

`github.go`:

```go
func (g *GitHub) hasStateLabel(is Issue) bool {
	for _, l := range is.Labels {
		if l.Name == g.state.WIP || l.Name == g.state.Failed || l.Name == g.state.Done ||
			l.Name == g.state.Rework || l.Name == g.state.NeedsInfo || l.Name == g.state.Stopped {
			return true
		}
	}
	return false
}
```

`tracker.go`:

```go
// trackedStateLabels returns the loop's board states (wip/stopped/rework/done).
// It is only the fallback search set for a config with no eligible label; the
// normal path scopes the fetch to the eligible label instead (see
// listTrackedIssues).
func trackedStateLabels(cfg *Config) []string {
	return []string{cfg.StateLabels.WIP, cfg.StateLabels.Done, cfg.StateLabels.Rework, cfg.StateLabels.Stopped}
}
```

and in `pickStateLabel`, change only the priority slice:

```go
	for _, name := range []string{cfg.StateLabels.WIP, cfg.StateLabels.Stopped, cfg.StateLabels.Rework, cfg.StateLabels.Done, cfg.EligibleLabel} {
```

Update `pickStateLabel`'s doc comment to read `priority order WIP > Stopped > Rework > Done > eligible`.

`serve.go`:

```go
func stateKind(cfg *Config, label string) string {
	switch label {
	case cfg.StateLabels.Done:
		return "done"
	case cfg.StateLabels.WIP:
		return "wip"
	case cfg.StateLabels.Rework:
		return "rework"
	case cfg.StateLabels.Stopped:
		return "stopped"
	case cfg.StateLabels.Failed:
		return "failed"
	case cfg.EligibleLabel:
		return "queued"
	default:
		return ""
	}
}
```

and in `stripeClass`, add before `default`:

```go
	case "stopped":
		return "bg-muted/40"
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./...`
Expected: `ok  	loope`

- [ ] **Step 6: Commit**

```bash
git add config.go github.go tracker.go serve.go config_test.go tracker_test.go
git commit -m "feat: add the ai-stopped state label"
```

---

## Task 2: The stop marker file

**Files:**
- Modify: `tracker.go` (append after `clearParkCause`, around `tracker.go:253`)
- Test: `tracker_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `const stopFile = "stop"`; `recordStopRequest(logDir string)`, `stopRequested(logDir string) bool`, `clearStopRequest(logDir string)`.

- [ ] **Step 1: Write the failing test**

Append to `tracker_test.go`:

```go
func TestStopMarkerLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "issue-7")
	if stopRequested(dir) {
		t.Fatal("no marker written yet, stopRequested should be false")
	}
	recordStopRequest(dir)
	if !stopRequested(dir) {
		t.Fatal("after recordStopRequest, stopRequested should be true")
	}
	b, err := os.ReadFile(filepath.Join(dir, stopFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err != nil {
		t.Fatalf("marker content %q is not an RFC3339 timestamp: %v", b, err)
	}
	clearStopRequest(dir)
	if stopRequested(dir) {
		t.Fatal("after clearStopRequest, stopRequested should be false")
	}
}

func TestStopMarkerEmptyDirIsNoOp(t *testing.T) {
	recordStopRequest("")
	clearStopRequest("")
	if stopRequested("") {
		t.Fatal("empty logDir must never report a stop")
	}
}
```

Ensure `tracker_test.go` imports `os`, `path/filepath`, `strings`, `time` (add any missing).

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./... -run TestStopMarker -v`
Expected: FAIL — `undefined: recordStopRequest`.

- [ ] **Step 3: Implement the marker helpers**

Append to `tracker.go` after `clearParkCause`:

```go
// stopFile marks an issue as operator-held: work is halted and only an explicit
// continue resumes it. Unlike the state/park-cause markers, which only mirror a
// transition, this one is load-bearing in three ways: it tells a running
// pipeline that its cancelled context was a stop rather than a daemon shutdown;
// it carries a stop issued from another process to the daemon that owns the
// run; and it survives a daemon restart so the orphan sweep recovers the issue
// as stopped instead of parking it for auto-resume. Only the file's EXISTENCE
// is load-bearing — its RFC3339 content is for human postmortems.
const stopFile = "stop"

// recordStopRequest writes the stop marker for an issue. Best-effort, like the
// other log-writers: a no-op on an empty dir and errors are swallowed.
func recordStopRequest(logDir string) {
	if logDir == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, stopFile), []byte(time.Now().Format(time.RFC3339)), 0o644)
}

// stopRequested reports whether the issue is operator-held.
func stopRequested(logDir string) bool {
	if logDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(logDir, stopFile))
	return err == nil
}

// clearStopRequest removes the stop marker. Called by continue — never by the
// stop completing, since the marker is the durable record of the hold.
func clearStopRequest(logDir string) {
	if logDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(logDir, stopFile))
}
```

`tracker.go` already imports `os`, `path/filepath`, and `time`; verify with `head -20 tracker.go` and add `time` if absent.

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./... -run TestStopMarker -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add tracker.go tracker_test.go
git commit -m "feat: add the per-issue stop marker file"
```

---

## Task 3: The live-run registry

**Files:**
- Create: `control.go`
- Create: `control_test.go`
- Modify: `loop.go:16-29` (`Orchestrator` fields)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type runRegistry struct { mu sync.Mutex; live map[int]context.CancelFunc }`
  - `(*runRegistry) register(n int, cancel context.CancelFunc) bool`
  - `(*runRegistry) deregister(n int)`
  - `(*runRegistry) cancel(n int) bool`
  - `(*runRegistry) running(n int) bool`
  - `(*runRegistry) numbers() []int`
  - `Orchestrator.registry runRegistry` (value field, zero-usable)

- [ ] **Step 1: Write the failing test**

Create `control_test.go`:

```go
package main

import (
	"context"
	"testing"
)

func TestRunRegistryRegisterCancelDeregister(t *testing.T) {
	var reg runRegistry
	_, cancel := context.WithCancel(context.Background())
	cancelled := false
	wrapped := func() { cancelled = true; cancel() }

	if !reg.register(7, wrapped) {
		t.Fatal("first register should succeed")
	}
	if !reg.running(7) {
		t.Fatal("registered issue should report running")
	}
	if reg.register(7, wrapped) {
		t.Fatal("second register of the same issue must be refused")
	}
	if !reg.cancel(7) {
		t.Fatal("cancel of a registered issue should report found")
	}
	if !cancelled {
		t.Fatal("cancel must invoke the registered cancel func")
	}
	reg.deregister(7)
	if reg.running(7) {
		t.Fatal("deregistered issue must not report running")
	}
	if reg.cancel(7) {
		t.Fatal("cancel of an unregistered issue should report not found")
	}
}

func TestRunRegistryNumbers(t *testing.T) {
	var reg runRegistry
	reg.register(3, func() {})
	reg.register(9, func() {})
	got := reg.numbers()
	if len(got) != 2 {
		t.Fatalf("numbers() = %v, want two entries", got)
	}
	seen := map[int]bool{}
	for _, n := range got {
		seen[n] = true
	}
	if !seen[3] || !seen[9] {
		t.Fatalf("numbers() = %v, want 3 and 9", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./... -run TestRunRegistry -v`
Expected: FAIL — `undefined: runRegistry`.

- [ ] **Step 3: Create control.go with the registry**

```go
package main

import (
	"context"
	"sync"
)

// runRegistry tracks the cancel func of every pipeline running in this process,
// keyed by issue number, so a stop request can halt one immediately. It is the
// in-memory half of the stop mechanism; the on-disk stop marker is the half
// that crosses process boundaries and restarts.
type runRegistry struct {
	mu   sync.Mutex
	live map[int]context.CancelFunc
}

// register claims issue n for a pipeline in this process. It returns false when
// the issue is already registered, which is what stops a continue from starting
// a second Claude session in a worktree one is already running in.
func (r *runRegistry) register(n int, cancel context.CancelFunc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live == nil {
		r.live = map[int]context.CancelFunc{}
	}
	if _, ok := r.live[n]; ok {
		return false
	}
	r.live[n] = cancel
	return true
}

// deregister releases issue n. Always called via defer by the pipeline that
// registered it, so a panicking run still frees its slot.
func (r *runRegistry) deregister(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, n)
}

// cancel halts issue n's pipeline if it is running in this process, reporting
// whether one was found. The entry is left in place: the pipeline goroutine
// deregisters as it unwinds.
func (r *runRegistry) cancel(n int) bool {
	r.mu.Lock()
	fn, ok := r.live[n]
	r.mu.Unlock()
	if !ok {
		return false
	}
	fn()
	return true
}

// running reports whether issue n has a pipeline live in this process.
func (r *runRegistry) running(n int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.live[n]
	return ok
}

// numbers returns the issue numbers currently registered. watchStops iterates
// this, so a quiet daemon does one os.Stat per live pipeline per tick and
// nothing else.
func (r *runRegistry) numbers() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(r.live))
	for n := range r.live {
		out = append(out, n)
	}
	return out
}
```

- [ ] **Step 4: Add the registry and baseCtx fields to Orchestrator**

In `loop.go`, extend the struct (keep every existing field and comment):

```go
type Orchestrator struct {
	cfg    *Config
	runner Runner
	gh     *GitHub
	wt     *Worktree

	// registry holds the cancel func of every pipeline live in this process, so
	// a stop request can halt one immediately.
	registry runRegistry

	// baseCtx is the daemon's lifetime context, set by main. Work started from
	// a short-lived caller (an HTTP request) runs on it instead, so a continue
	// survives its HTTP response and dies with the daemon. nil means
	// context.Background().
	baseCtx context.Context

	// Auto-resume bookkeeping: per-issue backoff between resume attempts and
	// once-per-process skip logging. In-memory only — a restart retrying
	// immediately costs at most one extra attempt.
	mu            sync.Mutex
	resumeBackoff map[int]backoffState
	skipLogged    map[int]bool
	now           func() time.Time // test seam; nil means time.Now
}
```

Add the accessor just below `clock()` in `loop.go`:

```go
// base returns the daemon-lifetime context for work that must outlive its
// caller. Defaults to Background so tests and one-shot CLI paths work unset.
func (o *Orchestrator) base() context.Context {
	if o.baseCtx != nil {
		return o.baseCtx
	}
	return context.Background()
}
```

- [ ] **Step 5: Run it to verify it passes**

Run: `go test ./...`
Expected: `ok  	loope`

- [ ] **Step 6: Commit**

```bash
git add control.go control_test.go loop.go
git commit -m "feat: add the live-run registry"
```

---

## Task 4: Graceful process termination

**Files:**
- Modify: `runner.go:26-56` (both `execRunner` methods)
- Test: `runner_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: no new symbols — `execRunner.Run`/`RunStream` now SIGTERM on cancellation and escalate to SIGKILL after `runnerWaitDelay`.

- [ ] **Step 1: Write the failing test**

Append to `runner_test.go`:

```go
// A cancelled command must be asked to exit with SIGTERM first, so claude gets
// a chance to flush its session transcript before it dies. The trap makes the
// shell exit 0 on SIGTERM; a SIGKILL would surface as a signal error instead.
func TestExecRunnerCancelSendsSIGTERM(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := execRunner{}.Run(ctx, "", nil, "", "sh", "-c", `trap 'exit 0' TERM; sleep 5 & wait`)
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled command should have exited cleanly on SIGTERM, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled command did not exit")
	}
}
```

Add `context` and `time` to `runner_test.go`'s imports if missing.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./... -run TestExecRunnerCancelSendsSIGTERM -v`
Expected: FAIL — the error is non-nil (`signal: killed`), because `exec.CommandContext` defaults to SIGKILL.

- [ ] **Step 3: Implement graceful cancellation**

In `runner.go`, add the constant and helper, and call it from both methods:

```go
// runnerWaitDelay is how long a cancelled process has to exit on SIGTERM before
// exec escalates to SIGKILL. Ten seconds is enough for claude to flush its
// session transcript, which is what makes a stop resumable.
const runnerWaitDelay = 10 * time.Second

// gracefulCancel makes ctx cancellation send SIGTERM rather than the SIGKILL
// exec.CommandContext defaults to, escalating only if the process is still
// alive after runnerWaitDelay. This matters for `claude`: a SIGKILL mid-call
// loses the session transcript, so a stopped run could not be continued.
func gracefulCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = runnerWaitDelay
}
```

In `Run`, immediately after `cmd := exec.CommandContext(...)`:

```go
	gracefulCancel(cmd)
```

Do the same in `RunStream`. Add `"syscall"` and `"time"` to `runner.go`'s imports.

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./... -run TestExecRunner -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add runner.go runner_test.go
git commit -m "feat: terminate cancelled processes with SIGTERM before SIGKILL"
```

---

## Task 5: Live session capture

**Files:**
- Modify: `claude.go:72-137` (`ClaudeCall`, `Call`), append `sessionSniffer` after `RecordSession`
- Modify: `pipeline_bug.go:9-14`, `pipeline_feature.go:27-32`, `pipeline_feature.go:119-124`, `pipeline_feature.go:142-147`
- Test: `claude_test.go`

**Interfaces:**
- Consumes: `Claude.RecordSession(id, kind string)` (existing).
- Produces: `ClaudeCall.Kind string`; `type sessionSniffer struct{...}` implementing `io.Writer`.

**Why:** `RecordSession` runs only *after* `Call` returns. A session killed mid-call therefore never persists its id, so continue would resume the previous step's session — or, on the first call, find none and refuse. The id is available live: `claude --output-format stream-json` emits `{"type":"system","subtype":"init","session_id":"..."}` first.

- [ ] **Step 1: Write the failing tests**

Append to `claude_test.go`:

```go
// midStreamRunner writes an init event, then calls check (which asserts the
// session file already exists), then writes the terminal result event. It is
// how we prove the sniffer persists the id BEFORE Call returns.
type midStreamRunner struct {
	initLine   string
	resultLine string
	check      func()
}

func (m midStreamRunner) Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (string, string, error) {
	return "", "", nil
}

func (m midStreamRunner) RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (string, error) {
	_, _ = io.WriteString(w, m.initLine+"\n")
	if m.check != nil {
		m.check()
	}
	_, _ = io.WriteString(w, m.resultLine+"\n")
	return "", nil
}

func TestCallWithKindRecordsSessionMidStream(t *testing.T) {
	dir := t.TempDir()
	var midID string
	r := midStreamRunner{
		initLine:   `{"type":"system","subtype":"init","session_id":"live-1"}`,
		resultLine: `{"type":"result","result":"done","session_id":"live-1","is_error":false}`,
		check: func() {
			if si, err := readSession(dir); err == nil {
				midID = si.SessionID
			}
		},
	}
	c := &Claude{runner: r, logDir: dir}
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "execute", Kind: "feature", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	if midID != "live-1" {
		t.Fatalf("session recorded mid-stream = %q, want live-1 (a stop mid-call must leave a resumable session)", midID)
	}
	si, err := readSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if si.Kind != "feature" {
		t.Fatalf("kind = %q, want feature", si.Kind)
	}
}

func TestCallWithoutKindNeverWritesSession(t *testing.T) {
	dir := t.TempDir()
	r := midStreamRunner{
		initLine:   `{"type":"system","subtype":"init","session_id":"eph-1"}`,
		resultLine: `{"type":"result","result":"answer","session_id":"eph-1","is_error":false}`,
	}
	c := &Claude{runner: r, logDir: dir}
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "answer-1", Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := readSession(dir); err == nil {
		t.Fatal("an ephemeral (Kind-less) call must never write the session file")
	}
}

func TestCallWithKindToleratesMalformedStreamLines(t *testing.T) {
	dir := t.TempDir()
	r := midStreamRunner{
		initLine:   `{not json at all`,
		resultLine: `{"type":"result","result":"done","session_id":"late-1","is_error":false}`,
	}
	c := &Claude{runner: r, logDir: dir}
	res, err := c.Call(context.Background(), ClaudeCall{Label: "execute", Kind: "bug", Prompt: "go"})
	if err != nil {
		t.Fatalf("a malformed stream line must not fail the call: %v", err)
	}
	if res.SessionID != "late-1" {
		t.Fatalf("session id = %q, want late-1", res.SessionID)
	}
}
```

Add `io` to `claude_test.go`'s imports if missing.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./... -run 'TestCallWith' -v`
Expected: FAIL — `unknown field Kind in struct literal`.

- [ ] **Step 3: Add Kind and the sniffer**

In `claude.go`, extend `ClaudeCall`:

```go
type ClaudeCall struct {
	Dir             string
	Label           string
	Prompt          string
	Model           ModelConfig
	Resume          string
	DisallowedTools []string
	SkipPermissions bool
	// Kind, when non-empty, marks this as a primary working session: its id is
	// persisted the moment claude announces it, not after the call returns, so a
	// session killed mid-call is still resumable. It is the pipeline kind
	// ("bug"/"feature") stored alongside the id. Ephemeral calls (triage,
	// answerer) leave it empty and never touch the session file.
	Kind string
}
```

Append after `RecordSession`:

```go
// sessionSniffer wraps the stream sink, watching arriving stream-json lines for
// the first session_id and persisting it immediately. Without this, a session
// killed mid-call (a stop, a crash) would leave `session` pointing at the
// previous step, and continue would resume the wrong session or none at all.
// It writes through unconditionally and never fails a call: sniffing is
// best-effort, exactly like the other log-writers.
type sessionSniffer struct {
	w    io.Writer
	buf  []byte // partial trailing line
	done bool
	on   func(id string)
}

func (s *sessionSniffer) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if s.done {
		return n, err
	}
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := s.buf[:i]
		s.buf = s.buf[i+1:]
		if id := sniffSessionID(line); id != "" {
			s.done = true
			s.buf = nil
			s.on(id)
			break
		}
	}
	return n, err
}

// sniffSessionID returns the session_id carried by one stream-json line, or ""
// when the line is malformed or carries none.
func sniffSessionID(line []byte) string {
	var ev struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &ev) != nil {
		return ""
	}
	return ev.SessionID
}
```

In `Call`, wrap the sink after the existing `streamFile` block (replace the two lines that follow it):

```go
	var buf bytes.Buffer
	sink := io.Writer(&buf)
	if f := c.streamFile(seq, call.Label); f != nil {
		defer f.Close()
		sink = io.MultiWriter(f, &buf)
	}
	// A primary working session persists its id the moment claude announces it,
	// so a stop or crash mid-call still leaves something continue can resume.
	if call.Kind != "" {
		sink = &sessionSniffer{w: sink, on: func(id string) { c.RecordSession(id, call.Kind) }}
	}
	stderr, err := c.runner.RunStream(ctx, call.Dir, env, call.Prompt, sink, "claude", args...)
```

`claude.go` already imports `bytes`, `encoding/json`, and `io`.

- [ ] **Step 4: Set Kind at the primary call sites**

`pipeline_bug.go` — add `Kind: "bug",` to the `ClaudeCall` literal (keep the existing post-return `RecordSession`; it is now a same-id overwrite that still carries the authoritative kind).

`pipeline_feature.go` — add `Kind: "feature",` to three literals: the `architect` closure's call (line ~27), the `plan` call in `runPlanThenExecute` (line ~119), and the `execute` call in `executePlan` (line ~142). Do **not** set it on the `done-confirm-*` or `answer-*` calls.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./...`
Expected: `ok  	loope` — the existing `claude_test.go` and pipeline tests must still pass unchanged.

- [ ] **Step 6: Commit**

```bash
git add claude.go claude_test.go pipeline_bug.go pipeline_feature.go
git commit -m "fix: persist the claude session id as soon as the stream announces it"
```

---

## Task 6: One shared resume body, with registry wrapping and the stop branch

**Files:**
- Modify: `rework.go:14-59`
- Modify: `loop.go:141-179` (`handleIssue`)
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `runRegistry` (Task 3), `stopRequested` (Task 2), `ClaudeCall.Kind` (Task 5).
- Produces:
  - `(*Orchestrator) resume(ctx context.Context, n int, fromLabel string) error` — resumes issue n's persisted session in its preserved worktree, then ships.
  - `(*Orchestrator) Rework(ctx context.Context, n int) error` — unchanged behaviour, now `return o.resume(ctx, n, o.cfg.StateLabels.Rework)`.
  - `(*Orchestrator) finishStopped(ctx context.Context, n int, fromLabel string) error` — **stubbed in this task, fully implemented in Task 7.**

- [ ] **Step 1: Write the failing tests**

Append to `loop_test.go`:

```go
func TestHandleIssuePipelineErrorWithStopMarkerFinishesStopped(t *testing.T) {
	env := newFakeEnv(t)
	env.failClaude = true
	o := env.orchestrator()
	recordStopRequest(o.issueLogDir(7))

	err := o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main")
	if err != nil {
		t.Fatalf("a stopped pipeline is a clean outcome, got %v", err)
	}
	if got := readStateFile(t, o.issueLogDir(7)); got != "ai-stopped" {
		t.Fatalf("state = %q, want ai-stopped", got)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) != 0 {
		t.Fatal("a stopped pipeline must not park as ai-rework")
	}
}

func TestHandleIssuePipelineErrorWithoutStopMarkerStillParks(t *testing.T) {
	env := newFakeEnv(t)
	env.failClaude = true
	o := env.orchestrator()

	if err := o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main"); err == nil {
		t.Fatal("a genuine pipeline failure must still return its error")
	}
	if got := readStateFile(t, o.issueLogDir(7)); got != "ai-rework" {
		t.Fatalf("state = %q, want ai-rework", got)
	}
}

func TestHandleIssueRegistersAndDeregisters(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	_ = o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main")
	if o.registry.running(7) {
		t.Fatal("handleIssue must deregister the issue when it returns")
	}
}

// readStateFile returns the recorded state label for a log dir.
func readStateFile(t *testing.T, logDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(logDir, stateFile))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return strings.TrimSpace(string(b))
}
```

If `loop_test.go` already defines a helper reading the state file, reuse it instead of adding `readStateFile`, and adjust these tests to call it.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./... -run 'TestHandleIssue' -v`
Expected: FAIL — `undefined: finishStopped` is not yet referenced, so the stop-marker test fails with `state = "ai-rework", want ai-stopped`.

- [ ] **Step 3: Add the temporary finishStopped stub**

Append to `control.go` (Task 7 replaces the body; the signature is final):

```go
// finishStopped parks issue n in the operator-held stopped state, preserving
// every artifact. fromLabel is the state label the issue currently carries, or
// "" when it carries none (a queued ticket).
func (o *Orchestrator) finishStopped(ctx context.Context, n int, fromLabel string) error {
	cctx := context.WithoutCancel(ctx)
	if fromLabel == "" {
		_ = o.gh.AddLabel(cctx, n, o.cfg.StateLabels.Stopped)
	} else {
		_ = o.gh.SwapLabels(cctx, n, fromLabel, o.cfg.StateLabels.Stopped)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Stopped)
	clearParkCause(o.issueLogDir(n))
	return nil
}
```

- [ ] **Step 4: Wrap handleIssue and add its stop branch**

In `loop.go`, at the top of `handleIssue` (right after `branch := branchName(n)`):

```go
	// Own this issue for the life of the pipeline: a stop cancels ictx, which
	// kills the in-flight claude process and unwinds us into the stop branch
	// below.
	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !o.registry.register(n, cancel) {
		return fmt.Errorf("issue #%d is already running", n)
	}
	defer o.registry.deregister(n)
```

Then replace every remaining use of `ctx` inside `handleIssue` with `ictx` — `AddLabel`, `Comment`, `wt.Create`, `FetchIssueContent`, `DownloadIssueImages`, the pipeline call, and the terminal `finishDone`/`finishNeedsInfo`/`park`/`ship` calls. (`finishDone`, `finishNeedsInfo`, and `park` already call `context.WithoutCancel`, so passing the cancelled `ictx` is safe.)

Add the stop branch immediately before the existing park:

```go
	if perr != nil {
		// A stop and a daemon shutdown both surface as a cancelled claude call.
		// The marker is what tells them apart: only a stop leaves one, so a
		// SIGTERM to the daemon still parks its in-flight issues as before.
		if stopRequested(o.issueLogDir(n)) {
			return o.finishStopped(ictx, n, o.cfg.StateLabels.WIP)
		}
		return o.park(ictx, n, o.cfg.StateLabels.WIP, perr)
	}
	return o.ship(ictx, issue, wtPath, branch, base, kind, o.cfg.StateLabels.WIP)
```

- [ ] **Step 5: Split Rework into the shared resume body**

Replace `rework.go`'s `Rework` with:

```go
// Rework resumes a parked (ai-rework) issue and ships it. It reads the preserved
// worktree and the saved Claude session, resumes that session headlessly with a
// "finish the job" prompt, then runs the shared ship step (swapping
// ai-rework->ai-done on success). Idempotent: a failure re-parks as ai-rework
// with the worktree intact, so it can be run again. It is the entry point for
// `-rework` and for ResumeParked's auto-resume; behaviour is unchanged.
func (o *Orchestrator) Rework(ctx context.Context, n int) error {
	return o.resume(ctx, n, o.cfg.StateLabels.Rework)
}

// resume resumes issue n's persisted Claude session in its preserved worktree,
// then ships. fromLabel is the state label the issue currently carries, which
// ship swaps to Done and park swaps to Rework — ai-rework for a rework, ai-wip
// for a continue. Like handleIssue it registers the run so a stop can cancel it,
// and finishes as stopped rather than parked when a stop marker is present.
func (o *Orchestrator) resume(ctx context.Context, n int, fromLabel string) error {
	wtPath := worktreePath(o.cfg.WorkDir, n)
	if _, err := os.Stat(wtPath); err != nil {
		return fmt.Errorf("issue #%d: no preserved worktree at %s to resume (remove the %s label to re-queue from scratch): %w",
			n, wtPath, o.cfg.StateLabels.Rework, err)
	}
	logDir := o.issueLogDir(n)
	si, err := readSession(logDir)
	if err != nil {
		return fmt.Errorf("issue #%d: no saved session to resume (remove the %s label to re-queue from scratch): %w",
			n, o.cfg.StateLabels.Rework, err)
	}
	if si.SessionID == "" {
		return fmt.Errorf("issue #%d: saved session file has no session id", n)
	}

	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !o.registry.register(n, cancel) {
		return fmt.Errorf("#%d is already running", n)
	}
	defer o.registry.deregister(n)

	base, err := o.wt.DefaultBranch(ictx)
	if err != nil {
		return err
	}
	title, err := o.gh.IssueTitle(ictx, n)
	if err != nil {
		return err
	}

	c := &Claude{runner: o.runner, logDir: logDir, configDir: o.cfg.ClaudeConfigDir}
	res, err := c.Call(ictx, ClaudeCall{
		Dir: wtPath, Label: "rework", Prompt: reworkPrompt(), Resume: si.SessionID,
		Model:           o.cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
		Kind:            si.Kind,
	})
	// Record before the error check so a rework that fails again (e.g. a fresh
	// 429) still advances the saved session to the latest one for the next run.
	if res != nil {
		c.RecordSession(res.SessionID, si.Kind)
	}
	if err != nil {
		if stopRequested(logDir) {
			return o.finishStopped(ictx, n, fromLabel)
		}
		return o.park(ictx, n, fromLabel, err)
	}

	branch := branchName(n)
	if reason, ok := parseAlreadyDone(res.Result); ok {
		return o.finishDone(ictx, n, wtPath, branch, fromLabel, reason)
	}
	return o.ship(ictx, Issue{Number: n, Title: title}, wtPath, branch, base, si.Kind, fromLabel)
}
```

Add `"context"` to `rework.go`'s imports (already present) — no other import changes.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./...`
Expected: `ok  	loope`. Existing rework tests must pass untouched — the error strings and label transitions are byte-identical for `fromLabel = Rework`.

- [ ] **Step 7: Commit**

```bash
git add rework.go loop.go control.go loop_test.go
git commit -m "refactor: share one resume body between rework and continue"
```

---

## Task 7: `Orchestrator.Stop` and the real `finishStopped`

**Files:**
- Modify: `control.go`
- Modify: `daemon.go` (add `lockOwnerAlive`)
- Test: `control_test.go`, `daemon_test.go`

**Interfaces:**
- Consumes: `runRegistry` (Task 3), stop-marker helpers (Task 2), `pidAlive`/`lockPath` (`daemon.go`).
- Produces:
  - `lockOwnerAlive(workDir string) bool`
  - `(*Orchestrator) Stop(ctx context.Context, n int) error`
  - `(*Orchestrator) finishStopped(ctx context.Context, n int, fromLabel string) error` (final body)
  - `(*Orchestrator) currentStateLabel(ctx context.Context, n int) (string, error)`

**Decision table Stop implements** (marker is always written first):

| Condition | Action | Printed |
|---|---|---|
| Registered in this process | `registry.cancel(n)`; the pipeline labels as it unwinds | `stopping #N (halting the running session)` |
| Not local, state is WIP, live daemon holds the lock | marker only | `stop requested for #N — the running daemon will halt it shortly` |
| Anything else | `finishStopped` directly | `stopped #N` |

- [ ] **Step 1: Write the failing tests**

Append to `control_test.go` (add imports `strings`, `testing`, plus whatever the helpers need):

```go
// stopEnv wires a fakeEnv orchestrator whose gh `issue view --json labels`
// returns the labels the test wants, so Stop can read the current state.
func stopEnv(t *testing.T, labels ...string) (*fakeEnv, *Orchestrator) {
	t.Helper()
	env := newFakeEnv(t)
	base := env.f.handler
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, `{"name":"`+l+`"}`)
	}
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.HasPrefix(joined, "issue view") && strings.Contains(joined, "labels") {
			return `{"labels":[` + strings.Join(quoted, ",") + `]}`, "", nil
		}
		return base(c)
	}
	return env, env.orchestrator()
}

func TestStopRegisteredRunCancelsAndLeavesLabelingToThePipeline(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	cancelled := make(chan struct{})
	o.registry.register(7, func() { close(cancelled) })

	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("Stop must cancel a locally registered run")
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("Stop must write the marker first")
	}
	if len(env.callsMatching("gh", "--add-label ai-stopped")) != 0 {
		t.Fatal("the pipeline labels as it unwinds; Stop must not label a registered run")
	}
}

func TestStopQueuedTicketAddsStoppedLabel(t *testing.T) {
	env, o := stopEnv(t, "ai-agent")
	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "--add-label ai-stopped")) == 0 {
		t.Fatal("a queued ticket with no state label should get ai-stopped added")
	}
	if len(env.callsMatching("git", "worktree")) != 0 {
		t.Fatal("stopping a queued ticket must not touch any worktree")
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("marker missing")
	}
}

func TestStopParkedTicketSwapsAndClearsParkCause(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-rework")
	recordParkCause(o.issueLogDir(7), "usage limit")

	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	swaps := env.callsMatching("gh", "--remove-label ai-rework")
	if len(swaps) == 0 || !strings.Contains(swaps[0], "--add-label ai-stopped") {
		t.Fatalf("want a rework->stopped swap, got %v", swaps)
	}
	if readParkCause(o.issueLogDir(7)) != "" {
		t.Fatal("a stopped ticket must carry no park cause")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-stopped")
	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatalf("stopping a stopped ticket must be a no-op success, got %v", err)
	}
}

func TestStopDoneTicketErrors(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-done")
	if err := o.Stop(context.Background(), 7); err == nil {
		t.Fatal("stopping a done ticket must error: there is nothing to stop")
	}
}

func TestFinishStoppedPreservesEverything(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	recordStopRequest(o.issueLogDir(7))

	if err := o.finishStopped(context.Background(), 7, "ai-wip"); err != nil {
		t.Fatal(err)
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("finishStopped must LEAVE the marker: it is the durable record of the hold")
	}
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Fatal("finishStopped must not remove the worktree")
	}
	if len(env.callsMatching("git", "branch -D")) != 0 {
		t.Fatal("finishStopped must not delete the branch")
	}
	comments := env.callsMatching("gh", "issue comment")
	if len(comments) == 0 || !strings.Contains(strings.Join(comments, "\n"), "Stopped by request") {
		t.Fatalf("want a stop comment, got %v", comments)
	}
}
```

Append to `daemon_test.go`:

```go
func TestLockOwnerAlive(t *testing.T) {
	work := t.TempDir()
	if lockOwnerAlive(work) {
		t.Fatal("no lock file: owner cannot be alive")
	}
	release, err := acquireLock(work)
	if err != nil {
		t.Fatal(err)
	}
	if !lockOwnerAlive(work) {
		t.Fatal("we hold the lock, so its owner (us) is alive")
	}
	release()
	if lockOwnerAlive(work) {
		t.Fatal("lock released: owner must not be reported alive")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./... -run 'TestStop|TestFinishStopped|TestLockOwnerAlive' -v`
Expected: FAIL — `undefined: lockOwnerAlive`, and `o.Stop undefined`.

- [ ] **Step 3: Extract lockOwnerAlive**

Append to `daemon.go`:

```go
// lockOwnerAlive reports whether a live process currently holds workDir's daemon
// lock. Stop uses it to tell "a daemon is running this issue and will react to
// the marker" from "nothing is running, so I must do the labeling myself".
func lockOwnerAlive(workDir string) bool {
	b, err := os.ReadFile(lockPath(workDir))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	return pidAlive(pid)
}
```

- [ ] **Step 4: Implement Stop, finishStopped, and currentStateLabel**

In `control.go`, replace the Task-6 `finishStopped` stub and add the rest:

```go
// currentStateLabel returns the state label issue n currently carries on
// GitHub, or "" when it carries none (a queued ticket).
func (o *Orchestrator) currentStateLabel(ctx context.Context, n int) (string, error) {
	labels, err := o.gh.IssueLabels(ctx, n)
	if err != nil {
		return "", err
	}
	sl := o.cfg.StateLabels
	for _, want := range []string{sl.WIP, sl.Stopped, sl.Rework, sl.Done, sl.NeedsInfo, sl.Failed} {
		if want != "" && hasLabel(labels, want) {
			return want, nil
		}
	}
	return "", nil
}

// Stop halts work on issue n and parks it in the operator-held stopped state,
// preserving every artifact. The stop marker is written FIRST, so the request is
// durable before anything else can fail — that is what lets `loope -stop <N>` in
// a second shell halt a run a daemon in another process owns, and what makes the
// stop survive a daemon restart.
//
// Then, by what is actually running: a pipeline live in THIS process is
// cancelled and does its own labeling as it unwinds; a WIP issue owned by a live
// daemon elsewhere is left to that daemon's watcher (~2s); anything else
// (queued, parked, or WIP with no daemon alive) is labeled here and now.
//
// Stopping a stopped issue is a no-op success. Stopping a done or needs-info
// issue is an error: there is nothing to stop.
func (o *Orchestrator) Stop(ctx context.Context, n int) error {
	state, err := o.currentStateLabel(ctx, n)
	if err != nil {
		return err
	}
	switch state {
	case o.cfg.StateLabels.Stopped:
		log.Printf("stopped #%d", n)
		return nil
	case o.cfg.StateLabels.Done, o.cfg.StateLabels.NeedsInfo:
		return fmt.Errorf("#%d is %s — there is nothing to stop", n, state)
	}

	recordStopRequest(o.issueLogDir(n))

	if o.registry.cancel(n) {
		log.Printf("stopping #%d (halting the running session)", n)
		return nil
	}
	if state == o.cfg.StateLabels.WIP && lockOwnerAlive(o.cfg.WorkDir) {
		log.Printf("stop requested for #%d — the running daemon will halt it shortly", n)
		return nil
	}
	if err := o.finishStopped(ctx, n, state); err != nil {
		return err
	}
	log.Printf("stopped #%d", n)
	return nil
}

// finishStopped moves issue n into the stopped state, preserving the worktree,
// branch, logs, and session file — continue builds on all of it. fromLabel is
// the state label the issue carries, or "" for a queued ticket that has none.
//
// It uses a cancellation-proof context because the pipeline path calls it with
// an already-cancelled one, clears the park cause so ResumeParked can never see
// the issue as resumable, and deliberately LEAVES the stop marker: the marker is
// cleared by continue, not by the stop completing.
func (o *Orchestrator) finishStopped(ctx context.Context, n int, fromLabel string) error {
	cctx := context.WithoutCancel(ctx)
	_ = o.gh.Comment(cctx, n, fmt.Sprintf(
		"🤖 Stopped by request. Progress is preserved — continue with `loope -continue %d` or the dashboard.", n))
	if fromLabel == "" {
		if err := o.gh.AddLabel(cctx, n, o.cfg.StateLabels.Stopped); err != nil {
			return fmt.Errorf("issue #%d: marking stopped failed: %w", n, err)
		}
	} else if err := o.gh.SwapLabels(cctx, n, fromLabel, o.cfg.StateLabels.Stopped); err != nil {
		return fmt.Errorf("issue #%d: marking stopped failed: %w", n, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Stopped)
	clearParkCause(o.issueLogDir(n))
	return nil
}
```

Add `"fmt"` and `"log"` to `control.go`'s imports.

- [ ] **Step 5: Add GitHub.IssueLabels**

Append to `github.go` next to `IssueTitle`:

```go
// IssueLabels returns the labels currently on an issue. Stop reads it to decide
// which state it is transitioning out of.
func (g *GitHub) IssueLabels(ctx context.Context, num int) ([]Label, error) {
	out, err := g.gh(ctx, "issue", "view", strconv.Itoa(num), "--repo", g.slug, "--json", "labels")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Labels []Label `json:"labels"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return nil, fmt.Errorf("parse issue labels: %w", err)
	}
	return payload.Labels, nil
}
```

Confirm the shape of `IssueTitle` (`github.go:168`) and mirror its retry/`g.gh` usage exactly.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./...`
Expected: `ok  	loope`

- [ ] **Step 7: Commit**

```bash
git add control.go daemon.go github.go control_test.go daemon_test.go
git commit -m "feat: add Orchestrator.Stop and the stopped finisher"
```

---

## Task 8: The stop watcher

**Files:**
- Modify: `control.go`
- Test: `control_test.go`

**Interfaces:**
- Consumes: `runRegistry.numbers`/`cancel`, `stopRequested`.
- Produces: `(*Orchestrator) watchStops(ctx context.Context, every time.Duration)`.

- [ ] **Step 1: Write the failing test**

Append to `control_test.go`:

```go
func TestWatchStopsCancelsWhenMarkerAppearsOutOfBand(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-wip")
	cancelled := make(chan struct{})
	o.registry.register(7, func() { close(cancelled) })

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go o.watchStops(ctx, time.Millisecond)

	// Simulate `loope -stop 7` in a second process: it can only write the file.
	recordStopRequest(o.issueLogDir(7))

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("watchStops should cancel a registered run once its marker appears")
	}
}

func TestWatchStopsIgnoresUnmarkedRuns(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-wip")
	cancelled := make(chan struct{})
	o.registry.register(7, func() { close(cancelled) })

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go o.watchStops(ctx, time.Millisecond)

	select {
	case <-cancelled:
		t.Fatal("watchStops must not cancel a run with no stop marker")
	case <-time.After(100 * time.Millisecond):
	}
}
```

Add `"time"` to `control_test.go`'s imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./... -run TestWatchStops -v`
Expected: FAIL — `o.watchStops undefined`.

- [ ] **Step 3: Implement the watcher**

Append to `control.go`:

```go
// watchStops cancels any locally running pipeline whose stop marker has
// appeared. It is what lets `loope -stop <N>` in another shell halt a run this
// daemon owns: that process can only write the marker file, not reach into this
// process's goroutines.
//
// It iterates only over registered issue numbers, so a quiet daemon does one
// os.Stat per live pipeline per tick and nothing else. Returns when ctx is done.
func (o *Orchestrator) watchStops(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, n := range o.registry.numbers() {
				if stopRequested(o.issueLogDir(n)) {
					if o.registry.cancel(n) {
						log.Printf("issue #%d: stop requested — halting the running session", n)
					}
				}
			}
		}
	}
}
```

Add `"time"` to `control.go`'s imports.

Note: `cancel` is idempotent for our purposes — a repeated tick before the pipeline deregisters calls an already-called `CancelFunc`, which `context` explicitly permits.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./... -run TestWatchStops -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add control.go control_test.go
git commit -m "feat: watch for out-of-band stop requests"
```

---

## Task 9: `Orchestrator.Continue`

**Files:**
- Modify: `control.go`
- Test: `control_test.go`

**Interfaces:**
- Consumes: `resume(ctx, n, fromLabel)` (Task 6), `registry.running`, stop-marker helpers, `currentStateLabel`.
- Produces:
  - `(*Orchestrator) prepareContinue(ctx context.Context, n int) (run func(context.Context) error, err error)`
  - `(*Orchestrator) Continue(ctx context.Context, n int) error`

**Two shapes:**
- **Case 1** — preserved worktree *and* a saved session id: clear the marker, swap Stopped → WIP, then `resume(ctx, n, WIP)`.
- **Case 2** — neither: nothing to resume, so re-queue: clear the marker, `RemoveLabel(Stopped)`, `clearState`. `run` is nil.

- [ ] **Step 1: Write the failing tests**

Append to `control_test.go`:

```go
// seedResumable puts a worktree dir and a session file on disk for issue n, so
// continue takes the real-resume path.
func seedResumable(t *testing.T, o *Orchestrator, n int, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(worktreePath(o.cfg.WorkDir, n), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Claude{logDir: o.issueLogDir(n)}
	c.RecordSession(sessionID, "bug")
}

func TestContinueResumesPersistedSessionAndShips(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-stopped")
	seedResumable(t, o, 7, "sess-42")
	recordStopRequest(o.issueLogDir(7))

	if err := o.Continue(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if stopRequested(o.issueLogDir(7)) {
		t.Fatal("continue must clear the stop marker")
	}
	swaps := env.callsMatching("gh", "--remove-label ai-stopped")
	if len(swaps) == 0 || !strings.Contains(swaps[0], "--add-label ai-wip") {
		t.Fatalf("want a stopped->wip swap, got %v", swaps)
	}
	resumed := env.callsMatching("claude", "--resume sess-42")
	if len(resumed) == 0 {
		t.Fatal("continue must resume the persisted session id")
	}
	if len(env.callsMatching("gh", "--add-label ai-done")) == 0 {
		t.Fatal("a successful continue ships: wip -> done")
	}
}

func TestContinueWithoutWorktreeRequeues(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-stopped")
	recordStopRequest(o.issueLogDir(7))
	recordState(o.issueLogDir(7), "ai-stopped")

	if err := o.Continue(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	removals := env.callsMatching("gh", "--remove-label ai-stopped")
	if len(removals) == 0 {
		t.Fatal("with nothing to resume, continue re-queues by removing ai-stopped")
	}
	if _, err := os.Stat(filepath.Join(o.issueLogDir(7), stateFile)); err == nil {
		t.Fatal("re-queueing must clear the local state marker")
	}
	if len(env.callsMatching("claude", "--resume")) != 0 {
		t.Fatal("there is nothing to resume, so no claude call may be made")
	}
	if stopRequested(o.issueLogDir(7)) {
		t.Fatal("continue must clear the stop marker")
	}
}

func TestContinueRefusesRunningIssue(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-stopped")
	seedResumable(t, o, 7, "sess-42")
	o.registry.register(7, func() {})

	err := o.Continue(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "#7 is already running") {
		t.Fatalf("want '#7 is already running', got %v", err)
	}
}

func TestContinueRefusesNonStoppedIssue(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-rework")
	err := o.Continue(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "#7 is not stopped") {
		t.Fatalf("want '#7 is not stopped', got %v", err)
	}
}
```

Add `"os"` and `"path/filepath"` to `control_test.go`'s imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./... -run TestContinue -v`
Expected: FAIL — `o.Continue undefined`.

- [ ] **Step 3: Implement prepareContinue and Continue**

Append to `control.go`:

```go
// prepareContinue validates a continue and performs everything that must happen
// synchronously — the caller can therefore report a real error — then returns
// the resume closure to run, or nil when there is nothing to resume.
//
// Case 1, a preserved worktree and a saved session: swap stopped -> WIP (the
// ticket is genuinely working again, so the dashboard shows it live and
// SweepOrphans can recover it if the daemon dies mid-continue) and return the
// resume. Case 2, neither survived — the ticket was stopped while queued — so
// continue means re-queue: drop the stopped label and the local state, and the
// next poll cycle picks the issue up from scratch through triage.
func (o *Orchestrator) prepareContinue(ctx context.Context, n int) (func(context.Context) error, error) {
	state, err := o.currentStateLabel(ctx, n)
	if err != nil {
		return nil, err
	}
	if state != o.cfg.StateLabels.Stopped {
		return nil, fmt.Errorf("#%d is not stopped", n)
	}
	if o.registry.running(n) {
		return nil, fmt.Errorf("#%d is already running", n)
	}
	logDir := o.issueLogDir(n)

	resumable := false
	if _, err := os.Stat(worktreePath(o.cfg.WorkDir, n)); err == nil {
		if si, serr := readSession(logDir); serr == nil && si.SessionID != "" {
			resumable = true
		}
	}

	clearStopRequest(logDir)
	if !resumable {
		if err := o.gh.RemoveLabel(ctx, n, o.cfg.StateLabels.Stopped); err != nil {
			return nil, fmt.Errorf("issue #%d: re-queueing failed: %w", n, err)
		}
		clearState(logDir)
		log.Printf("issue #%d: nothing to resume — re-queued for a fresh run", n)
		return nil, nil
	}
	if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.Stopped, o.cfg.StateLabels.WIP); err != nil {
		return nil, fmt.Errorf("issue #%d: marking wip failed: %w", n, err)
	}
	recordState(logDir, o.cfg.StateLabels.WIP)
	return func(rctx context.Context) error {
		return o.resume(rctx, n, o.cfg.StateLabels.WIP)
	}, nil
}

// Continue takes stopped issue n out of the operator hold and drives it to a PR,
// synchronously: it resumes the persisted Claude session in the preserved
// worktree and then ships (WIP -> Done) or parks (WIP -> Rework) exactly as a
// rework does. A ticket stopped before any work started is simply re-queued.
func (o *Orchestrator) Continue(ctx context.Context, n int) error {
	run, err := o.prepareContinue(ctx, n)
	if err != nil || run == nil {
		return err
	}
	return run(ctx)
}
```

Add `"os"` to `control.go`'s imports.

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./...`
Expected: `ok  	loope`

- [ ] **Step 5: Commit**

```bash
git add control.go control_test.go
git commit -m "feat: add Orchestrator.Continue"
```

---

## Task 10: Keep the daemon's hands off stopped tickets

**Files:**
- Modify: `loop.go:339-367` (`shouldResume`), `loop.go:410-438` (`SweepOrphans`)
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `stopRequested`, `finishStopped`.
- Produces: no new symbols.

Eligibility is already handled (Task 1's `hasStateLabel`), and `ResumeParked` only scans `ai-rework`, so a stopped ticket is invisible to it — `shouldResume` gets the marker check as belt-and-braces for a ticket whose labels are inconsistent.

- [ ] **Step 1: Write the failing tests**

Append to `loop_test.go`:

```go
func TestShouldResumeFalseWhenStopMarkerPresent(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	logDir := o.issueLogDir(7)
	// Everything else says "resumable": cause, worktree, session.
	recordParkCause(logDir, "session limit reached")
	if err := os.MkdirAll(worktreePath(o.cfg.WorkDir, 7), 0o755); err != nil {
		t.Fatal(err)
	}
	(&Claude{logDir: logDir}).RecordSession("s1", "bug")
	if !o.shouldResume(7) {
		t.Fatal("precondition: this issue should be auto-resumable without a stop marker")
	}

	recordStopRequest(logDir)
	if o.shouldResume(7) {
		t.Fatal("a stopped issue must never be auto-resumed")
	}
}

func TestSweepOrphansStoppedWIPFinishesStopped(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.HasPrefix(joined, "issue list") {
			return `[{"number":7,"title":"Fix crash","labels":[{"name":"ai-wip"}]}]`, "", nil
		}
		return base(c)
	}
	o := env.orchestrator()
	logDir := o.issueLogDir(7)
	if err := os.MkdirAll(worktreePath(o.cfg.WorkDir, 7), 0o755); err != nil {
		t.Fatal(err)
	}
	(&Claude{logDir: logDir}).RecordSession("s1", "bug")
	recordStopRequest(logDir)

	if err := o.SweepOrphans(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readStateFile(t, logDir); got != "ai-stopped" {
		t.Fatalf("state = %q, want ai-stopped — a stopped-then-crashed run must not be parked for auto-resume", got)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) != 0 {
		t.Fatal("SweepOrphans must not park a stopped issue for resume")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./... -run 'TestShouldResumeFalseWhenStop|TestSweepOrphansStopped' -v`
Expected: FAIL — `shouldResume` returns true; the sweep records `ai-rework`.

- [ ] **Step 3: Add the stop check to shouldResume**

In `loop.go`, extend the reason chain (the marker check goes first: an operator hold outranks every other reason):

```go
	logDir := o.issueLogDir(n)
	reason := ""
	if stopRequested(logDir) {
		reason = "stopped by request; resume it with `loope -continue`"
	} else if cause := readParkCause(logDir); cause == "" {
		reason = "no recorded park cause; waiting for a human (`loop -rework`)"
	} else if _, resumable := classifyCause(cause); !resumable {
```

(keep the remaining `else if` branches exactly as they are).

- [ ] **Step 4: Add the stop check to SweepOrphans**

In `loop.go`, inside the `for _, is := range issues` loop, immediately after `logDir := o.issueLogDir(n)`:

```go
		// A stop marker means the run was stopped and the daemon then died. The
		// operator's hold outlives the crash, so recover it into stopped rather
		// than parking it for auto-resume.
		if stopRequested(logDir) {
			log.Printf("issue #%d: stale %s from a crashed run that was stopped — recovering as %s", n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Stopped)
			if err := o.finishStopped(ctx, n, o.cfg.StateLabels.WIP); err != nil {
				return err
			}
			continue
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./...`
Expected: `ok  	loope`

- [ ] **Step 6: Commit**

```bash
git add loop.go loop_test.go
git commit -m "feat: keep auto-resume and the orphan sweep off stopped tickets"
```

---

## Task 11: CLI flags `-stop` and `-continue`

**Files:**
- Modify: `main.go:20-87`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `Orchestrator.Stop/Continue`, `lockOwnerAlive`, `currentStateLabel`, `watchStops`.
- Produces:
  - `type cliFlags struct { configPath, addr *string; once, serve, showVersion *bool; rework, stopIssue, continueIssue *int }`
  - `registerFlags(fs *flag.FlagSet) cliFlags` — the single place flags are declared, so it is testable without running `main`.
  - flags `-stop <N>` and `-continue <N>`; `Orchestrator.baseCtx` set from the signal context; a `watchStops` goroutine in long-running modes.

Both flags are one-shot commands handled next to `-rework`, **before** the workDir lock is taken.

- [ ] **Step 1: Write the failing test**

Append to `main_test.go`:

```go
func TestRegisterFlagsParsesStopAndContinue(t *testing.T) {
	fs := flag.NewFlagSet("loope", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := registerFlags(fs)
	if err := fs.Parse([]string{"-stop", "42"}); err != nil {
		t.Fatal(err)
	}
	if *f.stopIssue != 42 {
		t.Fatalf("-stop = %d, want 42", *f.stopIssue)
	}

	fs2 := flag.NewFlagSet("loope", flag.ContinueOnError)
	fs2.SetOutput(io.Discard)
	f2 := registerFlags(fs2)
	if err := fs2.Parse([]string{"-continue", "7"}); err != nil {
		t.Fatal(err)
	}
	if *f2.continueIssue != 7 {
		t.Fatalf("-continue = %d, want 7", *f2.continueIssue)
	}
	if *f2.stopIssue != 0 {
		t.Fatalf("-stop default = %d, want 0", *f2.stopIssue)
	}
}

func TestRegisterFlagsKeepsExistingFlags(t *testing.T) {
	fs := flag.NewFlagSet("loope", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerFlags(fs)
	for _, name := range []string{"config", "once", "rework", "serve", "addr", "version", "stop", "continue"} {
		if fs.Lookup(name) == nil {
			t.Fatalf("flag -%s must be registered", name)
		}
	}
}
```

Add `"flag"`, `"io"`, and `"testing"` to `main_test.go`'s imports as needed.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./... -run TestRegisterFlags -v`
Expected: FAIL — `undefined: registerFlags`.

- [ ] **Step 3: Extract flag registration, add the two flags and their handlers**

In `main.go`, replace the inline flag declarations at the top of `main` with a testable registrar:

```go
// cliFlags is loope's whole command-line surface. It is declared in one place
// so the flag set can be built and asserted on without running main.
type cliFlags struct {
	configPath    *string
	addr          *string
	once          *bool
	serve         *bool
	showVersion   *bool
	rework        *int
	stopIssue     *int
	continueIssue *int
}

func registerFlags(fs *flag.FlagSet) cliFlags {
	return cliFlags{
		configPath:    fs.String("config", "loope.json", "path to config file"),
		once:          fs.Bool("once", false, "run a single poll cycle and exit"),
		rework:        fs.Int("rework", 0, "resume a parked (ai-rework) issue by number, ship it, then exit"),
		stopIssue:     fs.Int("stop", 0, "stop work on issue N, preserving all progress, then exit"),
		continueIssue: fs.Int("continue", 0, "continue a stopped issue N from its persisted Claude session, then exit"),
		serve:         fs.Bool("serve", false, "run the read-only progress dashboard and exit on signal"),
		addr:          fs.String("addr", "localhost:8080", "address for -serve to listen on"),
		showVersion:   fs.Bool("version", false, "print the loope version and exit"),
	}
}
```

and start `main` with:

```go
func main() {
	f := registerFlags(flag.CommandLine)
	flag.Parse()

	if *f.showVersion {
		fmt.Println("loope", version)
		return
	}

	cfg, err := LoadConfig(*f.configPath)
```

Rename the remaining uses in `main` accordingly (`*f.once`, `*f.rework`, `*f.serve`, `*f.addr`).

Set `baseCtx` when the orchestrator is built:

```go
	o := &Orchestrator{cfg: cfg, runner: r, gh: NewGitHub(r, cfg), baseCtx: ctx,
		wt: &Worktree{runner: r, repoPath: cfg.RepoPath, retry: cfg.GitHubRetry.policy()}}
```

Add the handlers immediately after the `-rework` block:

```go
	// -stop is safe against a live daemon: it writes the durable marker and
	// returns promptly, printing which path it took. It does not wait for the
	// running session to die.
	if *f.stopIssue > 0 {
		if err := o.Stop(ctx, *f.stopIssue); err != nil {
			log.Fatalf("stop #%d: %v", *f.stopIssue, err)
		}
		return
	}

	// -continue runs the resume synchronously and exits when the ticket ships or
	// parks, matching -rework. It refuses when a live daemon holds the lock and
	// the issue is WIP, since that would put two claude sessions in one worktree.
	if *f.continueIssue > 0 {
		n := *f.continueIssue
		if lockOwnerAlive(cfg.WorkDir) {
			state, err := o.currentStateLabel(ctx, n)
			if err != nil {
				log.Fatalf("continue #%d: %v", n, err)
			}
			if state == cfg.StateLabels.WIP {
				log.Fatalf("continue #%d: a daemon owns this workDir and #%d is %s — stop the daemon or use the dashboard", n, n, state)
			}
		}
		if err := o.Continue(ctx, n); err != nil {
			log.Fatalf("continue #%d: %v", n, err)
		}
		return
	}
```

Start the watcher in long-running modes, inside the existing `if !*once { ... }` block right after `sweep = true`:

```go
		// Only a lock-holding daemon owns live pipelines, so the watcher that
		// halts them on an out-of-band stop belongs here.
		go o.watchStops(ctx, 2*time.Second)
```

`main.go` already imports `time` and `log`.

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./... && go vet ./...`
Expected: `ok  	loope` and no vet output.

- [ ] **Step 5: Sanity-check the CLI surface**

Run: `go build -o /tmp/loope . && /tmp/loope -h 2>&1 | grep -E '^\s+-(stop|continue)'`
Expected: both flags listed with their descriptions.

- [ ] **Step 6: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add -stop and -continue CLI flags"
```

---

## Task 12: Dashboard stop / continue

**Files:**
- Modify: `serve.go:34-100` (`Server`, `NewServer`, `Handler`), `serve.go:602-616` (detail header)
- Modify: `serve.go:542-572` (page script — add the action handler)
- Modify: `serve.go:591` (rail badge) and `serve.go:609` (detail badge) — stopped case
- Modify: `control.go` (the `orchestratorController` adapter)
- Modify: `main.go` (pass the controller to `NewServer`)
- Test: `serve_test.go`

**Interfaces:**
- Consumes: `Orchestrator.Stop(ctx, n)`, `Orchestrator.prepareContinue(ctx, n)`, `Orchestrator.base()`.
- Produces:
  - `type Controller interface { Stop(n int) error; Continue(n int) error }`
  - `NewServer(r Runner, cfg *Config, ctl Controller) (*Server, error)`
  - `Server.ctl Controller`; routes `POST /stop`, `POST /continue`; template func `canAct` (bool: is a controller wired?)
  - `(*Orchestrator) controller() Controller` returning the adapter

- [ ] **Step 1: Write the failing tests**

Append to `serve_test.go`:

```go
// fakeController records calls and can fail on demand.
type fakeController struct {
	stopped   []int
	continued []int
	err       error
}

func (f *fakeController) Stop(n int) error {
	f.stopped = append(f.stopped, n)
	return f.err
}

func (f *fakeController) Continue(n int) error {
	f.continued = append(f.continued, n)
	return f.err
}

func postTo(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, nil).WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

Refactor the existing `newTestServer` (`serve_test.go:15`) so the fixture is shared and the controller is a parameter — `canAct` is baked into the template funcs at `NewServer` time, so assigning `s.ctl` afterwards would not enable the buttons:

```go
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newTestServerWithController(t, nil)
}

func newTestServerWithController(t *testing.T, ctl Controller) *Server {
	t.Helper()
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "architect", "design the thing", "the design output",
		`{"result":"the design output","session_id":"a3f9","is_error":false,"total_cost_usd":0.51}`)
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: `[{"number":142,"title":"Add OAuth login","labels":[{"name":"ai-wip"}]}]`}}}
	s, err := NewServer(r, cfg, ctl)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
```

func TestPostStopCallsController(t *testing.T) {
	ctl := &fakeController{}
	s := newTestServerWithController(t, ctl)
	code, body := postTo(t, s.Handler(), "/stop?issue=142")
	if code != http.StatusNoContent {
		t.Fatalf("code = %d body = %q, want 204", code, body)
	}
	if len(ctl.stopped) != 1 || ctl.stopped[0] != 142 {
		t.Fatalf("controller stops = %v, want [142]", ctl.stopped)
	}
}

func TestPostContinueCallsController(t *testing.T) {
	ctl := &fakeController{}
	s := newTestServerWithController(t, ctl)
	code, _ := postTo(t, s.Handler(), "/continue?issue=142")
	if code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", code)
	}
	if len(ctl.continued) != 1 || ctl.continued[0] != 142 {
		t.Fatalf("controller continues = %v, want [142]", ctl.continued)
	}
}

func TestPostStopControllerErrorIsA4xxWithReason(t *testing.T) {
	ctl := &fakeController{err: errors.New("#142 is already running")}
	s := newTestServerWithController(t, ctl)
	code, body := postTo(t, s.Handler(), "/stop?issue=142")
	if code != http.StatusConflict {
		t.Fatalf("code = %d, want 409", code)
	}
	if !strings.Contains(body, "#142 is already running") {
		t.Fatalf("body = %q, want the controller's reason", body)
	}
}

func TestPostStopBadIssueIs400(t *testing.T) {
	s := newTestServerWithController(t, &fakeController{})
	if code, _ := postTo(t, s.Handler(), "/stop?issue=abc"); code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", code)
	}
}

func TestGetStopIsNotRegistered(t *testing.T) {
	s := newTestServerWithController(t, &fakeController{})
	if code, _ := get(t, s.Handler(), "/stop?issue=142"); code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405 — a link or crawler must not be able to stop a ticket", code)
	}
}

func TestNilControllerRoutesAre503AndButtonsAbsent(t *testing.T) {
	s := newTestServer(t) // ctl is nil
	if code, _ := postTo(t, s.Handler(), "/stop?issue=142"); code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", code)
	}
	_, body := get(t, s.Handler(), "/detail?issue=142")
	if strings.Contains(body, `data-act="stop"`) {
		t.Fatal("a dashboard with no daemon behind it must not render action buttons")
	}
}

func TestDetailRendersStopButtonForWIP(t *testing.T) {
	s := newTestServerWithController(t, &fakeController{})
	_, body := get(t, s.Handler(), "/detail?issue=142") // the fixture issue is ai-wip
	if !strings.Contains(body, `data-act="stop"`) {
		t.Fatal("a wip ticket should offer stop")
	}
	if strings.Contains(body, `data-act="continue"`) {
		t.Fatal("a wip ticket must not offer continue")
	}
}
```

Add a stopped-state render test using the existing fixture pattern:

```go
func TestDetailRendersContinueButtonForStopped(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-9")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordState(dir, "ai-stopped")
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: `[{"number":9,"title":"Held","labels":[{"name":"ai-stopped"}]}]`}}}
	s, err := NewServer(r, cfg, &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	_, body := get(t, s.Handler(), "/detail?issue=9")
	if !strings.Contains(body, `data-act="continue"`) {
		t.Fatal("a stopped ticket should offer continue")
	}
	if strings.Contains(body, `data-act="stop"`) {
		t.Fatal("a stopped ticket must not offer stop")
	}
}

func TestDetailRendersNoActionsForDone(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-9")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordState(dir, "ai-done")
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: `[{"number":9,"title":"Shipped","labels":[{"name":"ai-done"}]}]`}}}
	s, err := NewServer(r, cfg, &fakeController{})
	if err != nil {
		t.Fatal(err)
	}
	_, body := get(t, s.Handler(), "/detail?issue=9")
	if strings.Contains(body, `data-act=`) {
		t.Fatal("a done ticket offers neither stop nor continue")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./... -run 'TestPost|TestGetStop|TestNilController|TestDetailRenders' -v`
Expected: FAIL — `not enough arguments in call to NewServer`, `s.ctl undefined`.

- [ ] **Step 3: Add the Controller interface, the field, and the routes**

In `serve.go`, above `Server`:

```go
// Controller is the mutating surface the dashboard exposes. Orchestrator
// implements it (via the adapter in control.go). A nil Controller — a dashboard
// with no daemon behind it — hides the buttons and makes the routes return 503.
type Controller interface {
	Stop(n int) error
	Continue(n int) error
}
```

Add the field to `Server`:

```go
	ctl Controller
```

Change the constructor signature and the returned struct:

```go
// NewServer parses the dashboard templates once and returns a Server that
// renders from the given Runner and Config. ctl, when non-nil, enables the
// mutating stop/continue routes and their buttons; pass nil for a strictly
// read-only dashboard. It errors if a template fails to parse.
func NewServer(r Runner, cfg *Config, ctl Controller) (*Server, error) {
```

Add to the `funcs` map (so templates can ask whether actions are available):

```go
		"canAct": func() bool { return ctl != nil },
```

and in the return:

```go
	return &Server{runner: r, cfg: cfg, ctl: ctl, gh: NewGitHub(r, cfg), page: page, rail: rail, detail: detail, ttl: defaultGHTTL, now: time.Now, prTried: map[int]bool{}}, nil
```

Extend `Handler` and add the handlers:

```go
// Handler returns the dashboard's HTTP routes: GET / (full page), GET /rail and
// GET /detail (the poll fragments), and the mutating POST /stop and
// POST /continue. The mutating routes are registered for POST only, so a link
// or a crawler cannot trigger either.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /rail", s.handleRail)
	mux.HandleFunc("GET /detail", s.handleDetail)
	mux.HandleFunc("POST /stop", s.handleStop)
	mux.HandleFunc("POST /continue", s.handleContinue)
	return mux
}

// handleStop halts work on ?issue=<N>. Stop is fast, so it runs inline.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func(n int) error { return s.ctl.Stop(n) })
}

// handleContinue resumes a stopped ?issue=<N>. The controller validates
// synchronously and runs the multi-minute resume in the background, so this
// returns as soon as the transition is real.
func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func(n int) error { return s.ctl.Continue(n) })
}

// act is the shared shape of the mutating routes: 503 with no controller, 400
// on a bad issue number, 409 with the controller's plain-text reason on a
// refusal, 204 on success.
func (s *Server) act(w http.ResponseWriter, r *http.Request, fn func(int) error) {
	if s.ctl == nil {
		http.Error(w, "no daemon behind this dashboard", http.StatusServiceUnavailable)
		return
	}
	n, err := strconv.Atoi(r.URL.Query().Get("issue"))
	if err != nil || n <= 0 {
		http.Error(w, "issue must be a positive number", http.StatusBadRequest)
		return
	}
	if err := fn(n); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

`serve.go` already imports `net/http` and `strconv`.

- [ ] **Step 4: Render the buttons and the stopped badge**

In `detailTmpl`, extend the chip row (`serve.go:613-616`) to:

```html
   <div class="mt-3 flex flex-wrap items-center gap-2 font-mono text-[11px]">
    <a href="{{issueURL .Number}}" target="_blank" rel="noopener" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/40">issue ↗</a>
    {{if .PRURL}}<a href="{{.PRURL}}" target="_blank" rel="noopener" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/40">pull request ↗</a>{{end}}
    {{if canAct}}
     {{if or (eq $k "wip") (eq $k "rework") (eq $k "queued")}}<button type="button" data-act="stop" data-issue="{{.Number}}" onclick="act(this)" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-warn/50">stop</button>{{end}}
     {{if eq $k "stopped"}}<button type="button" data-act="continue" data-issue="{{.Number}}" onclick="act(this)" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/50">continue</button>{{end}}
    {{end}}
   </div>
   <div id="acterr" class="mt-2 hidden font-mono text-[11px] text-err"></div>
```

Add the `stopped` case to both badge chains. In `railTmpl` (`serve.go:591`), insert before the final `{{else}}`:

```
{{else if eq $k "stopped"}}border-line2 bg-panel2 text-muted
```

In `detailTmpl` (`serve.go:609`), do the same in the class chain, and add `{{else if eq $k "stopped"}}stopped` to the label-text chain so the badge reads `stopped`.

Add the `act` handler to the page script, next to `copySid` (`serve.go:544`):

```js
 function act(btn){var a=btn.getAttribute('data-act'),n=btn.getAttribute('data-issue');btn.disabled=true;
  fetch('/'+a+'?issue='+n,{method:'POST'}).then(function(r){
   if(r.status===204){lastDetail=null;lastRail=null;poll();return;}
   return r.text().then(function(t){var e=document.getElementById('acterr');if(e){e.textContent=t.trim()||('could not '+a);e.classList.remove('hidden');}btn.disabled=false;});
  }).catch(function(){btn.disabled=false;});}
```

`act` references `lastDetail`/`lastRail`/`poll`, which are declared later in the same `<script>` block — `var`/`function` declarations hoist, and `act` only runs on click, so this is fine.

- [ ] **Step 5: Add the controller adapter and wire main**

Append to `control.go`:

```go
// orchestratorController adapts Orchestrator to the dashboard's Controller.
// The dashboard's requests are short-lived, so the adapter runs work on the
// daemon's context instead: a continue must survive its HTTP response and die
// with the daemon, not with the request.
type orchestratorController struct{ o *Orchestrator }

// controller returns the dashboard-facing mutating surface for this daemon.
func (o *Orchestrator) controller() Controller { return orchestratorController{o: o} }

// Stop is fast (it writes a marker and cancels or labels), so it runs inline and
// the UI gets a real error.
func (c orchestratorController) Stop(n int) error {
	return c.o.Stop(c.o.base(), n)
}

// Continue validates and performs the label transition synchronously — so the
// UI can report "#N is not stopped" or "#N is already running" — then runs the
// multi-minute resume in the background on the daemon's context.
func (c orchestratorController) Continue(n int) error {
	run, err := c.o.prepareContinue(c.o.base(), n)
	if err != nil || run == nil {
		return err
	}
	go func() {
		if err := guard("continue", func() error { return run(c.o.base()) }); err != nil {
			log.Printf("continue #%d: %v", n, err)
		}
	}()
	return nil
}
```

In `main.go`, pass the controller:

```go
		srv, err := NewServer(r, cfg, o.controller())
```

- [ ] **Step 6: Update the existing NewServer call sites in tests**

Every existing `NewServer(r, cfg)` in `serve_test.go` becomes `NewServer(r, cfg, nil)`.

Run: `grep -n 'NewServer(' serve_test.go` and fix each.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./... && go vet ./...`
Expected: `ok  	loope`, no vet output.

- [ ] **Step 8: Commit**

```bash
git add serve.go serve_test.go control.go main.go
git commit -m "feat: stop and continue from the dashboard"
```

---

## Task 13: Documentation

**Files:**
- Modify: `README.md` (label table ~line 62-69, recovery prose ~line 72-89, `gh label create` block ~line 107-111, dashboard flag table ~line 141-144, `stateLabels` example ~line 186)
- Modify: `loope.json.example` if it spells out `stateLabels` (check first)

**Interfaces:**
- Consumes: everything above.
- Produces: no code.

- [ ] **Step 1: Add the label row**

In the label lifecycle table, after the `ai-rework` row:

```markdown
| `ai-stopped` | You stopped it: work is halted and all progress preserved, awaiting `-continue` |
```

- [ ] **Step 2: Document the two verbs**

After the `-rework` recovery section (just before the `ai-failed` deprecation note), add:

````markdown
### Stop and continue a ticket

You can take a ticket out of the loop's hands at any stage and put it back
later. Nothing is deleted: the worktree, branch, logs, and Claude session id are
all preserved.

```bash
./loope -stop <N> -config loope.json      # halt work on #N, park it as ai-stopped
./loope -continue <N> -config loope.json  # resume #N from its saved session and ship it
```

`-stop` works on a running (`ai-wip`), queued (`ai-agent`), or parked
(`ai-rework`) ticket, and it is safe to run in a second shell while the daemon
is running: it writes a durable marker under `logs/issue-<N>/stop`, returns
immediately, and the daemon halts the live session within a couple of seconds
(the `claude` process gets a `SIGTERM` so it can flush its transcript). A
stopped ticket is **never** auto-resumed — that is the whole point of it being
its own state rather than `ai-rework`.

`-continue` resumes the persisted Claude session in the preserved worktree and
ships the PR, exactly as `-rework` does, swapping `ai-stopped` → `ai-wip` →
`ai-done`. It runs synchronously and exits when the ticket ships or parks. If
the ticket was stopped before any work started (no worktree or no saved
session), continue simply re-queues it and the next poll cycle picks it up from
scratch.

Both verbs are also available as buttons in the dashboard's detail pane.
````

- [ ] **Step 3: Add the label to the setup block**

```bash
gh label create ai-stopped --repo your-org/your-repo
```

- [ ] **Step 4: Update the stateLabels example**

```json
"stateLabels": {"wip": "ai-wip", "failed": "ai-failed", "done": "ai-done", "rework": "ai-rework", "needsInfo": "ai-needs-info", "stopped": "ai-stopped"}
```

Check `loope.json.example` for a `stateLabels` block and update it the same way if one exists.

- [ ] **Step 5: Add the localhost warning to the dashboard section**

Directly under the `-serve` flag table:

```markdown
> **Keep `-addr` bound to `localhost`** (as it defaults to). The dashboard's
> stop and continue buttons POST to `/stop` and `/continue`, which mutate ticket
> state, and — like the rest of the dashboard — they are unauthenticated. Binding
> to a public interface hands anyone who can reach the port control over your
> tickets.
```

- [ ] **Step 6: Verify the docs match reality**

Run: `go build -o /tmp/loope . && /tmp/loope -h 2>&1 | grep -E 'stop|continue'`
Expected: the flag help text matches what the README documents.

- [ ] **Step 7: Commit**

```bash
git add README.md loope.json.example
git commit -m "docs: document stop and continue"
```

---

## Task 14: Full-suite verification

**Files:** none modified.

- [ ] **Step 1: Run the whole suite with the race detector**

Run: `go test -race ./...`
Expected: `ok  	loope` — the registry, the watcher goroutine, and the continue goroutine are all concurrent, so this must be clean.

- [ ] **Step 2: Vet and build**

Run: `go vet ./... && go build ./...`
Expected: no output from either.

- [ ] **Step 3: Confirm nothing is left unformatted**

Run: `gofmt -l .`
Expected: no output.

- [ ] **Step 4: Commit any formatting fixes**

```bash
gofmt -w .
git add -A
git commit -m "chore: gofmt" || echo "nothing to format"
```

---

## Spec coverage

| Spec section | Task |
|---|---|
| New state `ai-stopped` (all six enumerators + badges) | 1, 12 |
| Stop marker file | 2 |
| Live-run registry | 3 |
| Graceful termination (SIGTERM + WaitDelay) | 4 |
| Live session capture (`Kind` + `sessionSniffer`) | 5 |
| Pipeline unwinding / stop branch in `handleIssue` and `resume` | 6 |
| Refactor: one resume body | 6 |
| `Stop`, `finishStopped`, decision table, `lockOwnerAlive` | 7 |
| Stop watcher | 8 |
| `Continue` cases 1 and 2 | 9 |
| Daemon paths ignoring stopped tickets (`shouldResume`, `SweepOrphans`) | 10 |
| CLI `-stop` / `-continue`, `-continue` daemon guard | 11 |
| Dashboard routes, `Controller`, non-blocking continue, buttons | 12 |
| README label row, `gh label create`, localhost note | 13 |

Out of scope, per the spec: a continue button on parked `ai-rework` tickets; stopping the daemon itself; dashboard authentication; any change to `ResumeParked`'s backoff or to `-rework`.
