# How loope works

Each poll cycle runs four steps:

1. **List** open issues carrying the eligible label (default `ai-agent`) that
   don't yet have a state label.
2. **Triage** — a Claude agent picks the single best issue and classifies it:
   - **`bug`** — small, well-scoped defect → one systematic-debugging session.
     It investigates, scores how confidently the bug can be fixed as reported
     and, below `confidenceThreshold`, escalates to `ai-needs-info` instead of
     guessing (see [Confidence gate](configuration.md#confidence-gate));
     otherwise it reproduces with a failing test, fixes, and commits.
   - **`feature`** — anything needing design → three sessions. An architect
     brainstorm session scores confidence and, below `confidenceThreshold`,
     escalates to `ai-needs-info`; otherwise it brainstorms with a cheaper
     "product owner proxy" agent in a Q&A loop, then writes and commits the
     spec. A **fresh** session turns that spec into a committed implementation
     plan, and a third session executes the plan.
   - **`done`** — the work is already fully implemented in the codebase → the
     loop comments, applies `ai-done`, and closes the issue without opening a PR.
3. **Work** happens on branch `ai/issue-<N>` in a dedicated git worktree under
   `workDir`, created from the remote default branch.
4. **Ship** — if the pipeline produced at least one commit, the branch is pushed
   and a PR is opened (`Closes #N`); the PR URL is commented on the issue.

## Concurrency and scheduling

A poll cycle does **not** wait for the pipelines it starts. It fills the free
`ticketsPerCycle` slots, returns, and polls again one interval later — so work
labelled while other pipelines are running is picked up as soon as a slot frees,
rather than at the end of a batch.

Within a cycle, auto-resumes of parked issues claim slots **before** new eligible
issues: continuing work that already has a worktree and session on disk outranks
starting more of it, so a permanently busy queue can't starve a parked issue.
Resumes are backoff-gated, so they leave the rest of the budget for new work.

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
| `ai-rework`     | Pipeline hit an error; progress preserved for manual rework       |
| `ai-needs-info` | Brainstorm wasn't confident enough; awaiting author clarification |
| `ai-stopped`    | You stopped the run from the dashboard; preserved, awaiting Continue |

On failure the loop comments the error on the issue, swaps `ai-wip` →
`ai-rework`, and **preserves** the worktree, branch, logs, and the Claude session
id (saved in `logs/issue-<N>/session`). Nothing is deleted, so no progress is
lost.

Parked issues recover automatically: each poll cycle the daemon auto-resumes
resumable `ai-rework` issues (backoff-gated), continuing the saved Claude session
in the preserved worktree, finishing the work, and shipping the PR (swapping
`ai-rework` → `ai-done`). If the worktree or session file is gone, remove the
`ai-rework` label to re-queue the issue from scratch.

You can also **Stop** a running ticket from the dashboard and **Continue** it
later — see [the dashboard docs](dashboard.md#stop-and-continue-a-ticket). A stop
swaps `ai-wip` → `ai-stopped` and preserves everything; because `ai-stopped` is
neither `ai-wip` nor `ai-rework`, no auto-resume or crash-sweep path ever touches
it, so a stopped ticket stays put (even across a daemon restart) until you hit
Continue.

> `ai-failed` is deprecated: the loop no longer applies it, though existing
> `ai-failed` issues are still recognized and stay out of the queue.

See [Operations](operations.md) for how transient failures and crashes
self-heal without manual cleanup.
