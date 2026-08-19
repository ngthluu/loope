# Push and open a PR at each feature-pipeline stage, not only at the end

Issue: #58

## Problem

`RunFeaturePipeline`/`ResumeFeaturePipeline` (`pipeline_feature.go`) run three
sessions — brainstorm→spec, plan, execute — entirely inside a local worktree.
Nothing is pushed to GitHub, and no PR exists, until the whole pipeline
returns successfully and `handleIssue` calls `o.ship` (`loop.go:482-512`),
which pushes the branch, creates the PR, and comments its URL in one shot.

That means a spec that took real Q&A effort to reach, or a plan a human might
want to read early, is invisible on GitHub until execute also finishes —
often much later, and never at all if execute fails or the run gets parked.
The bug pipeline (`pipeline_bug.go`) has no spec/plan/execute stages (a single
debug session) and is out of scope here.

## Decisions (from the issue author)

1. "Implement step" has no mid-execute granularity: push once, when the
   execute session (or the last grouped execute session) finishes — not per
   commit, not per plan checklist item.
2. Feature pipeline only; the bug pipeline is unchanged.
3. The PR is created immediately once the spec is committed, containing just
   the spec commit. The same PR (same branch) is reused for the plan and
   execute pushes — nothing else creates a second PR.
4. The plan-complete update is a **PR comment** ("update plan: ..."), not a
   `gh pr edit --body` replace.
5. If a push or PR-create fails mid-pipeline (spec or plan stage), the
   pipeline keeps going rather than aborting; the final `ship` step's own
   push/PR-create already retries this at the end (it's idempotent — see
   below).

## Design

### 1. Three new push points in the feature pipeline

`RunFeaturePipeline` and `ResumeFeaturePipeline` gain three parameters they
don't have today: `gh *GitHub`, `wt *Worktree`, and `branch string` (the
issue number/title are already in scope via `issueContent`/callers — the
branch name and title are threaded through from `handleIssue`, same as `ship`
already receives them). `pipeline_bug.go` is untouched.

- **Spec complete** — in `brainstormLoop`, right after `resolveSpec` succeeds
  and before `runPlanThenExecute` is called (`pipeline_feature.go:105-113`):
  push the branch, call `gh.CreatePR(branch, prTitle(title, n), prBody(n,
  "feature"))` (the exact helpers `ship` already uses), comment the URL with
  the existing `prComment(url)`, and `recordPR(logDir, url)`.
- **Plan complete** — in `runPlanThenExecute`, right after `findPlanFile`
  succeeds and before `executePlan` is called (`pipeline_feature.go:189-193`):
  push the branch, then post a comment with the fixed, deterministic text
  `"Updated plan: `<plan file path, relative to repo root>`"` via
  `gh.Comment`. No body edit.
- **Execute complete** — after `executePlan` (both the ungrouped and grouped
  paths) and after `resumeExecutePlan` return successfully: push the branch
  once. No comment — decision 1 rules out any finer signal, and the plan
  stage already announced the work; this push just makes sure the commits are
  on GitHub promptly instead of waiting for `ship`.

Each of these three points is a **new, small, best-effort step**: on any
error (push fails, `CreatePR` fails, `Comment` fails) it's logged
(`log.Printf`) and swallowed — the pipeline proceeds to the next stage
exactly as if the step had succeeded (decision 5). None of these steps can
turn a successful pipeline stage into a pipeline error.

### 2. Reusing the existing idempotent primitives

`Worktree.Push` and `GitHub.CreatePR` are already safe to call more than once
for the same branch: `Push` is a plain `git push`, and `CreatePR`
(`github.go:220-236`) already recovers the existing PR's URL and returns it
as success when `gh pr create` reports "already exists". So the spec-stage
step, the plan-stage step, the execute-stage step, and `ship`'s own
push/`CreatePR` at the very end can all run — in sequence, across however
many of these stages actually execute — without erroring or creating
duplicate PRs.

### 3. `ship` must not re-post the PR-link comment

`ship` (`loop.go:482-512`) unconditionally posts `prComment(url)` after
`CreatePR` returns. Once the spec stage already created the PR and posted
that same comment, `ship` would otherwise post it a second time on every
successful feature run — a visible, confusing duplicate.

Fix: add `hasPR(logDir) bool` next to the existing `recordPR` (`tracker.go`),
reading whether `<logDir>/pr` already exists. In `ship`, check `hasPR`
*before* calling `CreatePR`:

- Already recorded (the spec stage created it) → still run `CommitCount` and
  `Push` (they're the existing safety checks — an execute failure partway
  through grouped sessions could still leave nothing new to push, which
  should still park per the existing "no commits" rule) and still call
  `CreatePR` to resolve the canonical URL for the label swap, but **skip**
  `gh.Comment`/`recordPR` — the comment was already posted and the file
  already written.
- Not recorded (bug pipeline, or a feature run from before this change) →
  today's full behavior, unchanged.

### 4. What doesn't change

- `ship`'s commit-count check, label swap, and park-on-failure behavior are
  unchanged — it's still the authoritative "did this issue actually finish"
  gate.
- The bug pipeline, `RunBugPipeline`/`ResumeBugPipeline`, is untouched.
- No new state machine: the three new push points are unconditional steps
  inline in the existing control flow, not a persisted "which stage have I
  pushed" marker — `recordPR`'s existing file is the only new durable state,
  and it already existed for a different purpose (the dashboard PR link).

### Error handling

- Push/PR-create/comment failures at the spec or plan stage: logged, ignored,
  pipeline continues (decision 5).
- Push failure after execute completes: logged, ignored — `ship`'s own push
  at the end is the backstop.
- `ship` itself is unchanged in how *it* fails (parks on a deterministic
  tooling error), just skips one comment when a PR already exists.

### Testing

- Unit: the three new push/PR/comment steps in `pipeline_feature.go`, each
  exercised against a fake `GitHub`/`Worktree` — asserting the calls happen
  at the right point in the sequence, and that an injected error from any of
  them does not stop the pipeline from proceeding to the next stage.
- Unit: `hasPR`/`recordPR` round-trip in `tracker.go`.
- Regression: `ship` test asserting that when `<logDir>/pr` already exists,
  `gh.Comment` is NOT called again (only `CreatePR`, `CommitCount`, `Push`,
  and the label swap run), and that the pre-existing "no PR yet" path is
  unchanged.
- Integration-style (existing `loop_test.go` pattern): a full feature-pipeline
  run against a fake `Claude`/`GitHub`/`Worktree` asserting a PR exists (and
  is commented) right after the spec stage — before plan or execute run at
  all — and that only one "🤖 PR:" comment exists on the issue at the end of
  the run.
