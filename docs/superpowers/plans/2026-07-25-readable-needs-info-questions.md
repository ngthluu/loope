# Readable needs-info clarifying questions — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the `needs-info` escalation comment a short, skimmable, numbered list of questions the author can answer in one reply, driven from one shared prompt block used by both the feature and bug routes.

**Architecture:** Add a new container template `ai/prompts/ask-format.md.tmpl` holding a single `{{define "ask-format"}}…{{end}}` block that tells the session how to shape its low-confidence reply. `brainstorm.md.tmpl` and `debug.md.tmpl` include it with `{{template "ask-format" .}}` **inside** their existing `{{if gt .Threshold 0}}` guard, so the instruction never renders when the gate is off. `comments.md.tmpl`'s `needs-info` block is reworded into an explicit two-step instruction. No Go logic changes at all — only `.tmpl` files and tests.

**Tech Stack:** Go 1.x, `text/template` with `Option("missingkey=error")`, `embed.FS`, standard `go test`. Repo root is the Go package `main`; there is no `src/` layout and no test framework beyond the stdlib.

## Global Constraints

- **No Go source file changes.** `sanitizeFeedback` (`confidence.go:62`), `needsInfoComment` (`loop.go:730`), `brainstormPrompt` (`pipeline_feature.go:258`), `bugPrompt` (`pipeline_bug.go:39`) and the whole data flow stay exactly as they are. Only `ai/prompts/*.md.tmpl` and `*_test.go` files change.
- **`ai/prompts/` stays flat.** Neither the `//go:embed ai/prompts/*.md.tmpl` pattern nor the `ParseFS` glob descends; a nested file ships unparsed. Enforced by `TestEveryPromptFileOnDiskIsParsed`.
- **No sentinel string may appear literally in any template.** Sentinels are injected via `promptData()`. Enforced by `TestNoSentinelIsHardcodedInATemplate`. The new block must not contain `CONFIDENCE:`, `SPEC_READY:`, `PIPELINE_READY`, `PIPELINE_ALREADY_DONE:`, or `DONE_CONFIRMED`.
- **The two `WithoutThreshold` goldens must stay byte-identical.** `TestGoldenBrainstormPromptWithoutThreshold` and `TestGoldenBugPromptWithoutThreshold` are the assertion that the new block stayed inside the threshold guard. Do not edit them.
- **The instruction's concrete numbers, verbatim from the spec:** at most **5 questions**, under **200 words** total, merge-never-drop when over the cap.
- **Markdown is intended** in the block's output shape — the destination is a GitHub comment.
- **Every template must have a `promptTestData` entry or be in `skipTemplates`**, or `TestEveryTemplateRenders` fails.
- Run tests from the repository root with `go test .` (the module is a single package).

## Assumptions made while writing this plan

1. **Exact block wording.** The spec specifies the six rules but not their prose. This plan fixes the wording (Task 1) and every golden string in later tasks is byte-consistent with it. The strings below were produced by rendering the real templates, not written by hand — if you change one word of the block, the two threshold goldens in Task 2 must be regenerated to match.
2. **Placement:** the include goes at the *end* of each threshold guard, separated from the preceding text by one blank line, so it reads as a formatting appendix to the "then stop" instruction rather than interrupting the scoring instructions.
3. **The em-dash/ellipsis characters matter.** The block uses `…` (U+2026) in the `a) … b) … c) …` example and `—` (U+2014) in the merge rule. Copy the code blocks in this plan verbatim; a substituted `...` breaks the byte-exact goldens.
4. **The shared-block test lives in `prompts_test.go`** (structural/behavioral test about the prompt set), not `prompts_golden_test.go` (byte-exact goldens).

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `ai/prompts/ask-format.md.tmpl` | **Create** | Container file; holds only the `{{define "ask-format"}}` block — the single source of the reply-format instruction. |
| `ai/prompts/brainstorm.md.tmpl` | Modify (inside `{{if gt .Threshold 0}}`) | Feature route prompt; now includes the shared block. |
| `ai/prompts/debug.md.tmpl` | Modify (inside `{{if gt .Threshold 0}}`) | Bug route prompt; now includes the shared block. |
| `ai/prompts/comments.md.tmpl` | Modify (`needs-info` block only) | Human-facing comment wrapper; explicit two-step reply instruction. |
| `prompts_test.go` | Modify | `skipTemplates` + `promptTestData` entries; new both-routes-share-the-block test. |
| `prompts_golden_test.go` | Modify | Three golden strings updated; two left untouched on purpose. |

---

### Task 1: The shared `ask-format` block

Create the new template file and wire it into the test harness so it is parsed, rendered, and sentinel-checked — before any prompt includes it. At the end of this task the block exists and is covered, but no prompt uses it yet.

**Files:**
- Create: `ai/prompts/ask-format.md.tmpl`
- Modify: `prompts_test.go:13-33` (`promptTestData`), `prompts_test.go:35-38` (`skipTemplates`)
- Test: `prompts_test.go`

**Interfaces:**
- Consumes: `mustRender(name string, data map[string]any) string` and `promptData() map[string]any` from `prompts.go`; the package-level `prompts *template.Template`.
- Produces: a template named `ask-format` renderable with no data keys (`mustRender("ask-format", promptData())`), and a container template named `ask-format.md.tmpl` whose own body renders empty. Task 2 depends on both names.

- [ ] **Step 1: Write the failing test**

Add to `prompts_test.go`, after `TestEveryTemplateRenders`:

```go
// The reply-format instruction lives in exactly one place. This asserts the
// block exists and still carries the rules that make the needs-info comment
// readable — a silent deletion of one bullet would otherwise pass every other
// test in this file.
func TestAskFormatBlockCarriesItsRules(t *testing.T) {
	got := mustRender("ask-format", promptData())
	for _, want := range []string{
		"numbered list of questions",
		"At most 5 questions",
		"MERGE related gaps",
		"Under 200 words",
		"no preamble",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ask-format block is missing %q:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestAskFormatBlockCarriesItsRules .`
Expected: FAIL — a panic from `mustRender`: `render prompt "ask-format": template: no template "ask-format" associated with template "prompts"` (the template does not exist yet).

- [ ] **Step 3: Create the template file**

Create `ai/prompts/ask-format.md.tmpl` with exactly this content (one trailing newline, no other blank lines):

```
{{define "ask-format"}}Write that reply as a short, skimmable list the author can answer in one comment:
- Open with ONE sentence naming the single thing that blocks you most.
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Where plausible answers are guessable, offer them inline as `a) … b) … c) …` so the author can reply "1a, 2c".
- At most 5 questions. If more gaps exist, MERGE related gaps into one question — never drop one. Every ambiguity that lowered the score must stay answerable from the list.
- Under 200 words total.
- Nothing else: no preamble, no restatement of the issue, no account of what you read or explored, no code blocks, no closing pleasantries.{{end}}
```

Note the `{{define}}` opens on the same line as the first sentence and `{{end}}` closes on the same line as the last bullet: that is what keeps the rendered block free of leading/trailing blank lines, exactly as the blocks in `comments.md.tmpl` do it.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -run TestAskFormatBlockCarriesItsRules .`
Expected: PASS

- [ ] **Step 5: Register the container file and the block with the harness**

Two edits in `prompts_test.go`. First, add the block's (empty) test data — insert after the `"guidance-network": {},` line inside `promptTestData`:

```go
	"ask-format":           {},
```

Second, replace the `skipTemplates` declaration and its comment:

```go
// skipTemplates are the names in the set that are not prompts: the root
// template ParseFS was seeded with, and the container files whose own bodies
// are just the whitespace between their {{define}} blocks.
var skipTemplates = map[string]bool{
	"prompts":            true,
	"comments.md.tmpl":   true,
	"ask-format.md.tmpl": true,
}
```

- [ ] **Step 6: Run the full prompt test set**

Run: `go test -run 'TestEveryTemplateRenders|TestEveryPromptFileOnDiskIsParsed|TestNoSentinelIsHardcodedInATemplate|TestAskFormat' -v .`
Expected: all PASS. Specifically `TestEveryPromptFileOnDiskIsParsed` proves the new file was embedded and parsed (it walks the real directory, so a forgotten `go:embed` update would fail here), and `TestNoSentinelIsHardcodedInATemplate` proves the block spells no sentinel.

- [ ] **Step 7: Run the whole suite**

Run: `go test .`
Expected: ok — nothing else changed yet, so every golden still passes.

- [ ] **Step 8: Commit**

```bash
git add ai/prompts/ask-format.md.tmpl prompts_test.go
git commit -m "feat(prompts): add shared ask-format block for low-confidence replies"
```

---

### Task 2: Include the block in both routes

Wire the shared block into the feature and bug prompts, inside the threshold guard, and lock both rendered prompts down with goldens plus a test that they carry the *same* block.

**Files:**
- Modify: `ai/prompts/brainstorm.md.tmpl` (inside `{{if gt .Threshold 0}}`), `ai/prompts/debug.md.tmpl` (inside `{{if gt .Threshold 0}}`)
- Modify: `prompts_golden_test.go:16-37` (`TestGoldenBrainstormPromptWithThreshold`), `prompts_golden_test.go:112-136` (`TestGoldenBugPromptWithThreshold`)
- Test: `prompts_test.go` (new shared-block test), `prompts_golden_test.go`

**Interfaces:**
- Consumes: the `ask-format` template from Task 1; `brainstormPrompt(issue string, threshold int) string` (`pipeline_feature.go:258`) and `bugPrompt(issue string, threshold int) string` (`pipeline_bug.go:39`), both unchanged; `check(t *testing.T, name, got, want string)` from `prompts_golden_test.go:9`.
- Produces: nothing new for later tasks — Task 3 is independent of this one.

- [ ] **Step 1: Write the failing test**

Add to `prompts_test.go`, after `TestAskFormatBlockCarriesItsRules`:

```go
// Both routes must ask in the same shape, from the same source. This is what
// catches an edit made to one prompt that should have been an edit to the
// shared block — and, via the threshold=0 cases, that the instruction stays
// inside the gate's guard.
func TestBothRoutesShareTheAskFormatBlock(t *testing.T) {
	block := mustRender("ask-format", promptData())
	if !strings.Contains(brainstormPrompt("I", 70), block) {
		t.Error("brainstormPrompt(threshold=70) does not contain the ask-format block")
	}
	if !strings.Contains(bugPrompt("I", 70), block) {
		t.Error("bugPrompt(threshold=70) does not contain the ask-format block")
	}
	if strings.Contains(brainstormPrompt("I", 0), block) {
		t.Error("brainstormPrompt(threshold=0) contains the ask-format block; it must stay inside the threshold guard")
	}
	if strings.Contains(bugPrompt("I", 0), block) {
		t.Error("bugPrompt(threshold=0) contains the ask-format block; it must stay inside the threshold guard")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestBothRoutesShareTheAskFormatBlock -v .`
Expected: FAIL with both "does not contain the ask-format block" errors (the two threshold=0 assertions already hold).

- [ ] **Step 3: Include the block in `brainstorm.md.tmpl`**

In `ai/prompts/brainstorm.md.tmpl`, replace:

```
the specific questions the author must answer, then stop.
{{end}}
```

with:

```
the specific questions the author must answer, then stop.

{{template "ask-format" .}}
{{end}}
```

- [ ] **Step 4: Include the block in `debug.md.tmpl`**

In `ai/prompts/debug.md.tmpl`, replace:

```
print another sentinel and stop.
{{end}}
```

with:

```
print another sentinel and stop.

{{template "ask-format" .}}
{{end}}
```

The include goes after the "`{{.ConfidenceSentinel}}` line comes first" sentence — i.e. last inside the guard, mirroring `brainstorm.md.tmpl`.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test -run TestBothRoutesShareTheAskFormatBlock -v .`
Expected: PASS

- [ ] **Step 6: Watch the goldens fail, then update the two threshold ones**

Run: `go test -run TestGolden .`
Expected: FAIL — `brainstormPrompt(threshold=70) mismatch` and `bugPrompt(threshold=70) mismatch`. The two `WithoutThreshold` goldens must **not** appear in the failure output; if either does, the include landed outside the guard — fix the template, do not touch those tests.

In `prompts_golden_test.go`, replace the whole `want` literal of `TestGoldenBrainstormPromptWithThreshold` with:

```go
	want := `/superpowers:brainstorming ISSUE BODY

Before anything else, assess how confidently this issue can be implemented as
written and print CONFIDENCE: <0-100> as the FIRST line of your reply. If that score is
below 70, the issue is too under-specified or ambiguous to implement
responsibly: do NOT design or write a spec. Instead, list what is missing and
the specific questions the author must answer, then stop.

Write that reply as a short, skimmable list the author can answer in one comment:
- Open with ONE sentence naming the single thing that blocks you most.
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Where plausible answers are guessable, offer them inline as ` + "`a) … b) … c) …`" + ` so the author can reply "1a, 2c".
- At most 5 questions. If more gaps exist, MERGE related gaps into one question — never drop one. Every ambiguity that lowered the score must stay answerable from the list.
- Under 200 words total.
- Nothing else: no preamble, no restatement of the issue, no account of what you read or explored, no code blocks, no closing pleasantries.

HEADLESS MODE: your interlocutor is an automated product-owner agent, not a human.
Ask clarifying questions as plain text (AskUserQuestion is disabled).
Follow the brainstorming flow to a committed spec: clarifying questions, design,
then write and commit the spec document into this branch. Do NOT invoke the
writing-plans skill — a separate session writes the implementation plan.
When the spec file is written and committed, print SPEC_READY: <path> on its own line,
where <path> is the spec file path relative to the repository root.

If during brainstorming you determine the feature is already fully implemented
in this codebase, do not invent work: print PIPELINE_ALREADY_DONE: <one-sentence reason> on its own
line instead of continuing.`
```

The `` ` + "`a) … b) … c) …`" + ` `` splice is required: the golden is a raw string literal and the block contains backticks, which cannot be escaped inside one. Keep the rest of the test function (the `check(...)` call) as it is.

- [ ] **Step 7: Update the bug golden**

In `prompts_golden_test.go`, replace the whole `want` literal of `TestGoldenBugPromptWithThreshold` with:

```go
	want := `/superpowers:systematic-debugging ISSUE BODY

You may read the codebase first to investigate — but do NOT write code, tests,
or commits yet. Once you understand the failure, assess how confidently this bug
can be fixed as reported and print CONFIDENCE: <0-100> as the FIRST line of your reply.
Score the report, not the repair: a bug described precisely enough to act on
scores high however large the fix, and it still scores high when investigation
shows the behavior is already correct — that is a finding about the code, not a
gap in the report. Score low only when you cannot tell what behavior is wrong.
If that score is below 70, the report is too vague or ambiguous to fix
responsibly: change no file. Instead, list what is missing and the specific
questions the author must answer, then stop.
The CONFIDENCE: line comes first even when an instruction below tells you to
print another sentinel and stop.

Write that reply as a short, skimmable list the author can answer in one comment:
- Open with ONE sentence naming the single thing that blocks you most.
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Where plausible answers are guessable, offer them inline as ` + "`a) … b) … c) …`" + ` so the author can reply "1a, 2c".
- At most 5 questions. If more gaps exist, MERGE related gaps into one question — never drop one. Every ambiguity that lowered the score must stay answerable from the list.
- Under 200 words total.
- Nothing else: no preamble, no restatement of the issue, no account of what you read or explored, no code blocks, no closing pleasantries.

Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
PIPELINE_ALREADY_DONE: <one-sentence reason> on its own line and stop.`
```

- [ ] **Step 8: Run the goldens to verify they pass**

Run: `go test -run TestGolden -v .`
Expected: all PASS, including both `WithoutThreshold` tests, which were not edited.

- [ ] **Step 9: Run the whole suite**

Run: `go test ./...`
Expected: ok

- [ ] **Step 10: Commit**

```bash
git add ai/prompts/brainstorm.md.tmpl ai/prompts/debug.md.tmpl prompts_test.go prompts_golden_test.go
git commit -m "feat(prompts): ask both routes for a numbered clarifying-question list"
```

---

### Task 3: Two-step reply instruction in the needs-info comment

The questions now arrive numbered; the comment wrapper should tell the author exactly what to do with them. Independent of Task 2 — this is the human-facing half.

**Files:**
- Modify: `ai/prompts/comments.md.tmpl` (the `needs-info` block only)
- Modify: `prompts_golden_test.go:188-191` (`TestGoldenNeedsInfoComment`)
- Test: `prompts_golden_test.go`

**Interfaces:**
- Consumes: `needsInfoComment(score int, label, feedback string) string` (`loop.go:730`), unchanged.
- Produces: nothing consumed by other tasks.

- [ ] **Step 1: Write the failing test**

In `prompts_golden_test.go`, replace `TestGoldenNeedsInfoComment` with:

```go
func TestGoldenNeedsInfoComment(t *testing.T) {
	check(t, "needsInfoComment", needsInfoComment(42, "ai-needs-info", "Which database?"),
		"🤖 Not confident enough to implement (confidence 42/100). Answer the numbered questions below in a comment, then remove the `ai-needs-info` label to re-queue:\n\nWhich database?")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -run TestGoldenNeedsInfoComment -v .`
Expected: FAIL with `needsInfoComment mismatch` — got still says "Please clarify and remove the ...".

- [ ] **Step 3: Reword the `needs-info` block**

In `ai/prompts/comments.md.tmpl`, replace the first line of the `needs-info` define:

```
{{define "needs-info"}}🤖 Not confident enough to implement (confidence {{.Score}}/100). Please clarify and remove the `{{.Label}}` label to re-queue:
```

with:

```
{{define "needs-info"}}🤖 Not confident enough to implement (confidence {{.Score}}/100). Answer the numbered questions below in a comment, then remove the `{{.Label}}` label to re-queue:
```

The blank line and the `{{.Feedback}}` line that follow are unchanged, as are every other block in the file (`park`, `already-done`, `pr-*`, `guidance-*`).

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -run TestGoldenNeedsInfoComment -v .`
Expected: PASS

- [ ] **Step 5: Run the whole suite**

Run: `go test ./...`
Expected: ok — in particular `loop_test.go` and the pipeline tests, which exercise `finishNeedsInfo`, must be green; if one asserts on the old sentence, update that assertion to the new wording (the wording, not the behavior, is what changed).

- [ ] **Step 6: Commit**

```bash
git add ai/prompts/comments.md.tmpl prompts_golden_test.go
git commit -m "feat(prompts): tell the author how to answer and re-queue a needs-info issue"
```

---

## Verification checklist (after all three tasks)

- [ ] `go build ./...` — the embedded FS still compiles.
- [ ] `go test ./...` — everything green.
- [ ] `git diff main --stat` shows **no `.go` file outside `*_test.go`** and no file outside `ai/prompts/`, `prompts_test.go`, `prompts_golden_test.go`, `docs/superpowers/`.
- [ ] `TestGoldenBrainstormPromptWithoutThreshold` and `TestGoldenBugPromptWithoutThreshold` are untouched in the diff — the proof the instruction stayed behind the gate.
