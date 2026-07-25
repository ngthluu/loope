# How loope works

Each poll cycle runs four steps:

1. **List** open issues carrying the eligible label (default `ai-agent`) that
   don't yet have a state label.
2. **Triage** — a Claude agent picks the single best issue and classifies it:
   - **`bug`** — small, well-scoped defect → one systematic-debugging session.
     It investigates, scores how confidently the bug can be fixed as reported
     and, below `confidenceThreshold`, escalates to `ai-needs-info` instead of
     guessing (see [Confidence gate](configuration.md#confidence-gate));
     otherwise it reproduces with a failing test, fixes, and commits. A short
     read-only session then reads the issue and the resulting diff and appends a
     UAT checklist to the issue body.
   - **`feature`** — anything needing design → three sessions. An architect
     brainstorm session scores confidence and, below `confidenceThreshold`,
     escalates to `ai-needs-info`; otherwise it brainstorms with a cheaper
     "product owner proxy" agent in a Q&A loop, then writes and commits the
     spec. A short read-only session then turns that spec into a UAT checklist
     appended to the issue body. A **fresh** session turns the spec into a
     committed implementation plan, and a third session executes the plan.
   - **`done`** — the work is already fully implemented in the codebase → the
     loop comments, applies `ai-done`, and closes the issue without opening a PR.
3. **Work** happens on branch `ai/issue-<N>` in a dedicated git worktree under
   `workDir`, created from the remote default branch.
4. **Ship** — if the pipeline produced at least one commit, the branch is pushed
   and a PR is opened (`Closes #N`); the PR URL is commented on the issue.

## UAT checklist

Both routes publish a hand-verification checklist onto the **issue body** (not a
comment), under a `## 🤖 UAT checklist` heading preceded by an invisible
`<!-- loope:uat -->` marker. The marker is the idempotency key: an issue that
already carries it is never given a second checklist, so re-runs, resumes and
reworks leave the first one in place.

The step is deliberately non-blocking. It runs in its own ephemeral session
(`models.uat`, no inheritance from `architect`) with `Write`, `Edit` and
`NotebookEdit` disabled, and it never records a resumable session. If anything
goes wrong — the body fetch fails, the session errors or hits its cap, the
result carries no checklist, the body would grow past GitHub's size limit — the
step logs the issue number and the reason and returns, and the pipeline carries
on to plan/execute or to shipping. The session's full output is kept as
`<seq>-uat.output.md` in the issue's log directory either way.

On the feature route it runs immediately after the spec is committed, before the
plan exists: the checklist describes the behavior the spec promises. On the bug
route it runs after the fix, from the issue plus `git diff origin/<base>...HEAD`,
and only on the outcome where a fix was actually produced — an `ai-needs-info`
escalation or an already-done close publishes nothing. The rework flow does not
run it: the feature route already published one, and the marker check would skip
it anyway.

## Concurrency and scheduling

A poll cycle does **not** wait for the pipelines it starts. It fills the free
`ticketsPerCycle` slots, returns, and polls again one interval later — so work
labelled while other pipelines are running is picked up as soon as a slot frees,
rather than at the end of a batch.

Within a cycle, resumes of interrupted issues claim slots **before** new eligible
issues: continuing work that already has a worktree and session on disk outranks
starting more of it, so a permanently busy queue can't starve it. Resumes are
backoff-gated, so they leave the rest of the budget for new work.

On shutdown (Ctrl-C / SIGTERM) the daemon stops polling and waits for in-flight
pipelines to finish, so the `workDir` lock is never released while a pipeline is
live. Signal a second time to quit immediately without draining.

## Label lifecycle

Label names are configurable — see [`stateLabels`](configuration.md#statelabels).

| Label           | Meaning                                                           |
|-----------------|-------------------------------------------------------------------|
| `ai-agent`      | You add this: the issue is eligible for the loop                  |
| `ai-wip`        | The loop is working on it                                         |
| `ai-done`       | PR created; issue leaves the queue                                |
| `ai-rework`     | Pipeline hit an error; progress preserved, waiting for you        |
| `ai-needs-info` | Brainstorm wasn't confident enough; awaiting author clarification |
| `ai-stopped`    | You stopped the run from the dashboard; preserved, awaiting Continue |

On failure the loop comments the error on the issue, swaps `ai-wip` →
`ai-rework`, and **preserves** the worktree, branch, logs, and the Claude session
id (saved in `logs/issue-<N>/session`). Nothing is deleted, so no progress is
lost.

A parked issue then **waits for you** — nothing is retried automatically, not
even a usage limit or a network blip. The comment carries the full error, and the
way forward is to remove the `ai-rework` label: the issue is eligible again and
the next run reuses the preserved worktree and branch rather than starting over.

The one exception is an interrupted run: when a daemon restart lands mid-pipeline
the sweep parks the issue with an "interrupted mid-run" cause, and that — being a
hand-off rather than a failure — the daemon does resume on its own.

You can also **Stop** a running ticket from the dashboard and **Continue** it
later — see [the dashboard docs](dashboard.md#stop-and-continue-a-ticket). A stop
swaps `ai-wip` → `ai-stopped` and preserves everything; because `ai-stopped` is
neither `ai-wip` nor `ai-rework`, no resume or crash-sweep path ever touches it, so a stopped ticket stays put (even across a daemon restart) until you hit
Continue.

> `ai-failed` is deprecated: the loop no longer applies it, though existing
> `ai-failed` issues are still recognized and stay out of the queue.

See [Operations](operations.md) for how transient failures and crashes
self-heal without manual cleanup.
