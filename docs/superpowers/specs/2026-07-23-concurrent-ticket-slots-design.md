# Keep ticket slots full: `ticketsPerCycle` as a concurrency budget

Issue: [#6](https://github.com/ngthluu/loope/issues/6)
Date: 2026-07-23

## Problem

With `ticketsPerCycle: 3`, labelling two issues `ai-agent` starts two
pipelines. Labelling a third issue while those two run does **not** start it —
it stays unlabelled by the loop until both running pipelines finish. The third
slot sits idle for the whole duration of the first two.

The cause is in `Orchestrator.ProcessOnce` (`loop.go`). A cycle is a *blocking
batch*:

1. list eligible issues,
2. triage up to `TicketsPerCycle` of them,
3. launch one goroutine per pick,
4. `wg.Wait()` for **all** of them,
5. return.

`runLoop` (`main.go`) calls `ProcessOnce` synchronously, so no polling happens
at all while a batch runs. `ticketsPerCycle` therefore means "size of a
serialized batch", not "how much work may be in flight".

## Goal

`ticketsPerCycle` becomes a **concurrency budget**. Every poll cycle tops the
in-flight set back up to the budget from whatever is eligible right now.
Pipelines started in different cycles run side by side; a free slot is filled
within one poll interval of an issue becoming eligible.

## Decisions

Settled with the issue author before design:

1. **Reinterpret `ticketsPerCycle` in place** as "max concurrent pipelines". No
   new config key, no migration. Existing configs keep working; with the
   default of `1` the behavior is unchanged.
2. **The in-process slot counter is authoritative.** Slots are not derived from
   counting `ai-wip` issues on GitHub — the daemon holds an exclusive workDir
   lock, so no other instance can own live pipelines, and the label can lag or
   fail to apply.
3. **Auto-resumes share the same budget.** `ResumeParked` draws from the same
   slots and runs its resumes concurrently with new work, instead of blocking
   the cycle sequentially.
4. **`-once` fills slots once, waits for them to drain, then exits.** It does
   not top up as pipelines complete.
5. **Pipelines log their own outcome.** `ProcessOnce` and `ResumeParked` return
   only listing/selection errors, since pipeline errors now arrive after the
   cycle that started them has returned.

Explicitly **not** doing: the batch-hash idea from the issue. A hash would be
new state to persist and reconcile across restarts, and it identifies nothing
the live `active` set and the `ai-wip` label don't already identify.

## Design

### Slot ledger

New state on `Orchestrator`, guarded by the existing `mu`:

```go
active map[int]struct{} // issue numbers with a pipeline in flight
inFlight sync.WaitGroup // every acquired slot adds one
```

Four helpers, all taking `mu`:

- `slots() int` — the effective budget: `cfg.TicketsPerCycle`, clamped to a
  minimum of 1 (matching today's `selectIssues` clamp).
- `tryAcquire(n int) bool` — returns false if `n` is already in `active`, or if
  `len(active)` has reached the budget. Otherwise inserts `n`, calls
  `inFlight.Add(1)`, returns true.
- `release(n int)` — deletes `n` from `active` and calls `inFlight.Done()`.
- `freeSlots() int` — `slots() - len(active)`, floored at 0.

The **already-in-flight** check in `tryAcquire` is not redundant with the label
check. It closes two real windows:

- Between launching a pipeline and its `AddLabel(ai-wip)` landing, the issue
  still looks eligible to `ListEligibleIssues`, so the next cycle could pick it
  twice.
- `park` swaps `ai-wip` → `ai-rework` *before* the pipeline goroutine returns,
  so `ResumeParked` in the same cycle could try to resume an issue whose
  goroutine still holds its worktree.

`active` is keyed by issue number and shared by both the new-work path and the
resume path, so one ledger covers both.

### `ProcessOnce`

```
free := freeSlots()
if free == 0 { return nil }          // no gh call at all
issues := gh.ListEligibleIssues(...)
issues = filter(issues, not in active)
if len(issues) == 0 { return nil }
picks, selErr := selectIssues(ctx, issues, free)
if len(picks) == 0 { return selErr }
base, err := wt.DefaultBranch(ctx)   // join with selErr on failure
for each pick p:
    if !tryAcquire(p.issue.Number) { continue }
    go run(p, base)
return selErr
```

`selectIssues` takes the limit as a parameter instead of reading
`cfg.TicketsPerCycle`, so the caller controls how many picks a cycle asks for.
Its internals (repeated single-pick `Triage`, removing each chosen issue from
the candidate set, returning partial picks alongside a triage error) are
unchanged.

The per-pipeline goroutine keeps today's panic guard and gains slot release and
outcome logging:

```go
go func(p pick) {
    defer o.release(p.issue.Number)
    defer func() {
        if r := recover(); r != nil {
            log.Printf("issue #%d: pipeline panic: %v\n%s", ...)
            _ = o.park(ctx, p.issue.Number, o.cfg.StateLabels.WIP, fmt.Errorf("panic: %v", r))
        }
    }()
    log.Printf("issue #%d (%s): %s", p.issue.Number, p.kind, p.reason)
    if err := o.handleIssue(ctx, p.issue, p.kind, base); err != nil {
        log.Printf("issue #%d: pipeline failed: %v", p.issue.Number, err)
    }
}(picks[i])
```

`defer o.release(...)` is registered first so it runs **last** — after the
recover handler has parked the issue. A panicking pipeline therefore always
returns its slot.

Acquisition cannot fail for capacity reasons in practice: `ProcessOnce` and
`ResumeParked` are called from one goroutine in `runLoop`, and the only
concurrent mutation is completions, which *free* slots. `tryAcquire` is still
checked rather than assumed — it is the single place the invariant lives.

Note that `handleIssue`, `park`, `ship`, `finishDone`, `finishNeedsInfo`, and
`abort` are all unchanged. The change is entirely in who waits for them.

### `ResumeParked`

Same shape as `ProcessOnce`:

```
if freeSlots() == 0 { return nil }
issues := gh.ListIssuesWithLabel(ctx, Rework)
for each is:
    if ctx.Err() != nil { break }
    if freeSlots() == 0 { break }
    if !shouldResume(is.Number) { continue }
    if !tryAcquire(is.Number) { continue }
    go resume(is.Number)
return nil                            // only the listing error propagates
```

The resume goroutine mirrors the pipeline one:

```go
go func(n int) {
    defer o.release(n)
    defer func() {                    // same recover+park shape as pipelines
        if r := recover(); r != nil {
            log.Printf("issue #%d: resume panic: %v\n%s", n, r, debug.Stack())
            _ = o.park(ctx, n, o.cfg.StateLabels.Rework, fmt.Errorf("panic: %v", r))
        }
    }()
    log.Printf("issue #%d: auto-resuming parked work", n)
    if err := o.Rework(ctx, n); err != nil {
        log.Printf("auto-resume #%d failed: %v", n, err)
        o.noteResumeFailure(n)
        return
    }
    o.clearResumeState(n)
}(is.Number)
```

`shouldResume` (park cause classification, worktree/session presence, backoff
window, once-per-process skip logging) is unchanged. `noteResumeFailure` and
`clearResumeState` already take `mu`, so calling them from the resume goroutine
is safe; they must not be called while `mu` is held elsewhere.

Ordering within a cycle stays as it is today: `ProcessOnce` runs first and gets
first claim on free slots, then `ResumeParked` uses what's left. New work is
preferred over resumes when the budget is tight; resumes have backoff and will
come back.

### `runLoop` and shutdown

Today `ProcessOnce` blocks, so the workDir lock is only released after all work
finishes. That property must be preserved explicitly:

- On `ctx.Done()`, `runLoop` calls `o.Wait()` (a thin wrapper over
  `inFlight.Wait()`) before returning, so `defer release()` in `main` runs after
  pipelines have unwound. Pipelines see the cancelled context and finish through
  their existing `context.WithoutCancel` cleanup paths — the same unwinding that
  happens today when Ctrl-C lands during `wg.Wait()`.
- In `-once` mode, `runLoop` calls `o.Wait()` before returning. One cycle fills
  slots, the loop exits, no top-up occurs.

Nothing else in `runLoop` changes: the startup orphan sweep still runs before
anything is in flight, and `guard` still converts a panic in the cycle body into
a logged error.

### Unchanged

- `SweepOrphans`, `Rework`, `handleIssue` and every terminal path
  (`ship`/`park`/`finishDone`/`finishNeedsInfo`/`abort`).
- The dashboard (`serve.go`), which reads state from `logs/issue-<N>/` on disk
  and is unaffected by when pipelines are launched.
- Label semantics. `ai-wip` remains the durable marker of an in-flight run and
  is what makes `SweepOrphans` work after a crash.

## Error handling

| Failure | Handling |
|---|---|
| `ListEligibleIssues` / `ListIssuesWithLabel` fails | returned from `ProcessOnce` / `ResumeParked`, logged by `runLoop`, retried next cycle |
| `Triage` fails mid-selection | unchanged: partial picks are launched, the error is returned alongside them |
| `DefaultBranch` fails | joined with the selection error and returned; no pipelines launched, no slots consumed |
| Pipeline returns an error | already handled internally (`park` / `abort`); now additionally logged at goroutine completion |
| Pipeline panics | recovered in the goroutine, issue parked with the panic as a non-resumable cause, slot released |
| Resume fails | logged, `noteResumeFailure` doubles the backoff, slot released |

A crash while pipelines are in flight is unchanged from today: the issues stay
`ai-wip` on GitHub and the next startup's `SweepOrphans` parks them for resume
(worktree + session intact) or re-queues them.

## Testing

New tests:

- **Slot ledger unit tests** — `tryAcquire` refuses past the budget; refuses an
  issue already in `active`; `release` frees exactly one slot; `freeSlots`
  never goes negative; budget below 1 clamps to 1.
- **`ProcessOnce` returns before pipelines finish** — a fake pipeline that
  blocks on a channel; assert `ProcessOnce` returns while it is still blocked,
  then unblock and `Wait()`.
- **Top-up across cycles (the issue's scenario)** — budget 3, two blocked
  pipelines in flight; a second `ProcessOnce` with a third eligible issue picks
  exactly that one and starts it, without waiting for the first two.
- **Budget is respected** — budget 2, three eligible issues; only two pipelines
  start, the third waits for a completion.
- **Full budget short-circuits** — with zero free slots, `ProcessOnce` makes no
  `gh issue list` call at all (assert against the fake runner's recorded
  commands).
- **In-flight issues are filtered** — a stale `ListEligibleIssues` result that
  still includes an in-flight issue does not start a second pipeline for it.
- **`ResumeParked` shares the budget** — with the budget consumed by new work,
  no resume starts; with a free slot, one does, and an issue still in `active`
  is not resumed.
- **Drain on exit** — `runLoop` in `-once` mode does not return until in-flight
  pipelines complete.

Existing tests to migrate: the `ProcessOnce*` tests in `loop_test.go` that
assert on the returned error from a failing pipeline (e.g.
`TestProcessOnceFailurePathParksForRework`,
`TestProcessOnceRecordsLocalStateRework`, the panic test). Pipeline errors are
no longer returned, so each becomes `ProcessOnce(...)` followed by `o.Wait()`
and assertions on observable state — recorded label swaps, issue comments, and
the park cause file. Most already assert exactly that; only the `err == nil`
checks and the missing `Wait()` change. `TestProcessOnceHandlesMultipleTickets`
keeps its meaning with a `Wait()` added.

## Docs

- `README.md`: rewrite the `ticketsPerCycle` row — "maximum number of pipelines
  running concurrently. Each poll cycle tops the in-flight set back up to this
  limit from the eligible queue, so a newly labelled issue starts within one
  poll interval whenever a slot is free. Auto-resumes of parked issues draw from
  the same limit. Values below 1 are treated as 1."
- `README.md` "How it works": note that a poll cycle no longer waits for
  running pipelines.
