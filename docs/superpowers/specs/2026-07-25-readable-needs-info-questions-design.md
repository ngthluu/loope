# Readable clarifying questions on a needs-info escalation

Date: 2026-07-25
Issue: #31

## Problem

When a session scores below `confidenceThreshold`, the pipeline stops and asks
the author for more information. That escalation is the only moment in the whole
loop where a human is required to read what the model wrote and act on it — and
it is the one piece of model output the prompts give no format guidance for.

Both routes say the same thing today:

- `ai/prompts/brainstorm.md.tmpl:6-7` — "Instead, list what is missing and the
  specific questions the author must answer, then stop."
- `ai/prompts/debug.md.tmpl:11-12` — "Instead, list what is missing and the
  specific questions the author must answer, then stop."

The result is whatever prose the architect happens to produce: a recap of the
issue, an account of what it explored in the codebase, headed sections of
"missing information", questions buried mid-paragraph and phrased as design
discussion. `sanitizeFeedback` strips only the sentinel lines
(`confidence.go:62`), so all of that reaches GitHub verbatim through the
`needs-info` comment (`loop.go:334`, `ai/prompts/comments.md.tmpl:5`).

The author sees a wall of text where they needed a short list of decisions to
make. The escalation is correct and still fails at its job, because a comment
nobody finishes reading does not get answered — and an unanswered needs-info
issue sits out of the queue indefinitely.

## Goals

- The needs-info comment reads as a skimmable list of questions a human can
  answer in one reply.
- No information is lost: every ambiguity that drove the score down is still
  answerable from the questions posted.
- Both routes produce the same shape, from one shared source of instruction, so
  they cannot drift.
- The author is told explicitly how to reply and how to re-queue the issue.

## Non-goals

- Changing when the gate fires, the threshold, the sentinel, the parser, or the
  terminal outcome. This changes the wording of the reply only.
- Post-processing or reformatting the model's output in Go. `sanitizeFeedback`
  keeps its current job — sentinel removal — and gains nothing. The prompt is
  the right lever: shaping text is what the model is for, and a Go reformatter
  would have to parse free prose to do it.
- Touching the above-threshold path. A session that scores high proceeds to the
  spec exactly as it does today.
- Changing the `park`, `already-done`, or PR comment templates.

## Design

### One shared instruction block

Add `ai/prompts/ask-format.md.tmpl` holding a single
`{{define "ask-format"}}…{{end}}` block. Both `brainstorm.md.tmpl` and
`debug.md.tmpl` include it with `{{template "ask-format" .}}`, inside their
existing `{{if gt .Threshold 0}}` guard — the instruction is unreachable when
the gate is off, and rendering it there would be noise in the prompt.

This mirrors the container-file pattern `comments.md.tmpl` already uses: a file
whose own body is just whitespace between `{{define}}` blocks. Two consequences
in `prompts_test.go` follow from that and are part of this work:

- `skipTemplates` gains `"ask-format.md.tmpl"` — the container's own body
  renders empty, as `comments.md.tmpl`'s does.
- `promptTestData` gains an `"ask-format"` entry (`{}` — the block reads no
  keys), so `TestEveryTemplateRenders` covers it.

The alternative — duplicating the format text into both prompt files — was
rejected for the reason the sentinel constants are injected rather than typed
into the templates: two copies of one instruction drift, and the drift is
invisible until a human notices the two routes ask differently.

### The instruction

The block instructs the session to write its low-confidence reply as:

- **One opening sentence** naming the single thing that blocks it most.
- **A numbered list of questions**, ordered most-blocking first, one sentence
  each, each ending in a question mark.
- **Inline options where they are guessable**, written `a) … b) … c) …`, so the
  author can answer `1a, 2c` instead of composing prose.
- **Under 200 words total.**
- **At most 5 questions — by merging, never by dropping.** If more gaps exist
  than that, related gaps combine into one question. This is the rule that lets
  "short" and "100% content" hold at once: the cap bounds the reading, merging
  preserves the coverage. The block states the invariant directly — every
  ambiguity that drove the score down must be answerable from the list.
- **Nothing else**: no preamble, no restatement of the issue, no account of what
  the session read or explored, no code blocks, no closing pleasantries. These
  are the specific failure modes observed in the current output, so they are
  named specifically rather than covered by a general "be brief".

Markdown is intended and correct here — the destination is a GitHub comment, so
a numbered list renders as one.

### The comment wrapper

`ai/prompts/comments.md.tmpl`'s `needs-info` block currently reads "Please
clarify and remove the `<label>` label to re-queue:". It becomes an explicit
two-step instruction — answer the numbered questions in a comment, then remove
the label to re-queue — matching the numbered format the questions now arrive
in. The score line and the `{{.Feedback}}` placement are unchanged.

## Data flow

Unchanged end to end. `RunFeaturePipeline` / `RunBugPipeline` parse the score,
build `*lowConfidenceError{feedback: sanitizeFeedback(output)}`, `loop.go`
matches it and calls `finishNeedsInfo`, which comments
`needsInfoComment(score, label, feedback)` and swaps in the needs-info label.
Only the text flowing through that path changes shape.

## Testing

- **Golden tests** (`prompts_golden_test.go`) are byte-exact, so
  `TestGoldenBrainstormPromptWithThreshold`, `TestGoldenBugPromptWithThreshold`,
  and `TestGoldenNeedsInfoComment` are updated to the new expected strings. The
  two `WithoutThreshold` goldens must come through **unchanged** — that is the
  assertion that the new block stayed inside the threshold guard.
- **A test that both routes carry the same block**: render `brainstormPrompt`
  and `bugPrompt` at a non-zero threshold and assert both contain the rendered
  `ask-format` text. This is what catches a future edit to one prompt that
  should have been an edit to the shared block.
- **Existing structural tests** must keep passing without weakening:
  `TestEveryPromptFileOnDiskIsParsed` (the new file is flat and parsed),
  `TestEveryTemplateRenders` (via the `promptTestData` entry above), and
  `TestNoSentinelIsHardcodedInATemplate` — the new block must not spell any
  sentinel literally.

No test asserts on real model output; the gate's behavior is already covered by
`confidence_test.go` and the pipeline tests, and none of it changes.

## Risks

The instruction constrains a model, so compliance is best-effort — a session may
still exceed 200 words or write a preamble. The failure mode is the status quo
(a comment that is longer than ideal), not a broken pipeline: nothing in Go
parses the feedback beyond sentinel removal, so a non-conforming reply still
escalates correctly. This is the reason the design puts no length validation in
Go — a truncating or rejecting validator would trade a verbose comment for a
lost question, which is the worse outcome.

## Assumptions

Stated because the issue does not specify them; each was chosen to serve "easy
to read, short, 100% context":

1. The 200-word ceiling and 5-question cap are the concrete numbers behind
   "short". They are prompt text, so tuning them later is a one-line edit.
2. Questions are merged rather than dropped when they exceed the cap — the
   reading of "100% content" that keeps the escalation answerable.
3. Inline `a) b) c)` options are offered only where the session can guess
   plausible answers, not forced onto every question.
4. The change applies to both the feature and bug routes. They share the gate,
   the error type, and the comment; asking differently would be an accident, not
   a design.
