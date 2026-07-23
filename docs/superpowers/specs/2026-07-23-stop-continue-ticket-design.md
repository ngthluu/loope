# Stop / continue a ticket — design

Issue: [#3](https://github.com/ngthluu/loope/issues/3) — *Enhance: add a feature
to Stop/Continue a ticket (in both CLI and UI)*

## Goal

Give the operator manual control over a ticket's lifecycle:

- **Stop** — halt work on a ticket at any lifecycle stage and park it in a
  durable state the daemon will never leave on its own. All progress is
  preserved: worktree, branch, logs, and the Claude session id.
- **Continue** — take a stopped ticket and resume it from the persisted Claude
  session id, then ship it exactly as the existing rework path does.

Both verbs are available from the CLI and from the dashboard.

## Background: what already exists

| Piece | Where | Relevance |
|---|---|---|
| Session id persisted per issue | `Claude.RecordSession` → `logs/issue-<N>/session` | What continue resumes from |
| Resume-and-ship a preserved worktree | `Orchestrator.Rework` (`rework.go`) | Continue is a re-use of this |
| Auto-resume of transient parks | `Orchestrator.ResumeParked` (`loop.go`) | Must **not** touch stopped tickets |
| Orphan recovery after a crash | `Orchestrator.SweepOrphans` (`loop.go`) | Must respect a stop marker |
| Read-only dashboard | `serve.go`, `GET /`, `/rail`, `/detail` | Gains two POST routes |
| Pipelines as goroutines, `claude` via `exec.CommandContext` | `loop.go`, `runner.go` | Cancelling a per-issue context is how a run is halted |

Continue is therefore mostly a re-use of existing machinery. Stop is the new
part, and it needs one bug fixed to be correct (see
[Live session capture](#live-session-capture)).

## Design decisions

These were settled during brainstorming; recorded here so the implementation
does not relitigate them.

1. **Stopped is its own durable state**, not a reuse of `ai-rework`. Parking a
   stopped ticket as `ai-rework` would let `ResumeParked` auto-resume it minutes
   later, so the daemon could undo the operator's stop. Only an explicit
   continue moves a ticket out of stopped.
2. **Stop applies at any lifecycle stage** — a running `ai-wip` ticket, a queued
   `ai-agent` ticket the loop hasn't picked up, or a parked `ai-rework` one.
   Semantically: *keep the loop's hands off this ticket*.
3. **CLI uses flags, not subcommands.** The existing CLI is entirely flag-based
   (`-once`, `-serve`, `-rework <N>`) with no subcommand parsing. Adding
   `-stop <N>` / `-continue <N>` matches it; introducing a subcommand parser for
   two verbs would fork the CLI's shape for no gain.
4. **Stop is durable on disk, not just in memory.** A stop request is written to
   `logs/issue-<N>/stop` before anything else happens. This is what makes
   `loope -stop <N>` in a second shell able to halt a run owned by a daemon in
   another process, and what makes a stop survive a daemon restart.
5. **Nothing is deleted on stop.** Per `CLAUDE.md`: continue builds on what
   exists. Worktree, branch, logs, and session file are all left intact.

## Architecture

### New state: `ai-stopped`

`StateLabels` gains a `Stopped` field (JSON key `stopped`), defaulting to
`ai-stopped` via a new `labelStopped` constant next to the existing ones in
`config.go`.

The label must be threaded through every place that enumerates state labels:

| Function | File | Change |
|---|---|---|
| `defaultStateLabels` | `config.go` | Add `Stopped: labelStopped` |
| `GitHub.hasStateLabel` | `github.go` | Add `g.state.Stopped` — a stopped issue leaves the eligible queue |
| `trackedStateLabels` | `tracker.go` | Add `cfg.StateLabels.Stopped` so the dashboard fetches stopped issues |
| `pickStateLabel` | `tracker.go` | Insert `Stopped` in priority order: WIP > Stopped > Rework > Done > eligible |
| `stateKind` | `serve.go` | Map `Stopped` → `"stopped"` |
| `stripeClass` | `serve.go` | `"stopped"` → `bg-muted/40` (a neutral grey stripe, visually distinct from the amber rework and the live teal wip) |

Rail and detail badges get a `stopped` case alongside the existing
`done`/`wip`/`rework`/`failed` cases, styled with the neutral `border-line2
bg-panel2 text-muted` treatment already used for unknown states, plus the label
text `stopped`.

The label lifecycle table in `README.md` gains a row, and the `gh label create`
block gains `ai-stopped`.

### Stop marker file

New file `logs/issue-<N>/stop`, managed in `tracker.go` next to the existing
`state` and `park-cause` markers, following exactly their best-effort shape:

```go
const stopFile = "stop"

func recordStopRequest(logDir string)      // write RFC3339 timestamp
func stopRequested(logDir string) bool     // marker exists
func clearStopRequest(logDir string)       // remove marker
```

The file's content is a timestamp for human postmortems; only its **existence**
is load-bearing.

The marker has three jobs:

1. It tells a running pipeline that its context cancellation was a *stop*, not a
   daemon shutdown, so it finishes as stopped rather than parked.
2. It lets a stop issued from another process reach a pipeline this process
   owns.
3. It survives a daemon restart, so `SweepOrphans` recovers a stopped-but-crashed
   ticket into stopped rather than parking it for auto-resume.

### Live-run registry

New file `control.go` holds the in-process registry of live pipelines:

```go
// runRegistry tracks the cancel func of every pipeline running in this process,
// keyed by issue number, so a stop request can halt one immediately.
type runRegistry struct {
    mu   sync.Mutex
    live map[int]context.CancelFunc
}

func (r *runRegistry) register(n int, cancel context.CancelFunc) (ok bool)
func (r *runRegistry) deregister(n int)
func (r *runRegistry) cancel(n int) (found bool)
func (r *runRegistry) running(n int) bool
```

`register` returns false when the issue is already registered — this is what
makes continue refuse to start a second pipeline on the same worktree.

The registry is a field on `Orchestrator`, alongside the existing `resumeBackoff`
bookkeeping.

Both pipeline entry points wrap their context and register:

- `handleIssue` (`loop.go`): derive `ictx, cancel := context.WithCancel(ctx)`,
  `register(n, cancel)`, `defer deregister(n)`, and pass `ictx` to everything
  from `Worktree.Create` through the pipeline run.
- `resume` (the shared body of rework/continue, see below): the same wrapping.

### Stop watcher

A goroutine started by `main` for long-running modes (the same condition that
takes the workDir lock) ticks every 2 seconds:

```go
// watchStops cancels any locally running pipeline whose stop marker has
// appeared. It is what lets `loope -stop <N>` in another shell halt a run this
// daemon owns: that process can only write the marker file, not reach into this
// process's goroutines.
func (o *Orchestrator) watchStops(ctx context.Context)
```

It iterates only over registered issue numbers, so a quiet daemon does one
`os.Stat` per live pipeline every 2 seconds and nothing else.

### Graceful termination

`exec.CommandContext` kills with `SIGKILL` on cancellation, which gives `claude`
no chance to flush its session transcript. `execRunner.Run` and
`execRunner.RunStream` set:

```go
cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
cmd.WaitDelay = 10 * time.Second
```

so a stop sends `SIGTERM` first and escalates to `SIGKILL` only if the process
has not exited after 10 seconds. This is a strict improvement for the existing
Ctrl-C path too.

### Live session capture

**The bug this fixes:** `Claude.RecordSession` is only called by pipeline code
*after* `Claude.Call` returns. A session killed mid-call therefore never
persists its session id, so continue would resume the *previous* step's session —
or, on the very first call, find no session at all and refuse. Stop/continue is
not correct without this.

The id is available live: `claude --output-format stream-json` emits
`{"type":"system","subtype":"init","session_id":"..."}` as its first event, and
`parseTranscript` in `tracker.go` already reads `session_id` off stream events.

`ClaudeCall` gains a `Kind` field. When it is non-empty, `Call` tees its stream
sink through a sniffer that scans arriving lines for the first `session_id` and
calls `c.RecordSession(id, call.Kind)` the moment it appears:

```go
// sessionSniffer wraps the stream sink, watching arriving stream-json lines for
// the first session_id and persisting it immediately. Without this, a session
// killed mid-call (a stop, a crash) would leave `session` pointing at the
// previous step, and continue would resume the wrong session or none at all.
type sessionSniffer struct {
    w    io.Writer
    buf  []byte  // partial trailing line
    done bool
    on   func(id string)
}
```

It writes through unconditionally and never fails a call: a sniffing error is
dropped, exactly as the other log-writers are best-effort.

Callers that already record a session (`pipeline_bug.go`, `pipeline_feature.go`,
`rework.go`) set `Kind` on the call and keep their existing post-return
`RecordSession` — the post-return call is now a no-op-equivalent overwrite with
the same id, and it still carries the authoritative kind. The ephemeral answerer
call leaves `Kind` empty and so still never touches the session file.

## Behaviour

### Stop

`Orchestrator.Stop(ctx, n) error`, in `control.go`.

It always records the stop marker first, then acts by decision table:

| Condition | Action | Reported to the user |
|---|---|---|
| Registered in this process | `registry.cancel(n)`. The pipeline goroutine performs the labeling as it unwinds. | `stopping #N (halting the running session)` |
| Not local, current state is WIP, and a live daemon holds the workDir lock | Marker only. The daemon's watcher cancels within ~2s and labels. | `stop requested for #N — the running daemon will halt it shortly` |
| Anything else (queued, `ai-rework`, or WIP with no live daemon) | `finishStopped` directly | `stopped #N` |

Liveness of the daemon is read from `logs/daemon.lock` using the existing
`pidAlive` helper — `daemon.go` gets a small `lockOwnerAlive(workDir) bool`
extracted from `acquireLock`'s existing read-and-check logic.

Stopping an already-stopped ticket is a no-op success (idempotent). Stopping an
`ai-done` or `ai-needs-info` ticket is an error: there is nothing to stop.

`finishStopped(ctx, n int, fromLabel string) error`:

- Uses `context.WithoutCancel(ctx)` — the pipeline path calls it with an already
  cancelled context.
- Comments on the issue: `🤖 Stopped by request. Progress is preserved —
  continue with ` + "`loope -continue N`" + ` or the dashboard.`
- `SwapLabels(fromLabel → Stopped)`, or `AddLabel(Stopped)` when the ticket had
  no state label (the queued case).
- `recordState(logDir, Stopped)` and `clearParkCause(logDir)` — a stopped ticket
  must carry no park cause, so `ResumeParked` cannot see it as resumable even if
  its label were somehow mis-set.
- **Leaves the stop marker in place.** The marker is cleared by continue, not by
  the stop completing; it is the durable record that this ticket is
  operator-held.
- Does **not** remove the worktree or delete the branch.

### The pipeline unwinding after a cancel

`handleIssue`'s error handling gains one branch, checked *before* the existing
park:

```go
if perr != nil {
    if stopRequested(o.issueLogDir(n)) {
        return o.finishStopped(ctx, n, o.cfg.StateLabels.WIP)
    }
    return o.park(ctx, n, o.cfg.StateLabels.WIP, perr)
}
```

This is what distinguishes a stop from a daemon shutdown: both cancel the
context and surface as a `claude` error, but only a stop leaves a marker. A
`SIGTERM` to the daemon still parks its in-flight issues as before.

`resume` gets the identical branch.

Once `ship` has begun, a stop no longer applies — the pipeline is done and the
PR is going up. This is deliberate: interrupting between push and PR-create
leaves the messiest state, and the window is seconds.

### Daemon paths that must ignore stopped tickets

- **Eligibility** — handled by `hasStateLabel` including `Stopped`.
- **`ResumeParked`** — scans `ai-rework` issues only, so a stopped ticket is
  already invisible to it. `shouldResume` additionally returns false when
  `stopRequested(logDir)`, as belt-and-braces for a ticket whose labels are
  inconsistent.
- **`SweepOrphans`** — for each stale `ai-wip` issue, check the stop marker
  first: if present, the run was stopped and the daemon then died, so
  `finishStopped(WIP)` rather than the existing park-for-resume. The rest of the
  sweep is unchanged.

### Continue

`Orchestrator.Continue(ctx, n) error`, in `control.go`. Two shapes, by what
survived:

**Case 1 — preserved worktree and a saved session id exist.** The real resume:

1. Refuse if `registry.running(n)` — `#N is already running`.
2. `clearStopRequest(logDir)`.
3. Swap the current state label → `WIP`. The ticket is genuinely working again,
   and this makes the dashboard show it live, `SweepOrphans` recover it if the
   daemon dies mid-continue, and the labels tell the truth.
4. Run the shared resume body with `fromLabel = WIP`, which resumes the
   persisted session id with the existing `reworkPrompt`, then ships (WIP →
   Done) or parks (WIP → Rework) exactly as today.

**Case 2 — no preserved worktree or no saved session** (the ticket was stopped
while queued, before any work started). There is nothing to resume, so continue
means *re-queue*: `clearStopRequest`, `RemoveLabel(Stopped)`, `clearState`. The
eligible label alone remains, and the next poll cycle picks the issue up from
scratch through triage.

Continuing a ticket that is not stopped is an error.

### Refactor: one resume body

`rework.go`'s `Orchestrator.Rework` is split so continue and rework share the
resume-and-ship logic without duplicating it:

```go
// resume resumes issue n's persisted Claude session in its preserved worktree,
// then ships. fromLabel is the state label the issue currently carries, which
// ship swaps to Done and park swaps to Rework.
func (o *Orchestrator) resume(ctx context.Context, n int, fromLabel string) error

// Rework resumes a parked (ai-rework) issue and ships it. Unchanged behaviour:
// the entry point for `-rework` and for ResumeParked's auto-resume.
func (o *Orchestrator) Rework(ctx context.Context, n int) error {
    return o.resume(ctx, n, o.cfg.StateLabels.Rework)
}
```

`resume` carries the registry wrapping, the stop-marker branch, and the existing
"record the session before the error check" behaviour. `Rework`'s observable
behaviour — including its error messages about a missing worktree or session —
is unchanged, so `ResumeParked` and the existing tests are untouched.

## CLI

Two new flags in `main.go`, handled next to the existing `-rework` block, before
the lock is taken (they are one-shot commands that exit):

```
-stop <N>       stop work on issue N, preserving all progress, then exit
-continue <N>   continue a stopped issue N from its persisted Claude session, then exit
```

- `-stop` is safe to run against a live daemon: it writes the marker and returns
  promptly, printing which of the three decision-table paths it took. It does
  not wait for the running session to die.
- `-continue` runs the resume **synchronously** and exits when the ticket ships
  or parks, matching `-rework`.
- `-continue` refuses when a live daemon holds the lock **and** the issue's
  current state is WIP, since that would put two `claude` sessions in one
  worktree. (A stopped ticket is by definition not running, so the normal case
  is unaffected. This is the same hazard `-rework` already carries; the new flag
  guards against it rather than inheriting it.)
- `-rework <N>` is retained and unchanged.

## Dashboard

The dashboard stops being strictly read-only. This is the one place the design
widens the blast radius, so it is scoped deliberately:

- Two routes, both `POST` only: `POST /stop` and `POST /continue`, each taking
  `?issue=<N>`. `GET` on them is not registered, so a link or a crawler cannot
  trigger either.
- Actions are wired through a narrow interface, so `serve.go` never learns about
  pipelines:

  ```go
  // Controller is the mutating surface the dashboard exposes. Orchestrator
  // implements it. A nil Controller (a dashboard with no daemon behind it)
  // hides the buttons and makes the routes return 503.
  type Controller interface {
      Stop(n int) error
      Continue(n int) error
  }
  ```

- `NewServer(r, cfg, ctl)` gains the controller parameter; existing tests pass
  `nil`.
- **`Continue` must not block the HTTP request** — a resume is a multi-minute
  Claude session. `Orchestrator.Continue(n)` (the `Controller` method) validates
  synchronously (stopped? not already running? which case?) so the UI can report
  a real error, then runs the resume in a goroutine. The goroutine's context is
  the daemon's, not the request's: `Orchestrator` gains a `baseCtx` field set by
  `main` from the signal context, so a continue survives the HTTP response and
  dies with the daemon. `Controller.Stop` is fast and runs inline.
- Both handlers return `204 No Content` on success, or a `4xx`/`5xx` with a
  plain-text reason (`#3 is already running`, `#3 is not stopped`).

UI, in the detail-pane header next to the existing `issue ↗` / `pull request ↗`
chips:

- A **stop** button, shown when `stateKind` is `wip`, `rework`, or `queued`.
- A **continue** button, shown when `stateKind` is `stopped`.
- Neither is shown when the controller is nil, nor for `done` / `needs-info`.
- A small `onclick` handler posts, then calls the existing `poll()` so the rail
  and detail refresh immediately; a non-2xx response surfaces its body via a
  brief inline error line beneath the header rather than an `alert`.

Because the dashboard can now change state, `README.md` gains an explicit note
that `-addr` should stay bound to `localhost` (as it defaults to): the endpoints
are unauthenticated, exactly like the rest of the dashboard, and are now
mutating.

## Testing

Following the existing table-driven style with the fake runner in
`helpers_test.go`.

**`control_test.go`** (new):

- Stop of a locally-registered run cancels its context and the pipeline finishes
  as stopped: worktree still on disk, `session` file intact, state file reads
  `ai-stopped`, the `gh` calls show a WIP→stopped swap and no park comment.
- Stop of a queued ticket (no state label) adds `ai-stopped` and writes the
  marker; no worktree is touched.
- Stop of a parked `ai-rework` ticket swaps rework→stopped and clears the park
  cause.
- Stop is idempotent: stopping a stopped ticket is a no-op success.
- Stop of a done ticket errors.
- `runRegistry` register/cancel/deregister, including the double-register
  refusal.
- `watchStops` cancels a registered run when the marker appears out of band
  (simulating a second process), using an injectable tick.
- Continue, case 1: with a worktree and session on disk, it clears the marker,
  swaps stopped→WIP, calls `claude --resume <persisted id>`, and ships (WIP →
  Done).
- Continue, case 2: with no worktree, it removes the stopped label, clears the
  state file, and makes no `claude` call.
- Continue refuses a running ticket and a non-stopped ticket.

**`claude_test.go`** (extend):

- A call with `Kind` set writes the `session` file as soon as the stream's
  `init` event arrives, **before** the call returns — asserted by having the
  fake runner check the file mid-stream.
- A call with `Kind` empty never writes the `session` file (the answerer case).
- A malformed stream line does not fail the call.

**`loop_test.go`** (extend):

- A pipeline error with a stop marker present finishes as stopped; the same
  error without the marker still parks as `ai-rework` (guards against a stop
  branch swallowing genuine failures).
- `SweepOrphans` on a stale WIP issue with a stop marker finishes it as stopped
  instead of parking it for resume.
- `shouldResume` returns false when a stop marker is present.

**`serve_test.go`** (extend):

- `POST /stop` and `POST /continue` call the controller with the parsed issue
  number and return 204; a controller error becomes a 4xx with its message in
  the body.
- `GET /stop` is a 405 (not registered).
- With a nil controller the routes return 503 and the buttons are absent from
  the rendered detail pane.
- The stop button renders for wip/rework/queued and the continue button for
  stopped, and neither renders for done.

**`config_test.go`** (extend): `stopped` defaults to `ai-stopped` and is
overridable; `hasStateLabel` treats it as a state label.

## Out of scope

- A continue button on parked `ai-rework` tickets (i.e. driving `-rework` from
  the UI). It is nearly free once the plumbing exists, but it is a different
  verb from the one this issue asks for, and `ResumeParked` already handles the
  transient cases automatically.
- Stopping the daemon itself, or a fleet-wide pause. Ctrl-C already stops the
  daemon.
- Authentication for the dashboard's mutating routes; it stays localhost-bound
  and unauthenticated like the rest of the dashboard.
- Any change to `ResumeParked`'s backoff behaviour or to the `-rework` flag.
