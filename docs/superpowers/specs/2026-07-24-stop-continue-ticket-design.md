# Stop / Continue a ticket from the dashboard (#3)

**Status:** Design approved (product-owner answers to the seven clarifying
questions constitute sign-off). Ready for implementation planning.

## Goal

Let a user **Stop** a currently-running ticket and later **Continue** it from
the dashboard UI (no CLI). Stop deletes nothing; Continue restarts the stopped
ticket, resuming from the persisted Claude session id when one exists.

## Scope decisions (from the product owner)

1. **Only a currently-running ticket** (state `ai-wip`, pipeline in flight) can
   be Stopped. Queued/eligible tickets and already-parked `ai-rework` tickets
   are out of scope.
2. **"Stopped" is a new GitHub label `ai-stopped`** (a real, configurable state
   label — not a local-only marker).
3. Board state while stopped **is** `ai-stopped`.
4. If a ticket is stopped **before any session id is recorded**, Continue
   **restarts the pipeline from scratch**.
5. Continue is **deferred**: it marks the ticket "continue-requested" and the
   next free slot picks it up (same model as `ResumeParked`). It never runs the
   pipeline synchronously and never bypasses the concurrency budget.
6. The mutating buttons **require a confirmation step**.
7. Stop cancels the ticket's context, which kills the `claude` subprocess
   mid-turn. **Losing the in-progress turn is acceptable**; the last *completed*
   turn's session is preserved and Continue resumes from there.

## Background: what the codebase already provides

- The daemon loop (`runLoop` driving `Orchestrator`) and the read-only HTTP
  `Server` run in **one process** (`main.go`), but the `Server` holds **no
  reference to the `Orchestrator`** — it is a disk+GitHub read view only.
- Pipelines run as goroutines sharing the single root ctx; there is **no
  per-ticket cancellation** today.
- Session ids are persisted per turn to `<logDir>/session`
  (`RecordSession`/`readSession`, `claude.go`). `Orchestrator.Rework`
  (`rework.go`) already resumes a preserved worktree + persisted session id via
  `claude --resume <id>` — this **is** the "Continue from session" primitive.
- State is label-driven. `ListEligibleIssues` drops any issue carrying a state
  label (`hasStateLabel`, `github.go`). `ResumeParked` queries only
  `ai-rework`; `SweepOrphans` queries only `ai-wip`. The in-process slot ledger
  (`slots.go`, `active map[int]struct{}`) is authoritative for concurrency.

Three gaps must be bridged: (a) no Server→Orchestrator link, (b) no per-ticket
cancellation, (c) no mutation endpoints and no `ai-stopped` label.

## Design

### 1. New state label `ai-stopped`

- Add `labelStopped = "ai-stopped"` (`config.go`).
- Add `Stopped string` to `StateLabels`; set it in `defaultStateLabels()`.
- **Include it in `hasStateLabel`** (`github.go`) so a stopped ticket is dropped
  by `ListEligibleIssues` and never re-picked by `ProcessOnce`.

Because `ai-stopped` is neither `ai-wip` nor `ai-rework`, it is invisible to
`SweepOrphans` and `ResumeParked`. A stopped ticket therefore survives a daemon
restart **untouched** — no auto-resume — which is exactly the intended
"stopped until the user hits Continue" semantics.

### 2. Per-ticket cancellation in the Orchestrator

Add to `Orchestrator` (all guarded by the existing `mu`):

```go
cancels  map[int]context.CancelFunc // per-issue cancel for the in-flight pipeline
stopping map[int]bool               // issues whose current run was deliberately stopped
```

Each pipeline-launch goroutine (in both `ProcessOnce` and `ResumeParked`)
derives a child context and registers its cancel:

```go
go func() {
    defer o.release(n)
    cctx, cancel := context.WithCancel(ctx)
    o.setCancel(n, cancel)      // stores in o.cancels[n] under mu
    defer o.clearCancel(n)      // deletes o.cancels[n] under mu; calls cancel()
    defer func() { recover -> park }()   // unchanged panic guard
    o.handleIssue(cctx, issue, kind, base)
    if o.consumeStopping(n) {   // true+clears if Stop was requested
        o.pause(ctx, n)         // NOTE: parent ctx, still live (not cctx)
    }
}()
```

`handleIssue` receives the **child** ctx so a Stop kills its `claude`
subprocess and aborts any in-flight git work. Right after the pipeline call
returns and **before** its outcome switch, `handleIssue` checks the stopping
flag and, if set, returns without applying any outcome (leaves the ticket
`ai-wip` for `pause` to transition):

```go
err := RunFeaturePipeline(...)   // or RunBugPipeline
if o.isStopping(n) {
    return   // skip normal park/ship/finish; goroutine's pause handles it
}
switch { ... existing outcomes ... }
```

Helper methods (all lock `mu`): `setCancel`, `clearCancel`, `isStopping`
(read), `consumeStopping` (read-and-clear).

### 3. `Stop(n)` — Orchestrator method

```go
func (o *Orchestrator) Stop(n int) error {
    o.mu.Lock()
    cancel, ok := o.cancels[n]
    if !ok {
        o.mu.Unlock()
        return errNotRunning   // sentinel: nothing in flight to stop
    }
    o.stopping[n] = true
    o.mu.Unlock()
    cancel()   // kills the claude subprocess via exec.CommandContext
    return nil
}
```

Stop returns immediately. The label transition to `ai-stopped` happens
**asynchronously** in the pipeline goroutine once `handleIssue` unwinds, via
`pause`. This is eventually consistent: the rail (3s htmx poll) shows the
ticket flip to stopped a moment later.

### 4. `pause(ctx, n)` — new terminal outcome

Uses the **live parent ctx** (the child ctx is cancelled, so its GitHub calls
would fail):

```go
func (o *Orchestrator) pause(ctx context.Context, n int) {
    logDir := issueLogDir(n)
    o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Stopped)
    recordState(logDir, o.cfg.StateLabels.Stopped)
    o.gh.Comment(ctx, n, "⏸ Stopped by user. Worktree, logs and session are "+
        "preserved. Press Continue to resume.")
    // Deliberately does NOT touch the worktree, branch, logs, or session file,
    // and does NOT record a resumable park cause.
}
```

Because it swaps `ai-wip → ai-stopped` and records no park cause, no auto-resume
path will act on it.

### 5. `Continue(n)` — Orchestrator method (deferred resume)

```go
func (o *Orchestrator) Continue(ctx context.Context, n int) error {
    o.mu.Lock()
    _, running := o.active[n]
    o.mu.Unlock()
    if running {
        return errAlreadyRunning
    }
    logDir := issueLogDir(n)
    si, _ := readSession(logDir)
    if si.SessionID != "" {
        // Resume from the persisted session: hand to the ResumeParked path.
        o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.Stopped, o.cfg.StateLabels.Rework)
        recordState(logDir, o.cfg.StateLabels.Rework)
        recordParkCause(logDir, interruptedCause)   // resumable cause
    } else {
        // No session yet: restart from scratch. Re-queue as eligible.
        o.gh.RemoveLabel(ctx, n, o.cfg.StateLabels.Stopped)
        clearState(logDir)
        clearParkCause(logDir)
        // Worktree is left in place (reused per CLAUDE.md); the fresh pipeline
        // run opens a new session, so behavior is "from scratch".
    }
    return nil
}
```

Continue only rewrites labels/state. The **next `runLoop` cycle** picks it up
when a slot is free:

- **session present** → `ai-rework` → `ResumeParked` → `Rework` resumes from the
  session id (`shouldResume` passes: resumable cause + worktree + session
  exist, no backoff entry).
- **no session** → eligible → `ProcessOnce` runs a fresh pipeline from the top.

Either way it respects `TicketsPerCycle` and defers to a free slot, satisfying
decision 5. Continue is label-driven, so it works even after a daemon restart
(the maps are empty but the session file on disk is the source of truth).

### 6. HTTP endpoints and Server→Orchestrator link

- `NewServer` gains an `*Orchestrator` parameter; `main.go` passes the same `o`
  it drives. Add an `orch *Orchestrator` field to `Server`.
- Add two routes to `Handler` (Go 1.22 method-prefixed patterns):
  - `POST /stop`   → `handleStop`
  - `POST /continue` → `handleContinue`
- Handlers parse the issue number from the form/query, call
  `orch.Stop(n)` / `orch.Continue(r.Context(), n)`, and respond with the
  refreshed detail fragment (same rendering path as `handleDetail`) so htmx
  swaps the pane in place. `errNotRunning` / `errAlreadyRunning` render an
  inline, non-fatal message rather than an HTTP error (the ticket may have
  finished between render and click).

The synchronous GitHub calls in `Continue` and in the handlers use the request
context; the asynchronous `pause` uses the daemon's parent ctx.

### 7. UI

- **Detail pane** (`detail.html`): in the header, render a **Stop** button when
  `stateKind == "wip"` and a **Continue** button when `stateKind == "stopped"`.
  Each is an htmx control:
  - Stop: `hx-post="/stop?issue={{.Number}}" hx-confirm="Stop this ticket? The current turn's work will be lost." hx-target="#main" hx-swap="innerHTML"`.
  - Continue: `hx-post="/continue?issue={{.Number}}" hx-confirm="Continue this ticket?" hx-target="#main" hx-swap="innerHTML"`.
  `hx-confirm` provides the required confirmation step (decision 6) with no
  custom JS.
- **Rail** (`rail.html`) and **render.go**: add a `"stopped"` case to
  `stateKind` (label == `cfg.StateLabels.Stopped`) and a stripe/chip color to
  `stripeClass` (e.g. a muted/paused tone), plus any chip-copy mapping the
  templates use, so stopped tickets read distinctly in the list.

## Concurrency, races, and restart behavior

- **Stop while between turns / mid git-op:** the child ctx cancels; the next
  `claude` call or git command returns a ctx error and the pipeline unwinds to
  `pause`. Fine.
- **Stop landing during `ship`:** `handleIssue` checks `isStopping` *before* the
  outcome switch, so a Stop observed before shipping cleanly pauses. A Stop that
  lands *mid-`ship`* (after the check) may interrupt a push/PR; this is the same
  "lose in-progress work" tradeoff (decision 7) and is recoverable via Continue
  → `Rework`. Narrow and accepted.
- **Daemon crash in the Stop→pause window:** the ticket is briefly still
  `ai-wip` with a live worktree+session, so `SweepOrphans` would park it to
  `ai-rework` and auto-resume — pre-existing orphan behavior, tiny window,
  accepted.
- **Double Stop / Stop after completion:** second call finds no entry in
  `cancels`/`active` → `errNotRunning` → inline message. No-op.
- **Restart while stopped:** `ai-stopped` is queried by none of the resume
  paths, so the ticket stays put until Continue.

## Testing (TDD)

Following the project's existing table/fake-runner + fake-gh patterns:

- **`Stop`**: running ticket → `cancel` invoked and `stopping[n]` set;
  not-running ticket → `errNotRunning`.
- **`pause`**: transitions `ai-wip → ai-stopped`, records stopped state, leaves
  worktree/branch/logs/session intact, records no park cause.
- **`handleIssue` stop path**: when `stopping[n]` is set, the normal
  park/ship/finish outcome is skipped.
- **`Continue` with session**: `ai-stopped → ai-rework` + resumable park cause;
  a following `ResumeParked` acquires a slot and resumes via `Rework`.
- **`Continue` without session**: `ai-stopped` removed, state cleared, and
  `ProcessOnce` re-picks it as eligible.
- **`Continue` while running**: `errAlreadyRunning`.
- **`hasStateLabel`**: an issue carrying `ai-stopped` is excluded from
  `ListEligibleIssues` (no re-pickup).
- **HTTP**: `POST /stop` and `POST /continue` route correctly, call the
  orchestrator, and render the refreshed detail fragment; sentinel errors render
  an inline message, not a 5xx.
- **Render**: `stateKind` maps `ai-stopped → "stopped"`; Stop button appears for
  wip, Continue button for stopped.

## Out of scope

- Stopping queued/eligible or already-parked tickets.
- Graceful "finish the current turn then pause" (decision 7 accepts a hard kill).
- Authentication on the endpoints (unchanged; dashboard stays bound to
  `localhost:8080`, confirmation dialog is the only guard).
