# Stop / Continue a ticket from the dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a dashboard user Stop a currently-running (`ai-wip`) ticket and later Continue it, where Stop cancels the pipeline and parks the ticket as `ai-stopped` without deleting anything, and Continue defers a resume that the next free slot picks up.

**Architecture:** A new `ai-stopped` state label parks a stopped ticket out of every auto-resume path. Per-ticket `context.CancelFunc`s registered by the `ProcessOnce` pipeline goroutines give the read-only HTTP `Server` (now holding an `*Orchestrator` reference) a way to cancel one ticket's `claude` subprocess. Stop is asynchronous: it cancels and flags the run, and the goroutine transitions the label to `ai-stopped` (`pause`) as it unwinds. Continue is label-driven and deferred: it rewrites labels/state on disk so the next `runLoop` cycle resumes (session present → `ai-rework` → `Rework`) or restarts (no session → eligible → `ProcessOnce`) within the normal concurrency budget.

**Tech Stack:** Go 1.22 (`net/http` method-prefixed patterns), `html/template`, htmx (`hx-post` / `hx-confirm`), `gh` CLI, standard-library `context`.

## Global Constraints

- Go module (`go.mod`); build/test with `go build ./...` and `go test ./...` from the repo root.
- Preserve state on failure — never delete worktree/branch/logs/session on a stop or a continue (`CLAUDE.md`: "never remove and start from zero"). `pause` and Continue only rewrite labels/state files.
- State is label-driven: a ticket carrying any state label is dropped from the eligible queue by `hasStateLabel` (`github.go`). Concurrency is governed by the in-process slot ledger (`active` map, `slots.go`), never by counting GitHub labels.
- All `Orchestrator` shared-map access is guarded by the existing `o.mu` (`sync.Mutex`, not reentrant — never call one `mu`-locking helper from inside another).
- Tests follow the existing table / fake-runner (`fakeRunner`, `fakeEnv`, `newFakeEnv`) + fake-`gh` handler patterns; no real processes, no network.
- Commit messages: `feat:` for behavior, `docs:` for docs; end nothing with co-author trailers unless the user asks.

### Assumptions (headless calls, noted per the approved spec)

1. **Cancel registration is `ProcessOnce`-only, not `ResumeParked`.** Spec §2 mentions wiring both loops, but decision 1 scopes Stop to *currently-running `ai-wip`* tickets, and a resumed ticket is labeled `ai-rework` while it runs. Registering per-ticket cancels only on the `ProcessOnce` path keeps `pause`'s `ai-wip → ai-stopped` swap always correct and avoids a lingering `stopping` flag. A Stop targeting a resuming rework ticket is not offered by the UI and, if somehow issued, returns `errNotRunning` (harmless inline message). This is the only material deviation from the spec text and it strictly honors decision 1.
2. **`Server` gets its `*Orchestrator` via a field set in `main.go`, not a new `NewServer` parameter.** Same end state (the `Server` holds the reference) with a far smaller blast radius (13 existing `NewServer(r, cfg)` test call sites stay untouched). Handler tests set `s.orch` directly.
3. **The stopped-comment text is an inline literal** (`stoppedComment()`), not a prompt template, to avoid coupling to the golden-prompt tests.
4. **Dashboard visibility:** a stopped ticket keeps its eligible label, so `listTrackedIssues` already returns it; `pickStateLabel` and `stateKind`/`stripeClass` must learn `ai-stopped` so it renders as its own "stopped" bucket rather than falling through to "queued".

---

## File Structure

- `config.go` — add `labelStopped` const, `StateLabels.Stopped` field, set it in `defaultStateLabels()`.
- `github.go` — include `Stopped` in `hasStateLabel`.
- `slots.go` — (unchanged) reference for the `active` ledger and `mu`.
- `loop.go` — add `cancels`/`stopping` maps to `Orchestrator`, cancellation helpers, `Stop`, `pause`, `Continue`, sentinel errors, `stoppedComment`; wire the child ctx + stop guard into the `ProcessOnce` goroutine and `handleIssue`.
- `tracker.go` — add `Stopped` to `trackedStateLabels` and `pickStateLabel`.
- `render.go` — add `"stopped"` cases to `stateKind` and `stripeClass`.
- `serve.go` — add `orch *Orchestrator` field, `POST /stop` + `POST /continue` routes, `handleStop`/`handleContinue`/`mutate`/`actionNotice`, and a `Notice` field on `view`.
- `main.go` — set `srv.orch = o` after `NewServer`.
- `web/templates/detail.html` — Notice banner, stopped chip color, Stop/Continue buttons.
- `web/templates/rail.html` — stopped chip color in the rail row.
- Tests live beside each file: `config_test.go`, `github_test.go`, `loop_test.go`, `render_test.go` (or `serve_test.go` for the render helpers — put new `stateKind`/`stripeClass` cases where the existing ones are tested), `serve_test.go`.

---

### Task 1: `ai-stopped` state label

**Files:**
- Modify: `config.go:12-18` (label consts), `config.go:62-72` (`StateLabels` + `defaultStateLabels`)
- Modify: `github.go:97-105` (`hasStateLabel`)
- Test: `config_test.go`, `github_test.go`

**Interfaces:**
- Produces: `labelStopped = "ai-stopped"`; `StateLabels.Stopped string`; `defaultStateLabels()` returns it set; `hasStateLabel` returns true for an issue carrying `Stopped`.

- [ ] **Step 1: Write the failing test (config default)**

Update the existing default assertion in `config_test.go` (around line 88) and add a focused test. Change the existing `want` literal to include `Stopped`:

```go
// in the existing TestLoadConfigDefaults-style test, replace the want literal:
want := StateLabels{WIP: "ai-wip", Failed: "ai-failed", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}
```

And add:

```go
func TestDefaultStateLabelsIncludesStopped(t *testing.T) {
	if got := defaultStateLabels().Stopped; got != "ai-stopped" {
		t.Fatalf("defaultStateLabels().Stopped = %q, want ai-stopped", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run 'TestDefaultStateLabelsIncludesStopped|TestLoadConfig' -v`
Expected: FAIL — `Stopped` is not a field of `StateLabels` (compile error) / default mismatch.

- [ ] **Step 3: Implement the label**

In `config.go`, add the const alongside the others (after `labelNeedsInfo`):

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

Add the field to `StateLabels`:

```go
type StateLabels struct {
	WIP       string `json:"wip"`
	Failed    string `json:"failed"`
	Done      string `json:"done"`
	Rework    string `json:"rework"`
	NeedsInfo string `json:"needsInfo"`
	Stopped   string `json:"stopped"`
}
```

Set it in `defaultStateLabels`:

```go
func defaultStateLabels() StateLabels {
	return StateLabels{WIP: labelWIP, Failed: labelFailed, Done: labelDone, Rework: labelRework, NeedsInfo: labelNeedsInfo, Stopped: labelStopped}
}
```

- [ ] **Step 4: Write the failing test (`hasStateLabel`)**

Add to `github_test.go`:

```go
func TestHasStateLabelExcludesStopped(t *testing.T) {
	g := NewGitHub(&fakeRunner{}, &Config{RepoSlug: "o/r", StateLabels: defaultStateLabels()})
	is := Issue{Number: 5, Labels: []Label{{Name: "ai-agent"}, {Name: "ai-stopped"}}}
	if !g.hasStateLabel(is) {
		t.Fatal("an issue carrying ai-stopped must count as having a state label (dropped from the eligible queue)")
	}
}
```

- [ ] **Step 5: Run to verify it fails**

Run: `go test ./... -run TestHasStateLabelExcludesStopped -v`
Expected: FAIL — `hasStateLabel` does not yet check `Stopped`.

- [ ] **Step 6: Implement `hasStateLabel`**

In `github.go`, extend the OR chain:

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

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS (all existing tests still green with the new default field).

- [ ] **Step 8: Commit**

```bash
git add config.go github.go config_test.go github_test.go
git commit -m "feat: add ai-stopped state label"
```

---

### Task 2: Orchestrator cancellation state + helpers + sentinels

**Files:**
- Modify: `loop.go:16-35` (`Orchestrator` struct fields)
- Modify: `loop.go` (add helpers + sentinel errors near the top, e.g. after the struct)
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `o.mu` (existing mutex), `o.active` (existing ledger).
- Produces:
  - fields `cancels map[int]context.CancelFunc`, `stopping map[int]bool`
  - `func (o *Orchestrator) setCancel(n int, cancel context.CancelFunc)`
  - `func (o *Orchestrator) clearCancel(n int)`
  - `func (o *Orchestrator) isStopping(n int) bool`
  - `func (o *Orchestrator) consumeStopping(n int) bool` (read-and-clear)
  - `var errNotRunning = errors.New("issue is not running")`
  - `var errAlreadyRunning = errors.New("issue is already running")`

- [ ] **Step 1: Write the failing test**

Add to `loop_test.go`:

```go
func TestCancellationHelpers(t *testing.T) {
	o := &Orchestrator{}
	called := false
	o.setCancel(7, func() { called = true })

	// isStopping is false until a stop is flagged.
	if o.isStopping(7) {
		t.Fatal("isStopping(7) = true before any stop")
	}
	// consumeStopping is false when nothing is flagged.
	if o.consumeStopping(7) {
		t.Fatal("consumeStopping(7) = true with no flag set")
	}

	o.stopping = map[int]bool{7: true}
	if !o.isStopping(7) {
		t.Fatal("isStopping(7) = false after flag set")
	}
	if !o.consumeStopping(7) {
		t.Fatal("consumeStopping(7) = false after flag set")
	}
	// consume cleared it.
	if o.isStopping(7) {
		t.Fatal("consumeStopping did not clear the flag")
	}

	o.clearCancel(7)
	if _, ok := o.cancels[7]; ok {
		t.Fatal("clearCancel did not remove the cancel func")
	}
	_ = called
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./... -run TestCancellationHelpers -v`
Expected: FAIL — `setCancel`/`isStopping`/etc. undefined (compile error).

- [ ] **Step 3: Add the fields**

In `loop.go`, extend the `Orchestrator` struct (add the two maps under the existing `mu`-guarded block):

```go
	mu            sync.Mutex
	active        map[int]struct{} // issue numbers with a pipeline in flight
	inFlight      sync.WaitGroup   // one Add per acquired slot; drained on shutdown
	resumeBackoff map[int]backoffState
	skipLogged    map[int]bool
	cancels       map[int]context.CancelFunc // per-issue cancel for the in-flight ProcessOnce pipeline
	stopping      map[int]bool               // issues whose current run was deliberately stopped
	now           func() time.Time // test seam; nil means time.Now
```

- [ ] **Step 4: Add the helpers and sentinels**

In `loop.go` (place after the `Orchestrator` struct / `clock` helper). Add `errors` is already imported:

```go
// errNotRunning is returned by Stop when no pipeline is in flight for the issue
// (never started, already finished, or a double Stop) — a no-op, surfaced to the
// dashboard as an inline message rather than an error.
var errNotRunning = errors.New("issue is not running")

// errAlreadyRunning is returned by Continue when the issue's pipeline is already
// in flight, so there is nothing to re-queue.
var errAlreadyRunning = errors.New("issue is already running")

// setCancel registers the in-flight pipeline's cancel func for issue n so Stop
// can cancel that one ticket's claude subprocess. Guarded by mu.
func (o *Orchestrator) setCancel(n int, cancel context.CancelFunc) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cancels == nil {
		o.cancels = map[int]context.CancelFunc{}
	}
	o.cancels[n] = cancel
}

// clearCancel forgets issue n's cancel func once its pipeline goroutine returns.
// Guarded by mu. The context's own resources are released by the goroutine's
// defer cancel(); this only removes the map entry Stop looks up.
func (o *Orchestrator) clearCancel(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.cancels, n)
}

// isStopping reports whether a Stop was requested for issue n's current run.
// Guarded by mu.
func (o *Orchestrator) isStopping(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stopping[n]
}

// consumeStopping reports whether a Stop was requested for issue n and clears the
// flag if so, so the pipeline goroutine transitions to ai-stopped exactly once.
// Guarded by mu.
func (o *Orchestrator) consumeStopping(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopping[n] {
		delete(o.stopping, n)
		return true
	}
	return false
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./... -run TestCancellationHelpers -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add loop.go loop_test.go
git commit -m "feat: add per-ticket cancellation state to the orchestrator"
```

---

### Task 3: `Stop(n)` method

**Files:**
- Modify: `loop.go` (add `Stop`)
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `o.cancels`, `o.stopping`, `errNotRunning` (Task 2).
- Produces: `func (o *Orchestrator) Stop(n int) error` — flags `stopping[n]`, calls the registered cancel, returns nil; returns `errNotRunning` when no cancel is registered.

- [ ] **Step 1: Write the failing test**

Add to `loop_test.go`:

```go
func TestStopFlagsAndCancelsRunningTicket(t *testing.T) {
	o := &Orchestrator{}
	cancelled := false
	o.setCancel(7, func() { cancelled = true })

	if err := o.Stop(7); err != nil {
		t.Fatalf("Stop(7) on a running ticket: %v", err)
	}
	if !cancelled {
		t.Fatal("Stop did not invoke the registered cancel func")
	}
	if !o.isStopping(7) {
		t.Fatal("Stop did not set the stopping flag")
	}
}

func TestStopNotRunningReturnsSentinel(t *testing.T) {
	o := &Orchestrator{}
	if err := o.Stop(99); err != errNotRunning {
		t.Fatalf("Stop(99) with nothing in flight = %v, want errNotRunning", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./... -run 'TestStop' -v`
Expected: FAIL — `Stop` undefined.

- [ ] **Step 3: Implement `Stop`**

In `loop.go`:

```go
// Stop cancels the in-flight pipeline for issue n mid-turn and flags the run so
// its goroutine parks the ticket as ai-stopped (via pause) as it unwinds. It
// returns immediately — the label transition is eventually consistent, surfacing
// on the dashboard's 3s poll a moment later. A ticket with no pipeline in flight
// (never started, already finished, double Stop) returns errNotRunning: a no-op.
func (o *Orchestrator) Stop(n int) error {
	o.mu.Lock()
	cancel, ok := o.cancels[n]
	if !ok {
		o.mu.Unlock()
		return errNotRunning
	}
	if o.stopping == nil {
		o.stopping = map[int]bool{}
	}
	o.stopping[n] = true
	o.mu.Unlock()
	cancel() // kills the claude subprocess via exec.CommandContext
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./... -run 'TestStop' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add loop.go loop_test.go
git commit -m "feat: add Orchestrator.Stop"
```

---

### Task 4: `pause(ctx, n)` terminal outcome

**Files:**
- Modify: `loop.go` (add `pause` + `stoppedComment`)
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `o.gh.SwapLabels`, `o.gh.Comment`, `recordState`, `o.issueLogDir`, `o.cfg.StateLabels.WIP`/`.Stopped`.
- Produces: `func (o *Orchestrator) pause(ctx context.Context, n int)`; `func stoppedComment() string`.

- [ ] **Step 1: Write the failing test**

Add to `loop_test.go`. This asserts the transition, the state file, the preserved session, and that no park cause is recorded:

```go
func TestPauseTransitionsToStoppedAndPreservesState(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A recorded session that pause must leave untouched.
	if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(`{"sessionId":"s1","kind":"bug"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	recordState(logDir, "ai-wip")

	o.pause(context.Background(), 7)

	// ai-wip -> ai-stopped as one atomic swap.
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-stopped") {
		t.Fatalf("want single ai-wip->ai-stopped swap, got %v", swap)
	}
	if got := env.readLocalState(7); got != "ai-stopped" {
		t.Fatalf("local state = %q, want ai-stopped", got)
	}
	// Session preserved.
	if si, err := readSession(logDir); err != nil || si.SessionID != "s1" {
		t.Fatalf("session not preserved: %+v err=%v", si, err)
	}
	// No park cause recorded — nothing auto-resumes a stopped ticket.
	if c := readParkCause(logDir); c != "" {
		t.Fatalf("pause recorded a park cause %q, want none", c)
	}
	// A stop comment was posted.
	var commented bool
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "Stopped by user") {
			commented = true
		}
	}
	if !commented {
		t.Fatal("pause did not comment the stop notice")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./... -run TestPauseTransitions -v`
Expected: FAIL — `pause` undefined.

- [ ] **Step 3: Implement `pause` and `stoppedComment`**

In `loop.go`:

```go
// pause is the terminal outcome for a user-stopped run: swap ai-wip->ai-stopped,
// record the state, and comment. It runs on the LIVE parent ctx (the pipeline's
// child ctx is already cancelled, so its GitHub calls would fail). It
// deliberately does NOT touch the worktree, branch, logs, or session file, and
// records NO park cause — so no auto-resume path (SweepOrphans queries ai-wip,
// ResumeParked queries ai-rework) will ever act on a stopped ticket. It stays
// put until the user hits Continue.
func (o *Orchestrator) pause(ctx context.Context, n int) {
	logDir := o.issueLogDir(n)
	_ = o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Stopped)
	recordState(logDir, o.cfg.StateLabels.Stopped)
	_ = o.gh.Comment(ctx, n, stoppedComment())
}

// stoppedComment is the fixed notice posted when a run is stopped by the user.
func stoppedComment() string {
	return "⏸ Stopped by user. Worktree, logs and session are preserved. Press Continue to resume."
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./... -run TestPauseTransitions -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add loop.go loop_test.go
git commit -m "feat: add pause terminal outcome for stopped tickets"
```

---

### Task 5: Wire the child ctx + stop guard into `ProcessOnce` / `handleIssue`

**Files:**
- Modify: `loop.go:102-125` (`ProcessOnce` goroutine), `loop.go:159-200` (`handleIssue`)
- Test: `loop_test.go` (uses `gatePipelines`, `awaitStarted` from `concurrency_helpers_test.go`)

**Interfaces:**
- Consumes: `setCancel`/`clearCancel`/`isStopping`/`consumeStopping` (Task 2), `Stop` (Task 3), `pause` (Task 4).
- Produces: a Stop landing mid-pipeline drives the ticket to `ai-stopped` and skips the normal park/ship/finish outcome.

- [ ] **Step 1: Write the failing integration test**

Add to `loop_test.go`. It holds the pipeline in the gate, stops it, releases, and asserts the stopped transition and that no PR was created (ship skipped):

```go
func TestStopDuringPipelineParksAsStopped(t *testing.T) {
	env := newFakeEnv(t) // issue 7 eligible; pipeline succeeds unless stopped
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	go func() { _ = o.ProcessOnce(context.Background()) }()
	n := awaitStarted(t, started, 1)[0]

	if err := o.Stop(n); err != nil {
		t.Fatalf("Stop(%d): %v", n, err)
	}
	close(release) // let the gated claude call return
	o.Wait()

	// The run ends in ai-stopped, not shipped.
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-stopped") {
		t.Fatalf("want ai-wip->ai-stopped swap, got %v", swap)
	}
	if got := env.readLocalState(n); got != "ai-stopped" {
		t.Fatalf("local state = %q, want ai-stopped", got)
	}
	// Ship was skipped: no PR created.
	if pr := env.callsMatching("gh", "pr create"); len(pr) != 0 {
		t.Fatalf("a stopped run must not ship a PR, got %v", pr)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./... -run TestStopDuringPipelineParksAsStopped -v`
Expected: FAIL — the pipeline currently ships (PR created) because there is no stop guard.

- [ ] **Step 3: Wire the `ProcessOnce` goroutine**

In `loop.go`, replace the goroutine body inside `ProcessOnce`'s `for i := range picks` loop with the child-ctx version:

```go
		go func(p pick) {
			n := p.issue.Number
			// release is deferred FIRST so it runs LAST: a panicking pipeline parks
			// the issue in the recover handler below and still returns its slot.
			defer o.release(n)
			// Derive a per-ticket child ctx and register its cancel so Stop can kill
			// this one pipeline's claude subprocess without touching its siblings.
			cctx, cancel := context.WithCancel(ctx)
			defer cancel() // release the context's resources when the goroutine ends
			o.setCancel(n, cancel)
			defer o.clearCancel(n)
			// A panic in one pipeline must not kill the daemon or the sibling
			// pipelines: park the issue with the panic as its (non-resumable) cause,
			// preserving worktree and logs for a human. Uses the LIVE parent ctx.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("issue #%d: pipeline panic: %v\n%s", n, r, debug.Stack())
					_ = o.park(ctx, n, o.cfg.StateLabels.WIP, fmt.Errorf("panic: %v", r))
				}
			}()
			log.Printf("issue #%d (%s): %s", n, p.kind, p.reason)
			if err := o.handleIssue(cctx, p.issue, p.kind, base); err != nil {
				log.Printf("issue #%d: pipeline failed: %v", n, err)
			}
			// A Stop observed during the run transitions the ticket to ai-stopped
			// here, on the live parent ctx (the child ctx is cancelled). handleIssue
			// already skipped its normal outcome, leaving the ticket ai-wip for this.
			if o.consumeStopping(n) {
				o.pause(ctx, n)
			}
		}(picks[i])
```

- [ ] **Step 4: Add the stop guard to `handleIssue`**

In `loop.go`, in `handleIssue`, immediately after the pipeline call returns and **before** the outcome switch (`var done *alreadyDoneError`), insert the guard:

```go
	var perr error
	if kind == "bug" {
		perr = RunBugPipeline(ctx, c, o.cfg, wtPath, content)
	} else {
		perr = RunFeaturePipeline(ctx, c, o.cfg, wtPath, content, readPersona(o.cfg.PersonaPath))
	}
	// A Stop landed during the pipeline: skip the normal park/ship/finish outcome
	// and leave the ticket ai-wip. The launching goroutine's consumeStopping+pause
	// transitions it to ai-stopped on the live parent ctx.
	if o.isStopping(n) {
		return nil
	}
	var done *alreadyDoneError
	if errors.As(perr, &done) {
		return o.finishDone(ctx, n, wtPath, branch, o.cfg.StateLabels.WIP, done.reason)
	}
	// ... rest unchanged ...
```

(`n` is already bound at the top of `handleIssue` as `n := issue.Number`.)

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./... -run TestStopDuringPipelineParksAsStopped -v`
Expected: PASS.

- [ ] **Step 6: Run the full suite (regression on ProcessOnce)**

Run: `go test ./...`
Expected: PASS — the normal (non-stopped) pipeline still ships, panics still park, slots still balance.

- [ ] **Step 7: Commit**

```bash
git add loop.go loop_test.go
git commit -m "feat: cancel and park a running ticket on Stop"
```

---

### Task 6: `Continue(ctx, n)` deferred resume

**Files:**
- Modify: `loop.go` (add `Continue`)
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `o.active`, `readSession`, `o.gh.SwapLabels`/`RemoveLabel`, `recordState`, `recordParkCause`, `clearState`, `clearParkCause`, `interruptedCause` (existing), `errAlreadyRunning` (Task 2).
- Produces: `func (o *Orchestrator) Continue(ctx context.Context, n int) error`. Session present → `ai-stopped → ai-rework` + resumable park cause. No session → remove `ai-stopped`, clear state/cause (re-queued as eligible). Running → `errAlreadyRunning`.

- [ ] **Step 1: Write the failing tests**

Add to `loop_test.go`:

```go
func TestContinueWithSessionQueuesRework(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(`{"sessionId":"s1","kind":"bug"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	recordState(logDir, "ai-stopped")

	if err := o.Continue(context.Background(), 7); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	swap := env.callsMatching("gh", "--remove-label ai-stopped")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-rework") {
		t.Fatalf("want ai-stopped->ai-rework swap, got %v", swap)
	}
	if got := env.readLocalState(7); got != "ai-rework" {
		t.Fatalf("local state = %q, want ai-rework", got)
	}
	if c := readParkCause(logDir); c != interruptedCause {
		t.Fatalf("park cause = %q, want %q (resumable)", c, interruptedCause)
	}
}

func TestContinueWithoutSessionRequeuesEligible(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordState(logDir, "ai-stopped")
	recordParkCause(logDir, "whatever")

	if err := o.Continue(context.Background(), 7); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	// ai-stopped removed (not swapped) so the ticket falls back to eligible.
	rm := env.callsMatching("gh", "--remove-label ai-stopped")
	if len(rm) != 1 || strings.Contains(rm[0], "--add-label") {
		t.Fatalf("want a bare ai-stopped removal, got %v", rm)
	}
	if got := env.readLocalState(7); got != "" {
		t.Fatalf("local state = %q, want cleared", got)
	}
	if c := readParkCause(logDir); c != "" {
		t.Fatalf("park cause = %q, want cleared", c)
	}
}

func TestContinueWhileRunningReturnsSentinel(t *testing.T) {
	o := &Orchestrator{active: map[int]struct{}{7: {}}}
	if err := o.Continue(context.Background(), 7); err != errAlreadyRunning {
		t.Fatalf("Continue while running = %v, want errAlreadyRunning", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./... -run TestContinue -v`
Expected: FAIL — `Continue` undefined.

- [ ] **Step 3: Implement `Continue`**

In `loop.go`:

```go
// Continue re-queues a stopped issue for a deferred resume: it only rewrites
// labels/state on disk, and the next runLoop cycle picks it up when a slot is
// free (never synchronously, never bypassing the concurrency budget). With a
// preserved session it hands the issue to the auto-resume path (ai-stopped ->
// ai-rework + a resumable park cause, so ResumeParked -> Rework resumes from the
// session id). Without a session it re-queues from scratch (remove ai-stopped,
// clear state/cause, so the issue is eligible again and ProcessOnce runs a fresh
// pipeline; the worktree, if any, is reused per the project's continue-not-reset
// rule). Being label-driven, it survives a daemon restart — the maps are empty
// but the session file on disk is the source of truth. Returns errAlreadyRunning
// if the issue's pipeline is somehow already in flight.
func (o *Orchestrator) Continue(ctx context.Context, n int) error {
	o.mu.Lock()
	_, running := o.active[n]
	o.mu.Unlock()
	if running {
		return errAlreadyRunning
	}
	logDir := o.issueLogDir(n)
	si, _ := readSession(logDir)
	if si.SessionID != "" {
		if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.Stopped, o.cfg.StateLabels.Rework); err != nil {
			return err
		}
		recordState(logDir, o.cfg.StateLabels.Rework)
		recordParkCause(logDir, interruptedCause)
		return nil
	}
	if err := o.gh.RemoveLabel(ctx, n, o.cfg.StateLabels.Stopped); err != nil {
		return err
	}
	clearState(logDir)
	clearParkCause(logDir)
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./... -run TestContinue -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add loop.go loop_test.go
git commit -m "feat: add Orchestrator.Continue deferred resume"
```

---

### Task 7: Server→Orchestrator link + `POST /stop` and `POST /continue`

**Files:**
- Modify: `serve.go:33-48` (`Server` struct, add `orch`), `serve.go:65-72` (`Handler` routes), `serve.go:82-88` (`view`, add `Notice`), add handlers
- Modify: `main.go:114-117` (set `srv.orch = o`)
- Test: `serve_test.go`

**Interfaces:**
- Consumes: `Orchestrator.Stop` (Task 3), `Orchestrator.Continue` (Task 6), `errNotRunning`/`errAlreadyRunning`.
- Produces: `Server.orch *Orchestrator`; routes `POST /stop`, `POST /continue`; `view.Notice string`; handlers render the refreshed `detail` fragment.

- [ ] **Step 1: Write the failing tests**

Add to `serve_test.go`. A small POST helper plus routing/behavior tests. Wire a real orchestrator over the same fake runner so the `gh` swap is observable:

```go
func post(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, nil).WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// serverWithOrch builds a Server and a real Orchestrator sharing one fake runner
// and workDir, with issue 7's log dir seeded to the given state.
func serverWithOrch(t *testing.T, state string, session bool) (*Server, *fakeEnv) {
	t.Helper()
	env := newFakeEnv(t)
	cfg := &Config{
		RepoPath: "/clone", RepoSlug: "org/repo", EligibleLabel: "ai-agent",
		WorkDir: env.wtDir, MaxQARounds: 3, StateLabels: defaultStateLabels(),
		Models: Models{Architect: ModelConfig{Model: "opus"}, Triage: ModelConfig{Model: "sonnet"}},
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if session {
		if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(`{"sessionId":"s1","kind":"bug"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recordState(logDir, state)
	o := &Orchestrator{cfg: cfg, runner: env.f, gh: NewGitHub(env.f, cfg), wt: &Worktree{runner: env.f, repoPath: cfg.RepoPath}}
	o.gh.retry = testRetry
	s, err := NewServer(env.f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.orch = o
	return s, env
}

func TestContinueRouteQueuesReworkAndRendersFragment(t *testing.T) {
	s, env := serverWithOrch(t, "ai-stopped", true /* session */)
	code, body := post(t, s.Handler(), "/continue?issue=7")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if strings.Contains(body, "<html") {
		t.Fatal("continue should return the detail fragment, not a full page")
	}
	swap := env.callsMatching("gh", "--remove-label ai-stopped")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-rework") {
		t.Fatalf("continue did not queue rework, got %v", swap)
	}
}

func TestStopRouteInvokesOrchestratorAndRenders(t *testing.T) {
	s, _ := serverWithOrch(t, "ai-wip", true)
	// Nothing is in flight, so Stop returns errNotRunning and we render an inline
	// notice — not a 5xx.
	code, body := post(t, s.Handler(), "/stop?issue=7")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (sentinel is non-fatal)", code)
	}
	if !strings.Contains(body, "not running") {
		t.Fatalf("expected an inline not-running notice, got: %s", body)
	}
}

func TestMutateRouteRejectsBadIssue(t *testing.T) {
	s, _ := serverWithOrch(t, "ai-wip", true)
	code, _ := post(t, s.Handler(), "/stop?issue=abc")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a non-numeric issue", code)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./... -run 'TestContinueRoute|TestStopRoute|TestMutateRoute' -v`
Expected: FAIL — `s.orch` field, `/stop`/`/continue` routes, and `Notice` do not exist yet.

- [ ] **Step 3: Add the `orch` field and `Notice`**

In `serve.go`, add the field to `Server`:

```go
type Server struct {
	runner Runner
	cfg    *Config
	gh     *GitHub
	tmpl   *template.Template
	orch   *Orchestrator // mutation endpoints (/stop, /continue); nil for read-only servers

	ttl time.Duration
	now func() time.Time
	// ... unchanged ...
}
```

Add `Notice` to `view`:

```go
type view struct {
	Tickets  []Ticket
	Selected *Ticket
	GHError  string
	Notice   string
	Stats    stats
}
```

- [ ] **Step 4: Add the routes and handlers**

In `serve.go`, extend `Handler`:

```go
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /rail", s.handleRail)
	mux.HandleFunc("GET /detail", s.handleDetail)
	mux.HandleFunc("POST /stop", s.handleStop)
	mux.HandleFunc("POST /continue", s.handleContinue)
	mux.Handle("GET /static/", staticHandler())
	return mux
}
```

Add the handlers (e.g. right after `handleDetail`):

```go
// handleStop cancels the running pipeline for the posted issue and re-renders the
// detail fragment. Stop is asynchronous, so the fragment may still show ai-wip;
// the 3s poll shows the flip to stopped a moment later.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, func(n int) error { return s.orch.Stop(n) })
}

// handleContinue re-queues the posted stopped issue for a deferred resume and
// re-renders the detail fragment (already showing the new ai-rework/eligible
// state, since Continue rewrites the labels synchronously).
func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, func(n int) error { return s.orch.Continue(r.Context(), n) })
}

// mutate parses the issue number, runs the orchestrator action, and renders the
// refreshed detail fragment htmx swaps into #main. A sentinel error (the ticket
// finished between render and click, or is already running) is a non-fatal inline
// notice, not an HTTP error; only a malformed issue number is a 400.
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, action func(int) error) {
	n, err := strconv.Atoi(r.FormValue("issue"))
	if err != nil {
		http.Error(w, "invalid issue number", http.StatusBadRequest)
		return
	}
	actErr := action(n)
	v := s.load(r.Context(), strconv.Itoa(n))
	if actErr != nil {
		v.Notice = actionNotice(actErr)
	}
	renderHTML(w, s.tmpl, "detail", v)
}

// actionNotice maps a mutation error to a friendly inline message.
func actionNotice(err error) string {
	switch err {
	case errNotRunning:
		return "That ticket is not running — it may have already finished."
	case errAlreadyRunning:
		return "That ticket is already running."
	default:
		return "Action failed: " + err.Error()
	}
}
```

(`strconv` is already imported in `serve.go`.)

- [ ] **Step 5: Render the Notice in the detail fragment**

In `web/templates/detail.html`, add a notice banner right after the opening `<div>` and before the `{{if .GHError}}` block (line 1-2):

```html
{{define "detail"}}<div class="max-w-[1160px] px-10 py-7">
 {{if .Notice}}<div class="mb-5 rounded-md border border-live/30 bg-live/[0.06] px-4 py-3 font-mono text-[12px] leading-relaxed text-live/90">{{.Notice}}</div>{{end}}
 {{if .GHError}}<div class="mb-5 flex items-start gap-2 rounded-md border border-warn/30 bg-warn/[0.06] px-4 py-3 font-mono text-[12px] leading-relaxed text-warn/90"><span class="mt-px">⚠</span><span>GitHub unreachable — showing local logs only. Titles and states may be missing.<br><span class="text-warn/60">{{.GHError}}</span></span></div>{{end}}
```

- [ ] **Step 6: Wire the orchestrator in `main.go`**

In `main.go`, after `NewServer` succeeds, set the field:

```go
	srv, err := NewServer(r, cfg)
	if err != nil {
		log.Fatalf("dashboard: %v", err)
	}
	srv.orch = o // enable the /stop and /continue mutation endpoints
```

- [ ] **Step 7: Run to verify it passes**

Run: `go test ./... -run 'TestContinueRoute|TestStopRoute|TestMutateRoute' -v`
Expected: PASS.

- [ ] **Step 8: Run the full suite + build**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add serve.go main.go web/templates/detail.html serve_test.go
git commit -m "feat: add /stop and /continue dashboard endpoints"
```

---

### Task 8: UI — stopped state rendering + Stop/Continue buttons

**Files:**
- Modify: `render.go:168-199` (`stateKind`, `stripeClass`)
- Modify: `tracker.go:507-509` (`trackedStateLabels`), `tracker.go:526-541` (`pickStateLabel`)
- Modify: `web/templates/detail.html` (stopped chip color + buttons)
- Modify: `web/templates/rail.html` (stopped chip color)
- Test: `serve_test.go` (render helpers + fragment content)

**Interfaces:**
- Consumes: `cfg.StateLabels.Stopped` (Task 1).
- Produces: `stateKind(cfg, "ai-stopped") == "stopped"`; `stripeClass` maps `"stopped"`; `pickStateLabel` prefers `Stopped` over `Done`/eligible; detail header renders a Stop button for `wip` and a Continue button for `stopped`.

- [ ] **Step 1: Write the failing tests (render helpers + buttons)**

Add to `serve_test.go`:

```go
func TestStateKindMapsStopped(t *testing.T) {
	cfg := &Config{StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	if got := stateKind(cfg, "ai-stopped"); got != "stopped" {
		t.Fatalf("stateKind(ai-stopped) = %q, want stopped", got)
	}
	if got := stripeClass(cfg, "ai-stopped"); got == "bg-line2" || got == "" {
		t.Fatalf("stripeClass(ai-stopped) = %q, want a distinct stopped tone", got)
	}
}

func TestPickStateLabelPrefersStoppedOverEligible(t *testing.T) {
	cfg := &Config{StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	labels := []Label{{Name: "ai-agent"}, {Name: "ai-stopped"}}
	if got := pickStateLabel(labels, cfg); got != "ai-stopped" {
		t.Fatalf("pickStateLabel = %q, want ai-stopped (not the eligible label)", got)
	}
}

func TestDetailRendersStopButtonForWip(t *testing.T) {
	// newTestServer seeds issue 142 labeled ai-wip.
	code, body := get(t, newTestServer(t).Handler(), "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, `hx-post="/stop?issue=142"`) {
		t.Fatalf("wip detail should render a Stop button, got: %s", body)
	}
	if strings.Contains(body, `hx-post="/continue?issue=142"`) {
		t.Fatal("wip detail must not render a Continue button")
	}
}

func TestDetailRendersContinueButtonForStopped(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordState(dir, "ai-stopped")
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: `[{"number":142,"title":"Add OAuth","labels":[{"name":"ai-agent"},{"name":"ai-stopped"}]}]`}}}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	code, body := get(t, s.Handler(), "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, `hx-post="/continue?issue=142"`) {
		t.Fatalf("stopped detail should render a Continue button, got: %s", body)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./... -run 'TestStateKindMapsStopped|TestPickStateLabelPrefersStopped|TestDetailRendersStopButton|TestDetailRendersContinueButton' -v`
Expected: FAIL — `stateKind` returns `""` for `ai-stopped`; no buttons in the template.

- [ ] **Step 3: Add the `stateKind` and `stripeClass` cases**

In `render.go`, extend `stateKind`:

```go
func stateKind(cfg *Config, label string) string {
	switch label {
	case cfg.StateLabels.Done:
		return "done"
	case cfg.StateLabels.WIP:
		return "wip"
	case cfg.StateLabels.Rework:
		return "rework"
	case cfg.StateLabels.Failed:
		return "failed"
	case cfg.StateLabels.Stopped:
		return "stopped"
	case cfg.EligibleLabel:
		return "queued"
	default:
		return ""
	}
}
```

And `stripeClass` (a muted/paused tone):

```go
func stripeClass(cfg *Config, label string) string {
	switch stateKind(cfg, label) {
	case "done":
		return "bg-ok/50"
	case "wip":
		return "bg-live"
	case "rework":
		return "bg-warn/80"
	case "failed":
		return "bg-err/70"
	case "stopped":
		return "bg-muted/60"
	default:
		return "bg-line2"
	}
}
```

- [ ] **Step 4: Teach `pickStateLabel` and `trackedStateLabels` about stopped**

In `tracker.go`, add `Stopped` to `trackedStateLabels` (the no-eligible-label fallback set):

```go
func trackedStateLabels(cfg *Config) []string {
	return []string{cfg.StateLabels.WIP, cfg.StateLabels.Done, cfg.StateLabels.Rework, cfg.StateLabels.Stopped}
}
```

And to `pickStateLabel`'s priority order, after `Rework` and before `Done` (a stopped ticket keeps its eligible label, so `Stopped` must outrank the eligible fallback):

```go
	for _, name := range []string{cfg.StateLabels.WIP, cfg.StateLabels.Rework, cfg.StateLabels.Stopped, cfg.StateLabels.Done, cfg.EligibleLabel} {
		if name != "" && has(name) {
			return name
		}
	}
```

- [ ] **Step 5: Add the stopped chip color + buttons to `detail.html`**

In `web/templates/detail.html`, extend the chip color branch on line 8 to handle `stopped` (add before the trailing `{{else}}`):

```html
    {{if $k}}<span class="inline-flex items-center gap-1.5 rounded border px-2 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-widest {{if eq $k "done"}}border-ok/30 bg-ok/10 text-ok{{else if eq $k "wip"}}border-live/30 bg-live/10 text-live{{else if eq $k "rework"}}border-warn/30 bg-warn/10 text-warn{{else if eq $k "failed"}}border-err/30 bg-err/10 text-err{{else if eq $k "stopped"}}border-muted/30 bg-muted/10 text-muted{{else}}border-line2 bg-panel2 text-muted{{end}}">{{if eq $k "wip"}}<span class="hb inline-block h-1.5 w-1.5 rounded-full bg-live"></span>in progress{{else if eq $k "done"}}done{{else if eq $k "stopped"}}stopped{{else}}{{$k}}{{end}}</span>{{end}}
```

Then add the Stop/Continue buttons inside the header's flex row, right after the kind chip on line 9 (still inside the `<div class="mb-2.5 flex flex-wrap items-center gap-2">`):

```html
    {{if .Kind}}<span class="rounded border border-line2 bg-panel2 px-2 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-widest text-muted">{{.Kind}}</span>{{end}}
    {{if eq $k "wip"}}<button type="button" hx-post="/stop?issue={{.Number}}" hx-confirm="Stop this ticket? The current turn's work will be lost." hx-target="#main" hx-swap="innerHTML" class="ml-auto rounded border border-warn/40 bg-warn/10 px-2.5 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-widest text-warn hover:bg-warn/20">Stop</button>{{end}}
    {{if eq $k "stopped"}}<button type="button" hx-post="/continue?issue={{.Number}}" hx-confirm="Continue this ticket?" hx-target="#main" hx-swap="innerHTML" class="ml-auto rounded border border-live/40 bg-live/10 px-2.5 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-widest text-live hover:bg-live/20">Continue</button>{{end}}
```

- [ ] **Step 6: Add the stopped chip color to `rail.html`**

In `web/templates/rail.html`, extend the chip color branch on line 16 to handle `stopped` (add before the trailing `{{else}}`):

```html
      {{if $k}}<span class="inline-flex items-center gap-1 rounded-sm border px-1.5 py-px font-mono text-[9px] font-semibold uppercase tracking-wide {{if eq $k "done"}}border-ok/25 bg-ok/[0.13] text-ok{{else if eq $k "wip"}}border-live/30 bg-live/10 text-live{{else if eq $k "rework"}}border-warn/30 bg-warn/10 text-warn{{else if eq $k "failed"}}border-err/30 bg-err/10 text-err{{else if eq $k "stopped"}}border-muted/30 bg-muted/10 text-muted{{else}}border-line2 bg-panel2 text-muted{{end}}">{{if eq $k "wip"}}<span class="hb inline-block h-1 w-1 rounded-full bg-live"></span>{{end}}{{$k}}</span>{{end}}
```

- [ ] **Step 7: Run to verify it passes**

Run: `go test ./... -run 'TestStateKindMapsStopped|TestPickStateLabelPrefersStopped|TestDetailRendersStopButton|TestDetailRendersContinueButton' -v`
Expected: PASS.

- [ ] **Step 8: Run the full suite + build**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add render.go tracker.go web/templates/detail.html web/templates/rail.html serve_test.go
git commit -m "feat: render stopped state and Stop/Continue buttons"
```

---

## Self-Review

**1. Spec coverage:**

| Spec section | Task |
|---|---|
| §1 new `ai-stopped` label + `hasStateLabel` | Task 1 |
| §2 per-ticket cancellation (`cancels`/`stopping`, helpers, child ctx, `handleIssue` guard) | Tasks 2, 5 |
| §3 `Stop(n)` | Task 3 |
| §4 `pause(ctx, n)` | Task 4 |
| §5 `Continue(ctx, n)` deferred resume | Task 6 |
| §6 HTTP endpoints + Server→Orchestrator link | Task 7 |
| §7 UI (detail buttons, `hx-confirm`, rail/render stopped case) | Tasks 7 (buttons+notice), 8 (chip/stripe) |
| Testing (TDD) list | distributed across Tasks 1–8 (each behavior has a named test) |

Every testing bullet in the spec maps to a test: `Stop` (Task 3), `pause` (Task 4), `handleIssue` stop path (Task 5), `Continue` with/without session and while running (Task 6), `hasStateLabel` (Task 1), HTTP routes + sentinel inline (Task 7), render `stateKind`/buttons (Task 8).

**2. Placeholder scan:** No `TBD`/`handle edge cases`/"similar to Task N" — every code and test step shows full content.

**3. Type consistency:** Method names are stable across tasks: `setCancel`/`clearCancel`/`isStopping`/`consumeStopping` (Task 2) are consumed verbatim in Tasks 3/5; `Stop` (Task 3), `pause`+`stoppedComment` (Task 4), `Continue` (Task 6) match their call sites in Tasks 5/7; `errNotRunning`/`errAlreadyRunning` (Task 2) are consumed in Tasks 3/6/7; `view.Notice`/`s.orch`/`actionNotice`/`mutate` (Task 7) are self-consistent; `stateKind`/`stripeClass`/`pickStateLabel`/`trackedStateLabels` (Task 8) match the existing signatures. `StateLabels.Stopped` (Task 1) is the single field every later task reads.

**Deviations from spec (documented in Global Constraints → Assumptions):** cancel registration is `ProcessOnce`-only (honors decision 1); `Server` gets `orch` via a field set in `main.go` rather than a `NewServer` parameter (smaller blast radius, same end state); the stop comment is an inline literal.
