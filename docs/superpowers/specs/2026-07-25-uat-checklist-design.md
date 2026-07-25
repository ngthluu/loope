# UAT checklist published to the issue body

Issue: #32

## Problem

When loope finishes a ticket it opens a PR and marks the issue `ai-done`. A
human then has to read the diff (or the spec) to work out how to verify the
result by hand. Nothing in the pipeline produces a "here is how you check this
works" artifact.

## Goal

After the feature pipeline commits its spec, and after the bug pipeline fixes
the bug, a separate short Claude session writes a UAT checklist — simple, short,
checklist style, covering 100% of the context — and appends it to the GitHub
issue body so the human sees it in place.

## Decisions (from the issue author)

1. The UAT is logged as a file and appended to the **issue body**. If the body
   already carries a generated UAT, later runs on that issue must not write one
   again.
2. Feature route: the UAT session runs **immediately after the spec**, before
   plan and execute.
3. A failed UAT step **never blocks** the pipeline.
4. Bug route input is the **issue content plus the resulting diff**.
5. The UAT session gets its **own `models.uat` config block**, with **no
   fallback** to `architect`.

## Design

### New file: `uat.go`

```go
const (
    uatMarker      = "<!-- loope:uat -->"
    uatBeginSentinel = "UAT_BEGIN"
    uatEndSentinel   = "UAT_END"
)

// UATTarget is the GitHub surface the UAT step reads from and publishes to.
type UATTarget interface {
    IssueBody(ctx context.Context, n int) (string, error)
    AppendIssueBody(ctx context.Context, n int, text string) error
}

// UAT publishes a human-verifiable acceptance checklist onto the issue body.
// A nil *UAT disables the step entirely.
type UAT struct {
    Target UATTarget
    Num    int
}

func (u *UAT) RunFeature(ctx context.Context, c *Claude, cfg *Config, wtPath, specPath string)
func (u *UAT) RunBug(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string)
```

Both entry points are nil-receiver safe (`if u == nil || u.Target == nil {
return }`) and return nothing: every failure path logs and continues (decision
3). They share one unexported `run(ctx, c, cfg, wtPath, label, prompt string)`
that does the whole sequence; the exported pair only differ in which prompt they
build.

### The `run` sequence

1. **Idempotency check, before spending a session.** Fetch the live issue body
   via `Target.IssueBody`. If it contains `uatMarker`, log
   `issue #N: UAT already present, skipping` and return. If the fetch *errors*,
   also skip — a duplicated UAT section on the issue is worse than a missing
   one, and the next run gets another chance.
2. **Call Claude** with `Label: "uat"`, `Model: cfg.Models.UAT`,
   `Dir: wtPath`, `SkipPermissions: true`, and
   `DisallowedTools: []string{"AskUserQuestion", "Write", "Edit", "NotebookEdit"}`.
   The UAT session inspects the repo and reports; it must not modify or commit
   anything. `Claude.Call` already persists the result text as
   `<seq>-uat.output.md` under the issue log dir — that is the log file from
   decision 1; no extra file plumbing.
3. **Extract** the checklist with `parseUAT(result)`: the text after
   `uatBeginSentinel`, up to `uatEndSentinel` if present or to the end of the
   result if the session omitted it, trimmed. Missing begin sentinel, or
   an empty body between the sentinels, means "nothing to publish" — log and
   return. This is also how the bug route self-skips when the branch has no
   commits (the prompt instructs the session to emit nothing in that case), so
   no commit-count plumbing is needed inside the pipeline.
4. **Size guard.** Truncate the extracted checklist to 8000 characters. If the
   existing body plus the rendered section would exceed 60000 characters (below
   GitHub's 65536 body limit), skip and log rather than risk a rejected edit.
5. **Append** via `Target.AppendIssueBody`.

Every step logs on the skip path with the issue number and reason, so a missing
UAT is diagnosable from the daemon log.

### Rendered section

`AppendIssueBody` appends a blank line then the rendered `uat-section` template
(a new `define` block in `ai/prompts/comments.md.tmpl`, alongside the other
human-facing outbound text):

```
<!-- loope:uat -->

## 🤖 UAT checklist

<checklist>
```

The HTML comment is the idempotency marker and is invisible in rendered
markdown.

### GitHub methods (`github.go`)

```go
// IssueBody returns just the issue's body, used by the UAT step to detect an
// already-published checklist and to build the appended body.
func (g *GitHub) IssueBody(ctx context.Context, n int) (string, error)

// AppendIssueBody appends text to the issue body via a read-modify-write
// `gh issue edit --body`.
func (g *GitHub) AppendIssueBody(ctx context.Context, n int, text string) error
```

`IssueBody` is `gh issue view N --repo slug --json body`; `AppendIssueBody`
re-reads the body, concatenates, and calls `gh issue edit N --repo slug --body
<new>`. Both go through the existing `g.gh` helper so they inherit the retry
policy. The read-modify-write is not atomic; the loop is the only writer of the
body and the marker check makes a lost update at worst a missing UAT, never a
duplicate one.

### Pipeline wiring

`RunFeaturePipeline` and `RunBugPipeline` each take one new trailing `*UAT`
parameter. `handleIssue` in `loop.go` constructs
`&UAT{Target: o.gh, Num: n}` and passes it to whichever pipeline it runs.

**Feature route** (`pipeline_feature.go`), inside the `parseSpecReady` branch,
between resolving the spec and handing off:

```go
if specPath, ok := resolveSpec(wtPath, rel, start); ok {
    uat.RunFeature(ctx, c, cfg, wtPath, specPath)
    return runPlanThenExecute(ctx, c, cfg, wtPath, specPath, start)
}
```

This is decision 2 taken literally: the checklist describes the behavior the
spec promises, and it is published before any code exists.

**Bug route** (`pipeline_bug.go`), after the confidence and already-done gates
pass — that is, only on the outcome where a fix was actually produced. Neither
`ai-needs-info` nor `already done` reaches it:

```go
uat.RunBug(ctx, c, cfg, wtPath, issueContent, base)
return nil
```

`RunBugPipeline` gains a `base` parameter (the base branch, already available in
`handleIssue`) so the prompt can name `origin/<base>` for the diff.

**Rework** (`rework.go`) does not run the UAT step. The feature route already
published one before the parked session, and the marker check would skip it
anyway.

### Prompts

Two new flat templates in `ai/prompts/`, following the existing one-file-per-
session convention:

- `uat-feature.md.tmpl` — receives `.SpecPath`. Tells the session to read the
  spec and write a UAT checklist for a human verifying the shipped feature.
- `uat-bug.md.tmpl` — receives `.Issue` and `.Base`. Tells the session to read
  the issue detail and inspect `git diff origin/<Base>...HEAD` for what actually
  changed. If that diff is empty, emit nothing at all.

Both share the same output contract, expressed with the sentinels from
`promptData()` (never hardcoded, per the note in `prompts.go`):

- Output *only* the checklist between `UAT_BEGIN` and `UAT_END`, nothing before
  or after.
- Markdown `- [ ]` checkboxes, grouped under short `###` headings when there is
  more than one area.
- Each item is one concrete action a human performs and one observable result.
  No implementation detail, no file paths, no code.
- Short: aim for under 20 items. Cover every behavior in the source material —
  including error and edge cases it specifies — but do not invent scope beyond
  it.
- Do not modify, create, or commit any file.

Two builders in `uat.go` mirror the existing prompt builders:

```go
func uatFeaturePrompt(specPath string) string
func uatBugPrompt(issue, base string) string
```

`promptData()` gains `UATBeginSentinel` and `UATEndSentinel`.

### Config

`Models` gains a `UAT` field carrying the `uat` JSON key. Per decision 5 there is
**no** `uatConfig()` fallback helper — the block is used exactly as written, so
an absent block means the `claude` CLI's own defaults with no budget or turn
cap. `loope.json.example` and `docs/configuration.md` are updated with a
recommended block and a note that this role, unlike `execute`, does not inherit
from `architect`:

```json
"uat": {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 2, "maxTurns": 30}
```

`docs/how-it-works.md` gets a sentence per route describing where the UAT step
sits.

## Error handling

| Condition | Behavior |
|---|---|
| Issue body fetch fails | Skip, log. Pipeline continues. |
| Body already has the marker | Skip, log. Pipeline continues. |
| Claude session errors / hits its budget cap | Skip, log. Pipeline continues. **No** `RecordSession` — the UAT session is ephemeral and must never overwrite the resumable primary session. |
| No `UAT_BEGIN` in the result, or empty content | Skip, log. Pipeline continues. |
| Extracted checklist over 8000 chars | Truncate, then publish. |
| Resulting body over 60000 chars | Skip, log. Pipeline continues. |
| `AppendIssueBody` fails | Log. Pipeline continues. |

The UAT step never produces a park, an `ai-failed`, or a non-nil pipeline error.

## Testing

- `parseUAT`: begin+end present; begin without end (take the remainder); no
  begin; empty content; surrounding prose stripped.
- Idempotency: a fake `UATTarget` whose body already contains the marker records
  zero Claude calls and zero appends; a fake whose `IssueBody` errors likewise.
- Non-blocking: a Claude runner that errors on the `uat` label still lets
  `RunBugPipeline` return nil and `RunFeaturePipeline` proceed to plan/execute.
- Ordering, feature route: with a fake runner scripted through `SPEC_READY`, the
  recorded call labels show `uat` before `plan`.
- Ordering, bug route: `uat` runs after `debug`, and does **not** run on the
  low-confidence or already-done outcomes.
- Session hygiene: the `uat` call does not overwrite the `session` file written
  by the primary session.
- Size guard: an oversized checklist is truncated; an oversized resulting body
  is skipped.
- `GitHub.AppendIssueBody` argument shape, against the existing fake runner.
- Golden prompt tests for `uatFeaturePrompt` and `uatBugPrompt`, matching the
  pattern in `prompts_golden_test.go`.
- Config: `models.uat` round-trips from JSON and is passed through unmodified
  (explicitly asserting no architect inheritance).

## Out of scope

- Surfacing the UAT in the web dashboard or the PR body.
- Re-generating the UAT when a reworked branch diverges from its spec.
- Any UAT for the rework or triage flows.
