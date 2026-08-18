# Simpler clarifying questions and grouped UAT checklist Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a plain-language rule to the shared `ask-format` prompt block and replace `uat-format`'s single flat list with exactly two fixed groups (`### Happy path`, `### Edge cases`), keeping every other rule in both blocks unchanged.

**Architecture:** Both blocks are Go `text/template` definitions in `ai/prompts/*.md.tmpl`, rendered via `mustRender`/`renderTemplate` in this repo's root-package templating code and shared by the brainstorm/debug prompts (`ask-format`) and the feature/bug UAT prompts (`uat-format`). This is a text-only change: edit the two template files, then update the golden and format-block tests that pin their exact rendered text, then update one doc sentence.

**Tech Stack:** Go 1.25, `text/template`, `go test` (table-driven / golden string tests).

**Spec:** `docs/superpowers/specs/2026-08-19-simpler-questions-and-grouped-uat-design.md`

## Global Constraints

- `ask-format` keeps every existing rule (one sentence per question, `a) b) c)` guessable answers, 5-question cap, 200-word cap, no-preamble rule) — only one new rule is added.
- The new `ask-format` rule text is exactly: `- Write each question in short, plain sentences: common words, one idea per sentence, no jargon.` — placed immediately after the one-sentence-each rule line.
- `uat-format` uses exactly two group headings, in this fixed order: `### Happy path` then `### Edge cases`. No other headings, no intro/closing line. A group with zero items is omitted entirely — never invent items to fill it.
- UAT item shape is unchanged: `Action → expected result`, 15 words or fewer, not a sentence, no implementation detail/file paths/code, one item per behavior, compress wording never coverage, never modify/create/commit a file.
- No new enforcement in Go code (no length/shape/heading validation) — the prompt text is the only mechanism, matching the #44 decision this spec explicitly preserves.
- `maxUATChars`, `parseUAT`, `uatSection`, the `## 🤖 UAT checklist` heading, the confidence-gate sentinels, and the pipeline's non-blocking UAT contract are all out of scope — do not touch them.

---

### Task 1: Add the plain-language rule to `ask-format`

**Files:**
- Modify: `ai/prompts/ask-format.md.tmpl`
- Modify: `prompts_test.go` (`TestAskFormatBlockCarriesItsRules`, ~line 80)
- Modify: `prompts_golden_test.go` (`TestGoldenBrainstormPromptWithThreshold` ~line 16, `TestGoldenBugPromptWithThreshold` ~line 120)

**Interfaces:**
- Consumes: existing `mustRender("ask-format", promptData())` helper and `brainstormPrompt(issue string, threshold int) string` / `bugPrompt(issue string, threshold int) string` functions already defined in this package (no signature changes).
- Produces: nothing new is consumed by later tasks — `ask-format` and `uat-format` are independent template blocks (Task 1 and Task 2 do not depend on each other and could be done in either order, but are listed sequentially here).

The current file (`ai/prompts/ask-format.md.tmpl`) reads:

```
{{define "ask-format"}}Write that reply as a short, skimmable list the author can answer in one comment:
- Open with ONE sentence naming the single thing that blocks you most.
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Where plausible answers are guessable, offer them inline as `a) … b) … c) …` so the author can reply "1a, 2c".
- At most 5 questions. If more gaps exist, MERGE related gaps into one question — never drop one. Every ambiguity that lowered the score must stay answerable from the list.
- Under 200 words total.
- Nothing else: no preamble, no restatement of the issue, no account of what you read or explored, no code blocks, no closing pleasantries.{{end}}
```

- [ ] **Step 1: Update the failing golden tests first (TDD for the rendered text)**

In `prompts_golden_test.go`, in `TestGoldenBrainstormPromptWithThreshold` (~line 27) and `TestGoldenBugPromptWithThreshold` (~line 138), insert a new line immediately after the "Then a numbered list of questions..." line and before the "Where plausible answers are guessable..." line, in both `want` blocks:

```go
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Write each question in short, plain sentences: common words, one idea per sentence, no jargon.
- Where plausible answers are guessable, offer them inline as ` + "`a) … b) … c) …`" + ` so the author can reply "1a, 2c".
```

Apply this same three-line ordering (insert the new line in the middle) to both `want` string literals.

- [ ] **Step 2: Run the golden tests to verify they fail**

Run: `go test ./... -run TestGoldenBrainstormPromptWithThreshold -v` and `go test ./... -run TestGoldenBugPromptWithThreshold -v`
Expected: both FAIL, diff shows the `want` string now has an extra plain-language line the actual rendered output doesn't have yet.

- [ ] **Step 3: Add the assertion to the format-block test**

In `prompts_test.go`, `TestAskFormatBlockCarriesItsRules` (~line 80-93), add `"Write each question in short, plain sentences"` to the `want` slice:

```go
func TestAskFormatBlockCarriesItsRules(t *testing.T) {
	got := mustRender("ask-format", promptData())
	for _, want := range []string{
		"numbered list of questions",
		"Write each question in short, plain sentences",
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

- [ ] **Step 4: Run it to verify it fails too**

Run: `go test ./... -run TestAskFormatBlockCarriesItsRules -v`
Expected: FAIL — rendered block does not yet contain `"Write each question in short, plain sentences"`.

- [ ] **Step 5: Edit the template**

Update `ai/prompts/ask-format.md.tmpl` to insert the new rule line right after the one-sentence-each rule:

```
{{define "ask-format"}}Write that reply as a short, skimmable list the author can answer in one comment:
- Open with ONE sentence naming the single thing that blocks you most.
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Write each question in short, plain sentences: common words, one idea per sentence, no jargon.
- Where plausible answers are guessable, offer them inline as `a) … b) … c) …` so the author can reply "1a, 2c".
- At most 5 questions. If more gaps exist, MERGE related gaps into one question — never drop one. Every ambiguity that lowered the score must stay answerable from the list.
- Under 200 words total.
- Nothing else: no preamble, no restatement of the issue, no account of what you read or explored, no code blocks, no closing pleasantries.{{end}}
```

- [ ] **Step 6: Run the three tests to verify they pass**

Run: `go test ./... -run 'TestAskFormatBlockCarriesItsRules|TestGoldenBrainstormPromptWithThreshold|TestGoldenBugPromptWithThreshold' -v`
Expected: PASS for all three.

- [ ] **Step 7: Run the full package test suite to check for other regressions**

Run: `go test ./...`
Expected: PASS. (`TestGoldenBrainstormPromptWithoutThreshold` and `TestGoldenBugPromptWithoutThreshold` render without the `ask-format` block at all — threshold 0 — so they are unaffected; `TestBothRoutesShareTheAskFormatBlock` re-derives its expected block from `mustRender` so it stays correct automatically.)

- [ ] **Step 8: Commit**

```bash
git add ai/prompts/ask-format.md.tmpl prompts_test.go prompts_golden_test.go
git commit -m "feat: plain-language rule for clarifying questions"
```

---

### Task 2: Replace `uat-format`'s flat list with two fixed groups

**Files:**
- Modify: `ai/prompts/uat-format.md.tmpl`
- Modify: `prompts_test.go` (`TestUATFormatBlockCarriesItsRules`, ~line 119-142)
- Modify: `prompts_golden_test.go` (`TestGoldenUATFeaturePrompt` ~line 254, `TestGoldenUATBugPrompt` ~line 271)

**Interfaces:**
- Consumes: existing `mustRender("uat-format", d)` helper (`d` is a `map[string]any` / `promptData()`-shaped map with a `"UATCoverage"` key already set by callers) and `uatFeaturePrompt(specPath string) string` / `uatBugPrompt(issueBody, defaultBranch string) string` functions already defined in this package. No signature changes.
- Produces: nothing consumed by other tasks; independent of Task 1.

The current file (`ai/prompts/uat-format.md.tmpl`) reads:

```
{{define "uat-format"}}Rules for the checklist:
- A single flat list of Markdown `- [ ]` checkboxes. No headings, no grouping, no intro line, no closing line.
- Each item is `Action → expected result`: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover {{.UATCoverage}}, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.{{end}}
```

- [ ] **Step 1: Update the golden tests first**

In `prompts_golden_test.go`, `TestGoldenUATFeaturePrompt` (~line 261-267) and `TestGoldenUATBugPrompt` (~line 286-292), replace the "Rules for the checklist:" block. Old text in both `want` literals:

```go
Rules for the checklist:
- A single flat list of Markdown ` + "`- [ ]`" + ` checkboxes. No headings, no grouping, no intro line, no closing line.
- Each item is ` + "`Action → expected result`" + `: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
```

New text (same in both `want` literals — the coverage line below it differs per test and is unchanged):

```go
Rules for the checklist:
- Two group headings only, in this order: ` + "`### Happy path`" + `, then ` + "`### Edge cases`" + `. Omit a group with no items; never invent items to fill it. No other headings, no intro line, no closing line.
- Each item is ` + "`Action → expected result`" + `: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
```

Apply this replacement in both `TestGoldenUATFeaturePrompt`'s and `TestGoldenUATBugPrompt`'s `want` strings; leave every other line (No implementation detail, One item per behavior/coverage line, Compress wording, Do not modify) untouched in both.

- [ ] **Step 2: Run the golden tests to verify they fail**

Run: `go test ./... -run 'TestGoldenUATFeaturePrompt|TestGoldenUATBugPrompt' -v`
Expected: both FAIL — actual rendered output still has the old "single flat list... No headings" line.

- [ ] **Step 3: Update the format-block test**

Replace `TestUATFormatBlockCarriesItsRules` in `prompts_test.go` (~line 119-142) with:

```go
func TestUATFormatBlockCarriesItsRules(t *testing.T) {
	d := promptData()
	d["UATCoverage"] = "every behavior the spec describes"
	got := mustRender("uat-format", d)
	for _, want := range []string{
		"### Happy path",
		"### Edge cases",
		"`Action → expected result`",
		"15 words or fewer",
		"Compress wording, never coverage",
		"every behavior the spec describes",
		"Do not modify, create, or commit any file.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("uat-format block is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "under 20 items") {
		t.Errorf("uat-format block still caps the item count:\n%s", got)
	}
}
```

This drops the old `strings.Contains(got, "###")` negative assertion (the spec restores `###` group headings by design) and the `"single flat list"` / `"No headings"` positive assertions, replacing them with positive assertions for both new fixed headings. The `"under 20 items"` negative assertion is unrelated to this change and is kept as-is.

- [ ] **Step 4: Run it to verify it fails**

Run: `go test ./... -run TestUATFormatBlockCarriesItsRules -v`
Expected: FAIL — rendered block does not yet contain `"### Happy path"` / `"### Edge cases"`.

- [ ] **Step 5: Edit the template**

Update `ai/prompts/uat-format.md.tmpl`:

```
{{define "uat-format"}}Rules for the checklist:
- Two group headings only, in this order: `### Happy path`, then `### Edge cases`. Omit a group with no items; never invent items to fill it. No other headings, no intro line, no closing line.
- Each item is `Action → expected result`: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover {{.UATCoverage}}, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.{{end}}
```

- [ ] **Step 6: Run the three tests to verify they pass**

Run: `go test ./... -run 'TestUATFormatBlockCarriesItsRules|TestGoldenUATFeaturePrompt|TestGoldenUATBugPrompt' -v`
Expected: PASS for all three.

- [ ] **Step 7: Run the full package test suite**

Run: `go test ./...`
Expected: PASS. `TestBothRoutesShareTheUATFormatBlock`, `TestEveryTemplateRenders`, and `TestEveryPromptFileOnDiskIsParsed` all re-derive expectations from the template files / `mustRender`, so they need no edits and should stay green.

- [ ] **Step 8: Commit**

```bash
git add ai/prompts/uat-format.md.tmpl prompts_test.go prompts_golden_test.go
git commit -m "feat: group UAT checklist into happy path and edge cases"
```

---

### Task 3: Update `docs/how-it-works.md`'s UAT description

**Files:**
- Modify: `docs/how-it-works.md:39-43`

**Interfaces:**
- Consumes: nothing (prose-only doc edit).
- Produces: nothing consumed by other tasks.

Current text (`docs/how-it-works.md:39-43`):

```
The checklist itself is a single flat list of `- [ ]` items — no group headings —
each written as `Action → expected result` in 15 words or fewer, so the whole
verification pass fits on one screen. Both routes render the same rules from
`ai/prompts/uat-format.md.tmpl`; brevity is a prompt rule, not a length cap in
code, and coverage is never traded for it.
```

- [ ] **Step 1: Edit the paragraph**

Replace it with:

```
The checklist is grouped into exactly two headings, `### Happy path` then
`### Edge cases` — a group with no items is omitted, never invented — with
each item written as `Action → expected result` in 15 words or fewer, so the
whole verification pass fits on one screen. Both routes render the same rules
from `ai/prompts/uat-format.md.tmpl`; brevity is a prompt rule, not a length
cap in code, and coverage is never traded for it.
```

- [ ] **Step 2: Verify the doc reads correctly**

Run: `sed -n '29,44p' docs/how-it-works.md` and read it — confirm no other sentence in the "UAT checklist" section still claims a flat, ungrouped list.

- [ ] **Step 3: Commit**

```bash
git add docs/how-it-works.md
git commit -m "docs: describe the grouped UAT checklist shape"
```

---

### Task 4: Final verification

**Files:** none (verification only)

**Interfaces:** none.

- [ ] **Step 1: Run the full test suite once more from a clean state**

Run: `go build ./... && go test ./...`
Expected: build succeeds, all tests PASS, no leftover references to "single flat list" or "no headings" in test files or templates.

- [ ] **Step 2: Grep for stale references**

Run: `grep -rn "single flat list\|no group headings\|No headings, no grouping" --include='*.go' --include='*.tmpl' --include='*.md' .`
Expected: no matches (or only in this plan/spec's own historical quoting, which is fine — spec and plan files describe what changed, not the current template).

- [ ] **Step 3: Confirm git status is clean and all three commits are present**

Run: `git log --oneline -4` and `git status`
Expected: three feature commits from Tasks 1-3 on top of the plan commit, working tree clean.
