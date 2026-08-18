# Post-ship code review rounds

Issue: #55

## Problem

Once loope ships a PR, nothing looks at the diff for correctness bugs or
cleanup opportunities. The only quality gate before `ai-done` is whatever the
execute/debug session self-checked.

## Goal

After a PR is created, run one or more blocking Claude sessions that invoke the
existing `/code-review` skill against the diff, apply its fixes, push them, and
post the findings as PR review comments — before the issue is marked
`ai-done`. Configurable model and round count; entirely optional (absent config
= step skipped). Applies to both the feature and bug routes.

## Decisions (from the issue author)

1. Blocking: the daemon waits for the review session(s), and the fixing happens
   **inside** that same session (no park-on-findings, no loop-back into the
   execute session).
2. Applies to both the feature pipeline (after `execute`) and the bug pipeline
   (after `debug`) — in practice, to both routes' single shared ship path.
3. The review session invokes the existing `/code-review` skill (with `--fix`)
   rather than a bespoke review prompt.
4. Config is a new `Models.CodeReview` block; its presence enables the step,
   its absence skips it entirely.
5. Findings are posted as **PR review comments**, which requires the PR to
   already exist — so this step runs **after `ship()`**, not between
   execute/debug and ship.
6. Round count is configurable (e.g. `rounds: 2` → two review-and-fix passes,
   each its own session), sequential, each running after the previous
   finishes.

## Design

### Where this sits relative to `ship()`

Today `ship()` (`loop.go:482-512`) ends with `SwapLabels(ai-wip → ai-done)`
immediately after `CreatePR`/`Comment`/`recordPR`. That label swap is what the
daemon's orphan-sweep/resume logic uses to decide an issue still needs work
(`ai-wip`) versus is finished (`ai-done`).

Because findings need an existing PR (decision 5), the review loop must run
**after** `CreatePR` succeeds — but it must run **before** the label swaps to
`ai-done`, otherwise a daemon restart mid-review-loop would strand the issue:
it would no longer be `ai-wip`, so nothing would resume it, and the review
loop would just silently never finish. So `ship()` gains a new step between
`recordPR(...)` and `SwapLabels(...)`:

```
CommitCount → Push → CreatePR → Comment(PR URL) → recordPR
  → NEW: CodeReview.Run(...)   // blocking, may push more commits
  → SwapLabels(ai-wip → ai-done) → recordState(Done)
```

The review loop's own errors or an unresolved "blocked" finding do **not**
revert the PR or re-park the issue — the PR already shipped successfully; code
review is a quality pass layered on top, not a gate on shipping. It only ever
delays (never blocks forever, since it always terminates after `Rounds` or an
early clean pass) the `ai-done` transition. This applies identically to the
feature and bug routes since both funnel through this one `ship()` call site.

### Config

```go
// config.go
type CodeReviewConfig struct {
    ModelConfig
    Rounds int `json:"rounds"` // <= 0 treated as 1
}

type Models struct {
    ...
    // CodeReview is the config for the post-ship review-and-fix loop. Unlike
    // Execute it has no fallback to Architect: a real model choice here
    // matters (the session must both find and fix issues), so an absent
    // block means the whole step is skipped, not "use defaults."
    CodeReview *CodeReviewConfig `json:"codeReview"`
}
```

`CodeReview` is a pointer specifically so JSON presence is the on/off switch
(mirrors `Telemetry *TelemetryConfig`, not `UAT`'s value-typed, always-
constructed field — `UAT` has no config-presence gate today, and copying that
would leave no way to satisfy decision 4 literally).

### New file `codereview.go`

```go
const (
    codeReviewBeginSentinel = "CODEREVIEW_BEGIN"
    codeReviewEndSentinel   = "CODEREVIEW_END"
)

// CodeReviewTarget is the GitHub surface the review loop posts findings to.
type CodeReviewTarget interface {
    PRNumberForBranch(ctx context.Context, branch string) (int, error)
    ReviewComment(ctx context.Context, prNumber int, body string) error
}

// CodeReview runs the post-ship review-and-fix loop. A nil *CodeReview, a nil
// Target, or a nil cfg.Models.CodeReview all disable the step.
type CodeReview struct {
    Target CodeReviewTarget
    Num    int
}

func (r *CodeReview) Run(ctx context.Context, c *Claude, cfg *Config, wtPath, branch, base, logDir string) error
```

`Run`, per round `i` from `lastCompletedRound(logDir)+1` through
`cfg.Models.CodeReview.Rounds` (defaulting to 1):

1. **Resolve the PR number** once via `Target.PRNumberForBranch(ctx, branch)`.
   If this errors, log and return — there is nowhere to post findings.
2. **Call Claude**: `Label: fmt.Sprintf("codereview-%d", i)`,
   `Model: cfg.Models.CodeReview.ModelConfig`, `Dir: wtPath`,
   `SkipPermissions: true`, prompt from `codeReviewPrompt(i, rounds, base)`.
   No `DisallowedTools` — unlike UAT, this session must write, commit, and run
   `/code-review --fix`.
3. **Push whatever the session committed**: `Push` is a plain idempotent
   `git push -u origin <branch>` (`worktree.go:104-109`) — safe to call every
   round regardless of whether the session actually committed, so Go code
   drives it rather than trusting the prompt to remember.
4. **Parse the result** with `parseCodeReview(result)`: text between
   `CODEREVIEW_BEGIN`/`CODEREVIEW_END` (same fence idiom as `UAT_BEGIN`/
   `UAT_END`), whose first line is `STATUS: clean|fixed|blocked` and the rest
   is a human-readable summary. Missing fence or missing status line is
   treated as `blocked` with the raw result as the summary, so an
   off-contract session still surfaces something rather than being silently
   dropped.
5. **Post** the summary via `Target.ReviewComment(ctx, prNumber, body)`,
   prefixed with the round number and a `<!-- loope:codereview:N -->` marker
   for log traceability (no idempotency check needed — Go, not GitHub state,
   tracks the round cursor).
6. **Persist progress**: write `i` to `<logDir>/codereview-round` so a daemon
   restart resumes at round `i+1` instead of redoing finished rounds (the
   CLAUDE.md "continue from existing state" principle).
7. **Stop early** if `STATUS: clean`. Otherwise continue to the next round, up
   to `Rounds`.

Every Claude call in this loop uses `Label` values distinct from the primary
resumable session (`codereview-N`) and does **not** call `RecordSession` for
the primary `session` file — it must not clobber the resumable primary
session pointer, same rule as UAT (`uat.go:107-108`). Its own resume state is
the separate `codereview-round` file from step 6.

### New `GitHub` methods (`github.go`)

Following the existing `g.gh(...)` + JSON pattern used by `existingPRURL`:

```go
// PRNumberForBranch returns the PR number for branch, via `gh pr view --json number`.
func (g *GitHub) PRNumberForBranch(ctx context.Context, branch string) (int, error)

// ReviewComment posts a PR review with a top-level comment body, via
// `gh pr review <number> --comment --body <text>`. Distinct from Comment,
// which posts a plain issue-style comment.
func (g *GitHub) ReviewComment(ctx context.Context, prNumber int, body string) error
```

### Prompt

New `ai/prompts/codereview.md.tmpl`, receiving `.Round`, `.Rounds`, `.Base`:

- Run `/code-review` (skill) against `origin/<Base>...HEAD` with `--fix`
  applied, then commit any changes it makes.
- Output *only* a status line and summary between `CODEREVIEW_BEGIN` and
  `CODEREVIEW_END`: `STATUS: clean` if `/code-review` found nothing, `STATUS:
  fixed` with a short bullet summary of what was fixed if it applied changes,
  or `STATUS: blocked` with a short explanation if a finding can't be safely
  auto-fixed.
- Do not ask questions (headless).

`promptData()` gains `CodeReviewBeginSentinel`/`CodeReviewEndSentinel`, and a
new builder `codeReviewPrompt(round, rounds int, base string) string` mirrors
the existing `uatFeaturePrompt`/`uatBugPrompt` shape.

### Wiring in `loop.go`

`ship()` constructs `&CodeReview{Target: o.gh, Num: n}` and calls
`.Run(ctx, c, cfg, wtPath, branch, base, o.issueLogDir(n))` between `recordPR`
and `SwapLabels`, logging (not propagating) any returned error — a review-loop
failure must never turn a successful ship into a park.

## Error handling

| Condition | Behavior |
|---|---|
| `cfg.Models.CodeReview == nil` | Step skipped entirely; `ship()` behaves exactly as before. |
| `PRNumberForBranch` fails | Log, skip the whole loop (nothing to post to). |
| Claude session errors or hits its budget cap | Log, stop the loop at the current round; already-posted rounds' comments and pushed commits stand. |
| No `CODEREVIEW_BEGIN` fence / missing `STATUS:` line | Treat as `blocked`, post the raw result as the summary, stop the loop. |
| `STATUS: blocked` | Post the summary, stop the loop (no further rounds spent on a finding the session says it can't fix). |
| `ReviewComment` fails | Log; still write the round-progress file and continue/stop per status, so the round isn't repeated just because the comment failed. |
| Push fails | Log, stop the loop — a round whose fix can't reach the PR isn't worth reviewing further. |

In every case, `ship()` still proceeds to `SwapLabels(ai-wip → ai-done)` —
this step never blocks or reverts a successful ship.

## Testing

- `parseCodeReview`: fence present with each status; fence present but no
  status line; no fence at all; summary text preserved verbatim.
- Round-loop behavior against a fake `CodeReviewTarget` and a fake `Runner`:
  stops early on `clean`; runs exactly `Rounds` times when always `fixed`;
  stops on `blocked`; resumes from `codereview-round` instead of repeating
  finished rounds.
- Config: `models.codeReview` round-trips from JSON; a config file without a
  `codeReview` block produces a nil pointer and `Run` is a no-op with zero
  Claude calls.
- `ship()` integration: a `CodeReview.Run` that errors still results in
  `SwapLabels`/`recordState(Done)` being called.
- `GitHub.PRNumberForBranch` / `ReviewComment` argument shape, against the
  existing fake runner used for `existingPRURL`.
- Golden prompt test for `codeReviewPrompt`, matching
  `prompts_golden_test.go`'s existing pattern.

## Out of scope

- Inline (per-line/per-hunk) PR review comments — this uses top-level
  `gh pr review --comment`, not the diff-anchored comments API.
- Surfacing round progress in the web dashboard.
- Applying this loop to the rework flow.
- A config knob to make code-review failures actually block `ai-done` — per
  decision 1/5, this step is additive quality, never a shipping gate.
