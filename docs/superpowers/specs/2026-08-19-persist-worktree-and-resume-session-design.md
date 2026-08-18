# Persist worktrees and Claude sessions; resume instead of restarting

Issue: #53

## Problem

Every pipeline stage (`brainstorm-N`, `plan`, `execute`, `debug`) shells out to
a fresh `claude` subprocess (`claude.go:103-158`). When a stage fails — a Claude
API error, a session/usage-limit park, a human asking for rework, a human
answering an `ai-needs-info` question, a user-initiated Stop/Continue, or a
daemon restart — the next attempt re-enters `handleIssue` and calls
`RunFeaturePipeline`/`RunBugPipeline` with `Resume: ""`, i.e. **from
`brainstorm-0`/`debug` again**, discarding the architect's prior reasoning even
though `<logDir>/session` already has a resumable session id sitting on disk
unused (`claude.go:233-235`).

On top of that, three outcomes actively delete the worktree and branch that the
next attempt would otherwise reuse: `finishDone` (`loop.go:281,284`),
`finishNeedsInfo` (`loop.go:306,309`), and `ship`'s success path
(`loop.go:496,501`). `park`/`abort`/`pause`/`SweepOrphans` already never delete
— they're the template this brings the other three in line with.

## Goal

Every re-entry into a ticket's pipeline — API error, `ai-rework` removed,
`ai-needs-info` answered, dashboard Continue, or daemon restart — resumes the
**same Claude session** with a `--resume` + a short prompt, instead of
restarting the pipeline from its first stage. Worktrees and branches are never
deleted by the daemon, ever, so there is always something to resume into.

## Decisions (from the issue author)

1. No stuck-process detection. The only "error" resume trigger is a pipeline
   step that already failed and returned to the daemon (park/abort), not a
   still-running process being killed. "Continue" is user- or label-triggered,
   never a timeout.
2. No automatic retry loop. The daemon still parks on failure and waits for a
   human (label removal or dashboard Continue) exactly as today — only *what
   happens after that trigger* changes, from "rerun from scratch" to "resume
   the session."
3. `finishDone`, `finishNeedsInfo`, and `ship` all stop deleting the
   worktree/branch, with no exceptions.
4. For `ai-needs-info`, the resume prompt is a diff of new issue content
   (comments + body) against the last-seen snapshot, not a bare "continue".
5. A fresh daemon start automatically resumes any ticket that has a persisted
   session, not just relabeling `ai-wip` back to eligible.

## Design

### 1. One decision point, not five special cases

Rather than adding separate resume logic for "rework removed", "needs-info
answered", "dashboard Continue", and "daemon restart", the four triggers
collapse into **one fact**: does `<logDir>/session` already hold a session for
this ticket's worktree? `handleIssue` (`loop.go:222`) checks this once, right
after `Worktree.Create` (which already reuses the existing worktree/branch by
path — `worktree.go:46-88` — unchanged):

- **No persisted session** (first attempt on this issue, or the ticket somehow
  never got past `Worktree.Create`/`FetchIssueContent`) → today's fresh path:
  `RunFeaturePipeline`/`RunBugPipeline` called exactly as now.
- **Persisted session exists** → the resume path: skip straight to the
  persisted stage with `Resume: <sessionID>` and the trigger-appropriate prompt
  (§4), instead of calling `brainstorm-0`/`debug` again.

This means `SweepOrphans` and daemon-restart auto-resume need **no bespoke
logic of their own** (closing decision-5 and the crash-recovery case in one
move): they already just clear `ai-wip` and let the normal cycle re-triage and
call `handleIssue` (`loop.go:446-462`), and `handleIssue`'s new check does the
right thing whether the re-entry was caused by a crash, a label change, or a
dashboard click.

Dashboard Continue (`Orchestrator.Continue`, `loop.go:412-426`) also needs no
change beyond this: it already just clears `ai-stopped` and re-queues; the
resume now happens naturally on the next pick-up.

### 2. Persist which stage a session belongs to

The feature pipeline runs **three distinct sessions** (architect brainstorm,
fresh plan, fresh execute — `pipeline_feature.go:26-165`); the bug pipeline
runs one (`debug`). `RecordSession` (`claude.go:250-262`) currently only
records `{sessionId, kind}`, overwritten on every call — after a full run it
silently holds whichever stage ran *last*, with no way to tell the resume path
which pipeline entry point to re-enter.

`SessionInfo` gains a `Stage` field (`"brainstorm"`, `"plan"`, `"execute"`,
`"debug"`), passed alongside `kind` at every existing `RecordSession` call site
(`pipeline_feature.go:41,115,135,158`, `pipeline_bug.go:21`) — no new call
sites, just a new argument sourced from the `Label` each call site already
passes to `ClaudeCall` (stripped of its round suffix, e.g. `"brainstorm-3"` →
`"brainstorm"`).

Resume dispatch on `Stage`:
- `"brainstorm"` → re-enter the architect Q&A loop with `Resume: sessionID`
  and the trigger prompt in place of the round-0 `brainstormPrompt` call; the
  existing round loop (`pipeline_feature.go:64-119`) continues unchanged from
  there (same Q&A round cap, same `SPEC_READY`/already-done handling).
- `"plan"` → re-enter `runPlanThenExecute`'s plan call
  (`pipeline_feature.go:127-141`) with `Resume: sessionID` and the trigger
  prompt instead of `planPrompt(specPath)`.
- `"execute"` → re-enter `executePlan` (`pipeline_feature.go:145-157`) the
  same way.
- `"debug"` → re-enter `RunBugPipeline`'s single call
  (`pipeline_bug.go:11-16`) the same way.

A stage with no natural resume point (there isn't one here — every recorded
stage is a real `Claude.Call` site) would fall back to the fresh path; this is
a safety net, not an expected case.

### 3. Stop deleting worktrees and branches

Remove the four `wt.Remove`/`wt.DeleteBranch` calls in `finishDone`
(`loop.go:281,284`) and `finishNeedsInfo` (`loop.go:306,309`), and the two
`wt.Remove` calls in `ship` (`loop.go:496,501`). `Worktree.Remove` and
`Worktree.DeleteBranch` (`worktree.go:90-102`) stay as primitives — only their
callers in these three functions change; `worktree.go`'s own reclaim-a-bare-branch
fallback (`worktree.go:80-85`) is untouched, since it only fires when there's no
worktree left to reuse anyway.

A shipped (`ai-done`) issue's worktree now sits on disk permanently, same as a
parked or stopped one already does. That's an accepted, explicit trade-off of
"never delete" — disk usage grows unboundedly per closed issue — noted here so
it isn't rediscovered as a surprise; no cleanup mechanism is in scope for this
change.

### 4. Building the resume prompt

Two prompt strategies, chosen by trigger, matching the issue author's answers:

- **Default ("continue")** — used for a park-and-retry (API error, rework
  label removed, dashboard Continue, daemon restart): the literal string
  `"continue"`. No new state needed.
- **`ai-needs-info` diff** — used specifically when an issue re-enters after
  carrying `ai-needs-info`: reconstitute the prompt from what changed in the
  issue since the session was last active, not a bare "continue".

  A new file `<logDir>/issue-snapshot` stores the exact string
  `FetchIssueContent` produced (title + body + non-bot comments) the last time
  the pipeline read the issue — written alongside `RecordSession` at the same
  call sites, so it always matches what the paused session actually saw.
  On resume, `FetchIssueContent` is called again; the new text is diffed
  line-by-line against the stored snapshot (added lines only — comments are
  append-only from the daemon's perspective, and an edited issue body is rare
  enough that the whole new body is one "added" block under a simple diff, which
  is an acceptable approximation). The diff becomes the resume prompt (falling
  back to `"continue"` if the diff is empty, e.g. the label was removed with no
  new comment). The snapshot file is then overwritten with the new full text.

  This reuses the existing `ai-needs-info` re-queue trigger (label removed —
  `github.go:97-111`) rather than adding a second, always-on comment poll for
  labeled issues: today's design excludes `ai-needs-info` issues from
  `ListEligibleIssues` entirely while labeled, and nothing in the issue's
  answers asks for that gate to open early. Reconciling against the stored
  snapshot (not just "new comments") satisfies "always reconcile with the real
  issue body" from the issue's expected behavior.

### 5. Daemon-restart auto-resume

No new startup pass. `SweepOrphans` (`loop.go:446-462`) already runs at boot
and on every cycle, stripping `ai-wip` from anything a crash stranded and
re-queueing it with worktree/branch/session untouched. Once §1's dispatch is in
`handleIssue`, that re-queued pick-up automatically resumes the persisted
session — the same code path as every other trigger. `ai-rework`/`ai-stopped`
tickets are already excluded from the eligible/orphan queries and only move on
via their existing human-driven triggers (label removal, dashboard Continue),
both of which independently resume per §1.

### Error handling

- If `readSession` fails (no file, corrupt JSON) → treated as "no persisted
  session", falls back to the fresh path. Never a hard error.
- If `--resume <id>` itself fails (session expired/purged Claude-side) →
  the resulting `ClaudeCall` error surfaces exactly like any other pipeline
  error today: `park`, comment, wait for a human. No special-cased fallback to
  a fresh brainstorm — silently discarding a session the user expected resumed
  would contradict the issue's intent.
- Existing confidence-gate, Q&A-round-cap, and already-done logic in the
  resumed stage are unchanged; they run identically whether the first message
  into that session was the original prompt or a resume prompt.

### Testing

- Unit: `SessionInfo` round-trip with the new `Stage` field; snapshot diff
  helper (added-lines-only) on a few fixtures (new comment appended, body
  edited, nothing changed).
- Unit: `handleIssue`'s dispatch — no session file → fresh call; session file
  present with each `Stage` value → correct resume entry point and `Resume`
  id, exercised against a fake `Runner`.
- Integration-style (existing pattern in `loop_test.go`): a park → simulated
  label removal → next `handleIssue` call asserts `ClaudeCall.Resume` is set
  and the prompt is `"continue"`; same for a simulated `ai-needs-info` answer
  asserting the prompt contains the diffed new comment text and not the whole
  issue body.
- Regression: `finishDone`/`finishNeedsInfo`/`ship` tests updated to assert the
  worktree/branch are **still present** afterward instead of removed.
