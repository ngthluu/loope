# Confidence gate for the bug-fix route

Date: 2026-07-24
Issue: #17

## Problem

The confidence gate runs on the feature route only. `RunFeaturePipeline`
(`pipeline_feature.go`) parses `CONFIDENCE:` from the architect's first
brainstorm turn and, below `cfg.ConfidenceThreshold`, returns
`*lowConfidenceError` without designing anything. The orchestrator turns that
into an `ai-needs-info` escalation.

`RunBugPipeline` (`pipeline_bug.go`) has no such gate. It makes one `debug`
call, inspects the result for `PIPELINE_ALREADY_DONE`, and otherwise ships
whatever the session produced. A vague bug report ("crashes sometimes on
startup") therefore gets a guessed fix and a PR, where the same vagueness in a
feature request would get pushed back to the author.

The orchestrator half of the plumbing is already kind-agnostic: `loop.go`
matches `*lowConfidenceError` from either pipeline and routes it to
`finishNeedsInfo`, and the `needs-info` comment template is worded without
reference to brainstorming. Only the bug pipeline's prompt and its
result-inspection are missing.

## Goals

- A bug report scored below `confidenceThreshold` is escalated to
  `ai-needs-info` with the session's questions, and no code is written.
- The bug route reuses the feature route's threshold, sentinel, parser, error
  type, and terminal outcome. No parallel mechanism.
- `confidenceThreshold: 0` disables the gate on both routes.

## Non-goals

- A separate threshold for bugs. One setting governs both routes.
- Changing the feature route's behavior.
- Re-scoring on `loop -rework`. A resumed session is not re-gated, matching the
  feature route.

## Design

### 1. Scoring happens after read-only investigation

The feature prompt scores "before anything else", from the issue text alone.
That phrasing is wrong for a bug: reports are terse by nature, and confidence
in a fix is a function of the code, not the prose. Scoring blind would escalate
bugs that are trivial once you open the file.

So the debug prompt permits investigation before scoring, and forbids mutation
until after:

> You may read the codebase first to investigate — but do NOT write code,
> tests, or commits yet.

The cost is tokens spent reading on reports that then get rejected. That is
accepted: a meaningful score is worth more than a cheap one, and the
alternative produces false escalations that cost a human round-trip.

### 2. `debug.md.tmpl` gains a threshold block

`bugPrompt` changes signature to `bugPrompt(issue string, threshold int)`,
mirroring `brainstormPrompt`, and passes `Threshold` into the template. The
template gains a `{{if gt .Threshold 0}}` block placed immediately after the
`/superpowers:systematic-debugging {{.Issue}}` line — the same position the
block occupies in `brainstorm.md.tmpl` — instructing the session to:

- investigate read-only first, writing nothing;
- print `{{.ConfidenceSentinel}} <0-100>` as the first line of its reply,
  scoring how confidently the bug can be fixed as reported;
- if below `{{.Threshold}}`, change no file, list what is missing and the
  specific questions the author must answer, and stop.

The sentinel is injected from `promptData()`, never hardcoded, so the
instruction and `parseConfidence` cannot drift.

When the threshold is `0` the block renders away entirely and the prompt is
byte-identical to today's.

### 3. `RunBugPipeline` gains the gate

After the existing record-session and error checks, and **before** the
already-done check:

```go
if cfg.ConfidenceThreshold > 0 {
    if score, ok := parseConfidence(res.Result); ok && score < cfg.ConfidenceThreshold {
        return &lowConfidenceError{score: score, feedback: stripConfidenceLine(res.Result)}
    }
}
```

Two properties, both inherited deliberately from the feature route:

- **Fail open.** An absent or unparseable score (`ok == false`) proceeds. A
  session that forgot the sentinel but fixed the bug still ships.
- **Confidence outranks already-done.** The gate runs first, so a session that
  is not confident enough to fix the bug cannot also close the issue as already
  implemented. A low score plus `PIPELINE_ALREADY_DONE` in the same output
  escalates rather than closes.

The session is recorded before all of this, unchanged, so `loop -rework` still
works after an errored call.

### 4. Orchestrator: no change

`loop.go:189-192` already matches `*lowConfidenceError` regardless of `kind`
and calls `finishNeedsInfo`, which removes the worktree and branch, comments
the score and questions, swaps `ai-wip` → `ai-needs-info`, records state, and
returns `nil`. No park cause is recorded, so auto-resume never picks the issue
up; it waits until a human removes the label.

Worktree removal is correct for a low-confidence bug too: the prompt forbids
writing, and anything a non-compliant session wrote is discarded with the
worktree rather than shipped.

The `needs-info` comment ("🤖 Not confident enough to implement (confidence
N/100)…") is already route-neutral and needs no change.

## Error handling

| Situation | Behavior |
|---|---|
| Score < threshold | `*lowConfidenceError` → `ai-needs-info`, no PR, issue stays open |
| Score ≥ threshold | Proceeds exactly as today |
| Sentinel absent / unparseable | Fails open, proceeds |
| `confidenceThreshold: 0` | Block omitted from prompt, gate skipped |
| Call errors (e.g. 429) | Unchanged: session recorded, error propagated, issue parked |
| Low score **and** already-done | Escalates (gate runs first) |

## Testing

New cases in `pipeline_bug_test.go`, modeled on
`TestFeaturePipelineLowConfidenceEscalates` / `…HighConfidenceProceeds`:

- low score returns `*lowConfidenceError` with the right score, feedback with
  the `CONFIDENCE:` line stripped, and exactly one Claude call;
- score at or above threshold proceeds to a normal success;
- `ConfidenceThreshold: 0` ignores even a low score in the output;
- output with no sentinel fails open;
- low score together with `PIPELINE_ALREADY_DONE` returns
  `*lowConfidenceError`, not `*alreadyDoneError`.

`prompts_golden_test.go` gains full-text goldens for `bugPrompt(threshold=70)`
and `bugPrompt(threshold=0)`, matching the existing brainstorm pair. The
`threshold=0` golden pins the no-regression claim: it must equal today's debug
prompt.

`loop_test.go` gains one end-to-end case: a `kind == "bug"` issue whose debug
session returns a low score gets `ai-needs-info`, no PR, and an open issue.

Existing bug-pipeline tests construct `&Config{…}` without a threshold, so the
zero value disables the gate and they pass unmodified — an intentional check
that the default-off path is untouched.

## Documentation

- `README.md` "Confidence gate" section: state that the gate runs on both
  routes, and that the bug route scores after read-only investigation while the
  feature route scores from the issue text.
- `README.md` config table: reword `confidenceThreshold` from "Brainstorm
  confidence" to cover both routes.
