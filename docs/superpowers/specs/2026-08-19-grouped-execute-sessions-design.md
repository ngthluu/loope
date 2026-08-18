# Grouped execute sessions for the feature pipeline

Issue: #54

## Problem

The feature pipeline's execute step (`executePlan` in `pipeline_feature.go`)
runs the entire implementation plan in a single Claude session. A large plan
(many tasks) can blow past that session's usable context before the plan is
finished, producing a degraded or truncated implementation with no way to
recover except a full rework.

## Goal

Let an operator configure a cap on how much of the plan one execute session
attempts, so a large plan is implemented across several fresh, bounded
sessions instead of one unbounded one — without requiring the daemon to
parse the plan file's step/task structure, which the plan author confirmed
is not reliably machine-parseable.

## Decisions (from the issue author)

1. No deterministic step-boundary parsing. The execute prompt just tells the
   session "implement the next N steps, then stop" and relies on the session
   itself to read the plan and prior git history to figure out where to pick
   up.
2. Each group runs in a **brand-new** Claude session (no `--resume` between
   groups).
3. On a mid-group failure, retry **that session** with a "continue" prompt.
   The completion/continuation signal is left to this design.
4. The steps-per-session cap lives in a **new global `Config` field**, set in
   the JSON config. It's optional; leaving it unset preserves the current
   single-session behavior exactly.
5. `subagent-driven-development` continues to run *inside* each session,
   unchanged — this feature only adds a boundary *between* sessions.

## Design

### Config

`Config` (`config.go`) gains one optional field:

```go
type Config struct {
    ...
    StepsPerSession int `json:"stepsPerSession"`
    ...
}
```

`0` (the zero value, and the default when the key is absent) means "current
behavior": one fresh execute session implements the whole plan, with no
grouping instructions and no completion sentinel required. This keeps every
existing config working unmodified (decision 4).

### Sentinels

Two new consts alongside `readySentinel` / `specReadySentinel` in
`pipeline_feature.go`:

```go
const groupDoneSentinel = "GROUP_DONE"     // this group finished; more plan remains
const planCompleteSentinel = "PLAN_COMPLETE" // the whole plan is now finished
```

Both are added to `promptData()` (`prompts.go`) as `GroupDoneSentinel` /
`PlanCompleteSentinel`, per the existing rule that sentinels are never
hardcoded into a `.tmpl` file.

### Prompt

`ai/prompts/execute.md.tmpl` gains a conditional block, rendered only when
grouping is active:

```
/superpowers:executing-plans Execute the plan at {{.PlanPath}}.
Use the execution style the plan recommends (subagent-driven or inline).
Follow TDD per the plan. Commit as you complete tasks.
HEADLESS: do not ask questions; make reasonable calls and note them in commit messages.
{{if gt .StepsPerSession 0}}
This plan is being executed across multiple bounded sessions to avoid running
out of context. Implement only the next {{.StepsPerSession}} steps of the
plan, even if more remain — do not go further. Before starting, check git log
and the plan file for what earlier sessions already completed, and continue
from there.
If those steps finish the entire plan, commit and end your final reply with
{{.PlanCompleteSentinel}}. Otherwise, once you've implemented up to
{{.StepsPerSession}} steps, commit your progress and end your final reply
with {{.GroupDoneSentinel}}.
{{end}}
```

`StepsPerSession` defaults to `0` in the template data for the ungrouped
call, so the conditional block — and therefore any sentinel requirement —
never appears in the current default flow. `executePrompt(planPath string)`
keeps its existing signature and is used unchanged for that path.

A second builder covers grouped calls:

```go
func executeGroupPrompt(planPath string, stepsPerSession int) string
```

It renders the same template with `StepsPerSession` set.

A short continuation prompt, its own template
`ai/prompts/execute-continue.md.tmpl`, is used for the mid-group retry:

```
Continue. You were implementing a bounded group of plan steps and either did
not finish or did not end with the expected sentinel. Check git status and
the plan file for what remains in this group, finish it, commit, and end
your reply with {{.PlanCompleteSentinel}} if the whole plan is now done, or
{{.GroupDoneSentinel}} if more remains for a later session.
```

```go
func executeContinuePrompt() string
```

### Daemon loop

`executePlan` (`pipeline_feature.go`) dispatches on `cfg.StepsPerSession`:

```go
func executePlan(ctx context.Context, c *Claude, cfg *Config, wtPath, planPath string) error {
    if cfg.StepsPerSession <= 0 {
        // unchanged: single fresh session, whole plan, no sentinel check
        res, err := c.Call(ctx, ClaudeCall{
            Dir: wtPath, Label: "execute", Prompt: executePrompt(planPath),
            Model: cfg.Models.executeConfig(), SkipPermissions: true,
            DisallowedTools: []string{"AskUserQuestion"},
        })
        if res != nil {
            c.RecordSession(res.SessionID, "feature")
        }
        return err
    }
    return executePlanGrouped(ctx, c, cfg, wtPath, planPath)
}
```

`executePlanGrouped` loops, spawning one **fresh** session per group
(decision 2):

```go
const maxExecuteGroups = 20 // safety cap: no deterministic total-step count exists
const maxGroupRetries = 2   // retries of a single group before failing the pipeline

func executePlanGrouped(ctx context.Context, c *Claude, cfg *Config, wtPath, planPath string) error {
    for i := 1; i <= maxExecuteGroups; i++ {
        label := fmt.Sprintf("execute-group-%d", i)
        result, err := runGroupWithRetry(ctx, c, cfg, wtPath, label, executeGroupPrompt(planPath, cfg.StepsPerSession))
        if err != nil {
            return err
        }
        if strings.Contains(result, planCompleteSentinel) {
            return nil
        }
        // strings.Contains(result, groupDoneSentinel), or ambiguous-but-retried-out:
        // either way, move on to a fresh session for the next group.
    }
    return fmt.Errorf("feature pipeline: execute did not signal %s within %d grouped sessions", planCompleteSentinel, maxExecuteGroups)
}
```

`runGroupWithRetry` runs one group's initial call, then — only when the call
errors *with a usable session id* or succeeds without either sentinel —
retries up to `maxGroupRetries` times via `--resume` on that same session
with the continuation prompt (decision 3). An error with no session id
(nothing to resume) fails the pipeline immediately, matching how every other
stage already propagates a hard Claude failure.

```go
func runGroupWithRetry(ctx context.Context, c *Claude, cfg *Config, wtPath, label, prompt string) (string, error) {
    resume := ""
    for attempt := 0; attempt <= maxGroupRetries; attempt++ {
        callLabel := label
        if attempt > 0 {
            callLabel = fmt.Sprintf("%s-retry-%d", label, attempt)
            prompt = executeContinuePrompt()
        }
        res, err := c.Call(ctx, ClaudeCall{
            Dir: wtPath, Label: callLabel, Prompt: prompt, Resume: resume,
            Model: cfg.Models.executeConfig(), SkipPermissions: true,
            DisallowedTools: []string{"AskUserQuestion"},
        })
        if res != nil {
            c.RecordSession(res.SessionID, "feature")
        }
        if err != nil {
            if res == nil || res.SessionID == "" {
                return "", err // nothing to resume; fail the pipeline now
            }
            resume = res.SessionID
            continue // retry via continuation prompt
        }
        if strings.Contains(res.Result, planCompleteSentinel) || strings.Contains(res.Result, groupDoneSentinel) {
            return res.Result, nil
        }
        resume = res.SessionID // neither sentinel: ambiguous, retry with "continue"
    }
    return "", fmt.Errorf("feature pipeline: %s did not signal completion after %d retries", label, maxGroupRetries)
}
```

Each retry attempt still calls `RecordSession`, matching the existing
overwrite-with-latest behavior used for the dashboard's "current session"
display.

### State continuity

No new state-passing mechanism is needed: every session in the pipeline
already shares the same worktree and branch, so a later group's fresh
session sees the prior group's committed work by reading the repo — exactly
the "recover and continue from existing state" principle already documented
for `Worktree.Create` in `CLAUDE.md`. The daemon itself tracks no per-group
progress; the plan file and git log are the source of truth the prompt
points each new session at.

### Config docs and example

- `loope.json.example` gets a commented `"stepsPerSession"` entry at the top
  level (sibling of `maxQARounds`), documented as optional.
- `docs/configuration.md` gets a short paragraph: what the field does, that
  `0`/absent means unbounded (current behavior), and that it only affects the
  feature pipeline's execute step.

## Error handling

| Condition | Behavior |
|---|---|
| `stepsPerSession` unset or `0` | Unchanged: one fresh session, whole plan, no sentinel required. |
| Group session errors with no session id | Pipeline fails immediately (nothing to resume). |
| Group session errors but has a session id | Retry via `--resume` + continuation prompt, up to `maxGroupRetries`. |
| Group session succeeds but emits neither sentinel | Same retry path as above. |
| Group session emits `GROUP_DONE` | Move to the next group in a fresh session. |
| Group session emits `PLAN_COMPLETE` | Pipeline succeeds. |
| Retries exhausted without a sentinel | Pipeline fails with a descriptive error (parks the issue, as any other pipeline error does). |
| `maxExecuteGroups` reached without `PLAN_COMPLETE` | Pipeline fails with a descriptive error. |

## Testing

- `executeGroupPrompt` / `executeContinuePrompt`: golden prompt tests
  alongside the existing ones in `prompts_golden_test.go`.
- `executePrompt(planPath)` (ungrouped) renders identically to today —
  regression test against the existing golden fixture.
- `executePlan` with `StepsPerSession == 0`: single call, label `execute`,
  no sentinel check, matching current behavior exactly (fake runner).
- `executePlanGrouped` with a fake runner scripted to return `GROUP_DONE`
  twice then `PLAN_COMPLETE`: three fresh sessions (`execute-group-1..3`,
  each with `Resume == ""`), pipeline returns nil.
- `runGroupWithRetry`: a scripted response with no sentinel triggers a
  `-retry-1` call with `Resume` set to the prior session id and the
  continuation prompt; exhausting `maxGroupRetries` returns an error.
- `runGroupWithRetry`: an error result with an empty `SessionID` fails
  immediately with no retry call made.
- `maxExecuteGroups` safety cap: a fake runner that always returns
  `GROUP_DONE` fails the pipeline after exactly `maxExecuteGroups` sessions.
- Config: `stepsPerSession` round-trips from JSON; absent key defaults to
  `0` and preserves existing example configs.

## Out of scope

- Any deterministic parsing of plan step/task boundaries.
- Grouping for the plan or brainstorm stages — only the execute step splits.
- Grouping for the bug pipeline (`pipeline_bug.go`), which has no plan/execute
  split today.
- Surfacing per-group progress in the web dashboard beyond the existing log
  file listing (each group already gets its own `NNN-execute-group-N.*` log
  files for free, via the existing `Label`-based log naming).
