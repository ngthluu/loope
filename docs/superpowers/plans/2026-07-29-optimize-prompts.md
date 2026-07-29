# Optimize loope's prompt templates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sweep loope's `ai/prompts/*.md.tmpl` set for the duplication and asymmetry the audit in the spec found, applying the same shared-block pattern already used for `ask-format`/`uat-format`, plus three targeted content fixes (findings 4, 5, 6).

**Architecture:** Two new `{{define}}` blocks (`proxy-preamble`, `headless-commit-notes`) live in their own dedicated `.md.tmpl` files, following the exact convention `ask-format.md.tmpl`/`uat-format.md.tmpl` already established (file name == block name, file's only content is the `{{define}}...{{end}}`). `uat-format` itself is extended in place. Three prompts get one new sentence each (a termination guard, an ordering rule, a tiebreaker) with no new template parameters. No Go signatures change — `promptData()` already seeds every value these changes need.

**Tech Stack:** Go `text/template`, `embed.FS` (existing `prompts.go` machinery, unchanged).

## Global Constraints

- No change to which pipeline stage calls which prompt, to `prompts.go`'s rendering mechanism, or to any sentinel constant's value.
- No change to `comments.md.tmpl` or to `triage.md.tmpl`'s JSON output contract beyond the one tiebreaker sentence.
- No attempt to unify `brainstorm.md.tmpl`'s and `debug.md.tmpl`'s confidence-gate preambles (spec: "Considered and rejected").
- Every new/changed `.md.tmpl` file must render with no `<no value>` placeholder and no hardcoded sentinel literal — `TestEveryTemplateRenders` and `TestNoSentinelIsHardcodedInATemplate` in `prompts_test.go` enforce this generically already; no edits needed to either test.
- All work below has already been dry-run against the real repo (templates written, tests run, `gofmt`/`go vet` checked) to pin down exact byte-for-byte output — every "want" string in this plan is copied from an actual passing `go test` run, not guessed.

---

## Task 1: Shared `proxy-preamble` block

Factors the identical three-paragraph opening of `answerer.md.tmpl` and `done-confirm.md.tmpl` ("You are the product owner's proxy..." / issue / persona) into one block, per spec finding 1 / design item 1.

**Files:**
- Create: `ai/prompts/proxy-preamble.md.tmpl`
- Modify: `ai/prompts/answerer.md.tmpl`
- Modify: `ai/prompts/done-confirm.md.tmpl`
- Modify: `prompts_test.go` (skipTemplates, promptTestData, new test)
- Test: `prompts_golden_test.go` (no changes needed — verify only)

**Interfaces:**
- Consumes: `promptData()` in `prompts.go` (unchanged), `mustRender(name string, data map[string]any) string` in `prompts.go` (unchanged).
- Produces: template block named `proxy-preamble`, taking `.Issue` and `.Persona`, consumed by `answerer.md.tmpl` and `done-confirm.md.tmpl` via `{{template "proxy-preamble" .}}`.

- [ ] **Step 1: Write the failing test**

Add to `prompts_test.go`, after `TestBothRoutesShareTheUATFormatBlock` and before the `TestEveryPromptFileOnDiskIsParsed` doc comment:

```go
// Both routes must open with the same preamble, from the same source. This is
// what catches an edit made to one prompt that should have been an edit to
// the shared block.
func TestBothRoutesShareTheProxyPreambleBlock(t *testing.T) {
	d := promptData()
	d["Issue"] = "I"
	d["Persona"] = "P"
	block := mustRender("proxy-preamble", d)
	if !strings.Contains(answererPrompt("I", "P", "A"), block) {
		t.Error("answererPrompt does not contain the proxy-preamble block")
	}
	if !strings.Contains(doneConfirmPrompt("I", "P", "R"), block) {
		t.Error("doneConfirmPrompt does not contain the proxy-preamble block")
	}
}
```

Also add these two entries to `promptTestData` (so `TestEveryTemplateRenders` covers the new block directly):

```go
	"proxy-preamble":        {"Issue": "I", "Persona": "P"},
```

and this entry to `skipTemplates` (so the container file itself, whose body is empty apart from the `{{define}}`, isn't treated as a renderable prompt):

```go
	"proxy-preamble.md.tmpl":        true,
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestBothRoutesShareTheProxyPreambleBlock -v`
Expected: FAIL — `mustRender` panics with `template: "proxy-preamble" is undefined` (the block doesn't exist yet), or `go build` fails because `prompts_test.go` references it. Either failure mode is correct at this point.

- [ ] **Step 3: Create the shared block file**

Create `ai/prompts/proxy-preamble.md.tmpl`:

```
{{define "proxy-preamble"}}You are the product owner's proxy in an automated development pipeline.

The GitHub issue being implemented:
{{.Issue}}

Product owner preferences (persona):
{{.Persona}}{{end}}
```

(No trailing content after `{{end}}` other than the file's final newline — matches the `ask-format.md.tmpl` / `uat-format.md.tmpl` convention exactly.)

- [ ] **Step 4: Rewrite `answerer.md.tmpl` to use the block**

Replace the full contents of `ai/prompts/answerer.md.tmpl` with:

```
{{template "proxy-preamble" .}}

The architect agent said:
{{.ArchitectMsg}}

Instructions: if the architect asked questions, answer them decisively.
If it presented a design or spec for approval, approve it or give concise feedback.
Reply with your answer only.
```

- [ ] **Step 5: Rewrite `done-confirm.md.tmpl` to use the block**

Replace the full contents of `ai/prompts/done-confirm.md.tmpl` with:

```
{{template "proxy-preamble" .}}

The architect claims this issue is ALREADY fully implemented, for this reason:
{{.Reason}}

Instructions: judge whether that claim is consistent with the issue and the
product owner's intent. If you agree the work is already done, reply with
exactly {{.DoneConfirmSentinel}} and nothing else. If you disagree or have doubts, do NOT print that
token — instead reply with one concise sentence telling the architect what is
still missing or must be designed.
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./... -run 'TestBothRoutesShareTheProxyPreambleBlock|TestEveryTemplateRenders|TestGoldenAnswererPrompt|TestGoldenDoneConfirmPrompt' -v`
Expected: PASS on all four. `TestGoldenAnswererPrompt` and `TestGoldenDoneConfirmPrompt` need no edits — the refactor is byte-identical to the prior literal text, already confirmed by an actual `go test` run during planning.

- [ ] **Step 7: Commit**

```bash
git add ai/prompts/proxy-preamble.md.tmpl ai/prompts/answerer.md.tmpl ai/prompts/done-confirm.md.tmpl prompts_test.go
git commit -m "refactor: factor answerer/done-confirm preamble into proxy-preamble block"
```

---

## Task 2: Extend `uat-format` with the "Output ONLY the checklist" sentence

Moves the identical sentence from `uat-bug.md.tmpl` and `uat-feature.md.tmpl` into the `uat-format` block that already sits right below it in both files, per spec finding 2 / design item 2.

**Files:**
- Modify: `ai/prompts/uat-format.md.tmpl`
- Modify: `ai/prompts/uat-bug.md.tmpl`
- Modify: `ai/prompts/uat-feature.md.tmpl`
- Modify: `prompts_test.go` (`TestUATFormatBlockCarriesItsRules`)
- Test: `prompts_golden_test.go` (no changes needed — verify only)

**Interfaces:**
- Consumes: `.UATBeginSentinel`, `.UATEndSentinel` — already present in every call's data via `promptData()` (seeded from `uatBeginSentinel`/`uatEndSentinel` in `uat.go`); `.UATCoverage` — already set by `uatFeaturePrompt`/`uatBugPrompt` in `uat.go` before calling `mustRender`.
- Produces: no interface change — `uat-format` is still invoked the same way, `{{template "uat-format" .}}`, from both files.

- [ ] **Step 1: Write the failing test**

In `prompts_test.go`, extend `TestUATFormatBlockCarriesItsRules`'s `want` slice — add one new line at the top (order doesn't matter for `strings.Contains`, but keep it first for readability):

```go
func TestUATFormatBlockCarriesItsRules(t *testing.T) {
	d := promptData()
	d["UATCoverage"] = "every behavior the spec describes"
	got := mustRender("uat-format", d)
	for _, want := range []string{
		"Output ONLY the checklist, between a line reading UAT_BEGIN and a line reading",
		"single flat list",
		"No headings",
		"`Action → expected result`",
		"15 words or fewer",
		"Compress wording, never coverage",
		"every behavior the spec describes",
		"Do not modify, create, or commit any file.",
	} {
```

(Leave the rest of the function body unchanged.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestUATFormatBlockCarriesItsRules -v`
Expected: FAIL — `uat-format block is missing "Output ONLY the checklist..."`.

- [ ] **Step 3: Rewrite `uat-format.md.tmpl`**

Replace the full contents of `ai/prompts/uat-format.md.tmpl` with:

```
{{define "uat-format"}}Output ONLY the checklist, between a line reading {{.UATBeginSentinel}} and a line reading
{{.UATEndSentinel}}. Print nothing before or after those two lines.

Rules for the checklist:
- A single flat list of Markdown `- [ ]` checkboxes. No headings, no grouping, no intro line, no closing line.
- Each item is `Action → expected result`: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover {{.UATCoverage}}, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.{{end}}
```

- [ ] **Step 4: Rewrite `uat-bug.md.tmpl` to drop the now-duplicated sentence**

Replace the full contents of `ai/prompts/uat-bug.md.tmpl` with:

```
A bug fix has just been committed on this branch. Write a UAT (user acceptance
test) checklist for a human who will verify the fix by hand.

The GitHub issue being fixed:
{{.Issue}}

Read the issue above, then inspect what actually changed with
`git diff origin/{{.Base}}...HEAD` and `git log origin/{{.Base}}..HEAD`, so the checklist
describes the real fix. If that diff is empty — nothing was committed — print
nothing at all: no markers, no checklist, no explanation.

{{template "uat-format" .}}
```

- [ ] **Step 5: Rewrite `uat-feature.md.tmpl` to drop the now-duplicated sentence**

Replace the full contents of `ai/prompts/uat-feature.md.tmpl` with:

```
Read the approved spec at {{.SpecPath}} and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Output ONLY the checklist, between a line reading {{.UATBeginSentinel}} and a line reading
{{.UATEndSentinel}}. Print nothing before or after those two lines.

{{template "uat-format" .}}
```

Note: this step deliberately leaves the file in its pre-Task-4 shape (still has the now-duplicated sentence removed but no termination guard yet) — Task 4 rewrites this same file again to add the guard. Writing it here first, then again in Task 4, keeps this task's diff scoped to exactly "stop duplicating the sentence" with no behavior change, so `TestGoldenUATFeaturePrompt` needs no edit yet.

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./... -run 'TestUATFormatBlockCarriesItsRules|TestBothRoutesShareTheUATFormatBlock|TestEveryTemplateRenders|TestGoldenUATBugPrompt|TestGoldenUATFeaturePrompt' -v`
Expected: PASS on all five. Both golden tests need no edits — confirmed byte-identical by an actual `go test` run during planning (the sentence moved but final concatenated text is unchanged).

- [ ] **Step 7: Commit**

```bash
git add ai/prompts/uat-format.md.tmpl ai/prompts/uat-bug.md.tmpl ai/prompts/uat-feature.md.tmpl prompts_test.go
git commit -m "refactor: fold UAT output-format sentence into uat-format block"
```

---

## Task 3: Shared `headless-commit-notes` block

Factors the identical "HEADLESS: do not ask questions; make reasonable calls and note them in commit messages." sentence out of `debug.md.tmpl` and `execute.md.tmpl`, per spec finding 3 / design item 3.

**Files:**
- Create: `ai/prompts/headless-commit-notes.md.tmpl`
- Modify: `ai/prompts/debug.md.tmpl`
- Modify: `ai/prompts/execute.md.tmpl`
- Modify: `prompts_test.go` (skipTemplates, promptTestData, new test)
- Modify: `prompts_golden_test.go` (`TestGoldenBugPromptWithThreshold`, `TestGoldenBugPromptWithoutThreshold` — the sentence's line-wrap changes; `TestGoldenExecutePrompt` needs no edit)

**Interfaces:**
- Consumes: `promptData()`, `mustRender()` (both unchanged).
- Produces: template block named `headless-commit-notes`, taking no parameters, consumed by `debug.md.tmpl` and `execute.md.tmpl` via `{{template "headless-commit-notes" .}}`.

- [ ] **Step 1: Write the failing test**

Add to `prompts_test.go`, immediately after the `TestBothRoutesShareTheProxyPreambleBlock` function added in Task 1:

```go
// Both routes must carry the same headless-mode instruction, from the same
// source. This is what catches an edit made to one prompt that should have
// been an edit to the shared block.
func TestBothRoutesShareTheHeadlessCommitNotesBlock(t *testing.T) {
	block := mustRender("headless-commit-notes", promptData())
	if !strings.Contains(bugPrompt("I", 70), block) {
		t.Error("bugPrompt(threshold=70) does not contain the headless-commit-notes block")
	}
	if !strings.Contains(bugPrompt("I", 0), block) {
		t.Error("bugPrompt(threshold=0) does not contain the headless-commit-notes block")
	}
	if !strings.Contains(executePrompt("docs/plan.md"), block) {
		t.Error("executePrompt does not contain the headless-commit-notes block")
	}
}
```

Also add to `promptTestData`:

```go
	"headless-commit-notes": {},
```

and to `skipTemplates`:

```go
	"headless-commit-notes.md.tmpl": true,
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestBothRoutesShareTheHeadlessCommitNotesBlock -v`
Expected: FAIL / build error — `headless-commit-notes` template does not exist yet.

- [ ] **Step 3: Create the shared block file**

Create `ai/prompts/headless-commit-notes.md.tmpl`:

```
{{define "headless-commit-notes"}}HEADLESS: do not ask questions; make reasonable calls and note them in commit messages.{{end}}
```

- [ ] **Step 4: Rewrite `debug.md.tmpl` to use the block**

Replace the full contents of `ai/prompts/debug.md.tmpl` with:

```
/superpowers:systematic-debugging {{.Issue}}
{{if gt .Threshold 0}}
You may read the codebase first to investigate — but do NOT write code, tests,
or commits yet. Once you understand the failure, assess how confidently this bug
can be fixed as reported and print {{.ConfidenceSentinel}} <0-100> as the FIRST line of your reply.
Score the report, not the repair: a bug described precisely enough to act on
scores high however large the fix, and it still scores high when investigation
shows the behavior is already correct — that is a finding about the code, not a
gap in the report. Score low only when you cannot tell what behavior is wrong.
If that score is below {{.Threshold}}, the report is too vague or ambiguous to fix
responsibly: change no file. Instead, list what is missing and the specific
questions the author must answer, then stop.
The {{.ConfidenceSentinel}} line comes first even when an instruction below tells you to
print another sentinel and stop.

{{template "ask-format" .}}
{{end}}
Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. {{template "headless-commit-notes" .}}

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
{{.AlreadyDoneSentinel}} <one-sentence reason> on its own line and stop.
```

Note: this file already carries the Task 5-shaped `debug.md.tmpl` content (it already has the ordering-rule sentence — that sentence pre-dates this plan, it's the existing debug.md.tmpl content, unchanged here). Only the last paragraph's HEADLESS sentence is being swapped for the template call.

- [ ] **Step 5: Rewrite `execute.md.tmpl` to use the block**

Replace the full contents of `ai/prompts/execute.md.tmpl` with:

```
/superpowers:executing-plans Execute the plan at {{.PlanPath}}.
Use the execution style the plan recommends (subagent-driven or inline).
Follow TDD per the plan. Commit as you complete tasks.
{{template "headless-commit-notes" .}}
```

- [ ] **Step 6: Run test to verify it fails again (golden tests are now stale)**

Run: `go test ./... -run 'TestGoldenBugPromptWithThreshold|TestGoldenBugPromptWithoutThreshold|TestGoldenExecutePrompt' -v`
Expected: `TestGoldenExecutePrompt` PASSes (byte-identical — the sentence was already alone on its own line). `TestGoldenBugPromptWithThreshold` and `TestGoldenBugPromptWithoutThreshold` FAIL: the two-line-wrapped sentence "HEADLESS: do not ask questions; make reasonable calls\nand note them in commit messages." (wrapped at a line boundary in the old literal file) becomes the single-line "HEADLESS: do not ask questions; make reasonable calls and note them in commit messages." (the shared block has no internal line break). This is a pure formatting artifact of extraction, not a content change — expected and correct.

- [ ] **Step 7: Update the two stale goldens**

In `prompts_golden_test.go`, in `TestGoldenBugPromptWithThreshold`, change:

```go
Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.
```

to:

```go
Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls and note them in commit messages.
```

And in `TestGoldenBugPromptWithoutThreshold`, apply the identical change (same two lines, same replacement).

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./... -run 'TestBothRoutesShareTheHeadlessCommitNotesBlock|TestEveryTemplateRenders|TestGoldenBugPromptWithThreshold|TestGoldenBugPromptWithoutThreshold|TestGoldenExecutePrompt' -v`
Expected: PASS on all five.

- [ ] **Step 9: Commit**

```bash
git add ai/prompts/headless-commit-notes.md.tmpl ai/prompts/debug.md.tmpl ai/prompts/execute.md.tmpl prompts_test.go prompts_golden_test.go
git commit -m "refactor: factor debug/execute headless-mode sentence into headless-commit-notes block"
```

---

## Task 4: Termination guard for `uat-feature.md.tmpl`

Closes spec finding 4 / design item 4: `uat-bug.md.tmpl` already prints nothing when the diff is empty; `uat-feature.md.tmpl` has no equivalent guard for its own empty case (spec exists but nothing was actually shipped).

**Files:**
- Modify: `ai/prompts/uat-feature.md.tmpl`
- Test: `prompts_golden_test.go` (`TestGoldenUATFeaturePrompt`)

**Interfaces:**
- Consumes: `.SpecPath` (already the only variable `uatFeaturePrompt` in `uat.go:169` sets — no new template parameter is introduced; the guard tells the model to inspect the repo with plain `git log`/`git diff`, not a `{{.Base}}` variable, since `uatFeaturePrompt`'s signature is `uatFeaturePrompt(specPath string) string` and has no base branch to pass in).
- Produces: no interface change.

- [ ] **Step 1: Write the failing test**

In `prompts_golden_test.go`, in `TestGoldenUATFeaturePrompt`, change the `want` string from:

```go
	want := `Read the approved spec at docs/spec.md and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Output ONLY the checklist, between a line reading UAT_BEGIN and a line reading
UAT_END. Print nothing before or after those two lines.
```

to:

```go
	want := `Read the approved spec at docs/spec.md and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Inspect the repository with ` + "`git log`" + ` and ` + "`git diff`" + ` first. If nothing was
committed, or the shipped changes don't actually implement the spec's
feature, print nothing at all: no markers, no checklist, no explanation.

Output ONLY the checklist, between a line reading UAT_BEGIN and a line reading
UAT_END. Print nothing before or after those two lines.
```

(Leave the rest of the `want` string — the `Rules for the checklist:` block and everything after — unchanged.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestGoldenUATFeaturePrompt -v`
Expected: FAIL — `got` is missing the new "Inspect the repository..." paragraph.

- [ ] **Step 3: Add the guard to `uat-feature.md.tmpl`**

Replace the full contents of `ai/prompts/uat-feature.md.tmpl` with:

```
Read the approved spec at {{.SpecPath}} and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Inspect the repository with `git log` and `git diff` first. If nothing was
committed, or the shipped changes don't actually implement the spec's
feature, print nothing at all: no markers, no checklist, no explanation.

{{template "uat-format" .}}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestGoldenUATFeaturePrompt|TestBothRoutesShareTheUATFormatBlock|TestEveryTemplateRenders' -v`
Expected: PASS on all three.

- [ ] **Step 5: Commit**

```bash
git add ai/prompts/uat-feature.md.tmpl prompts_golden_test.go
git commit -m "fix: add termination guard to uat-feature.md.tmpl for unshipped specs"
```

---

## Task 5: Sentinel-ordering rule for `brainstorm.md.tmpl`

Closes spec finding 5 / design item 5: `debug.md.tmpl` already states its `{{.ConfidenceSentinel}}` line must print first, even over `{{.AlreadyDoneSentinel}}`; `brainstorm.md.tmpl` has the identical two-sentinel structure (`ConfidenceSentinel` gate, `AlreadyDoneSentinel` later) with no such rule.

**Files:**
- Modify: `ai/prompts/brainstorm.md.tmpl`
- Test: `prompts_golden_test.go` (`TestGoldenBrainstormPromptWithThreshold` — `TestGoldenBrainstormPromptWithoutThreshold` needs no edit, since the added sentence is inside the `{{if gt .Threshold 0}}` guard)

**Interfaces:**
- Consumes: `.ConfidenceSentinel` — already seeded by `promptData()` and already used earlier in this same file.
- Produces: no interface change.

- [ ] **Step 1: Write the failing test**

In `prompts_golden_test.go`, in `TestGoldenBrainstormPromptWithThreshold`, change:

```go
below 70, the issue is too under-specified or ambiguous to implement
responsibly: do NOT design or write a spec. Instead, list what is missing and
the specific questions the author must answer, then stop.

Write that reply as a short, skimmable list the author can answer in one comment:
```

to:

```go
below 70, the issue is too under-specified or ambiguous to implement
responsibly: do NOT design or write a spec. Instead, list what is missing and
the specific questions the author must answer, then stop.
The CONFIDENCE: line comes first even when an instruction below tells you to
print another sentinel and stop.

Write that reply as a short, skimmable list the author can answer in one comment:
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestGoldenBrainstormPromptWithThreshold -v`
Expected: FAIL — `got` is missing the new ordering-rule line.

- [ ] **Step 3: Add the ordering rule to `brainstorm.md.tmpl`**

Replace the full contents of `ai/prompts/brainstorm.md.tmpl` with:

```
/superpowers:brainstorming {{.Issue}}
{{if gt .Threshold 0}}
Before anything else, assess how confidently this issue can be implemented as
written and print {{.ConfidenceSentinel}} <0-100> as the FIRST line of your reply. If that score is
below {{.Threshold}}, the issue is too under-specified or ambiguous to implement
responsibly: do NOT design or write a spec. Instead, list what is missing and
the specific questions the author must answer, then stop.
The {{.ConfidenceSentinel}} line comes first even when an instruction below tells you to
print another sentinel and stop.

{{template "ask-format" .}}
{{end}}
HEADLESS MODE: your interlocutor is an automated product-owner agent, not a human.
Ask clarifying questions as plain text (AskUserQuestion is disabled).
Follow the brainstorming flow to a committed spec: clarifying questions, design,
then write and commit the spec document into this branch. Do NOT invoke the
writing-plans skill — a separate session writes the implementation plan.
When the spec file is written and committed, print {{.SpecReadySentinel}} <path> on its own line,
where <path> is the spec file path relative to the repository root.

If during brainstorming you determine the feature is already fully implemented
in this codebase, do not invent work: print {{.AlreadyDoneSentinel}} <one-sentence reason> on its own
line instead of continuing.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestGoldenBrainstormPromptWithThreshold|TestGoldenBrainstormPromptWithoutThreshold|TestBothRoutesShareTheAskFormatBlock|TestEveryTemplateRenders' -v`
Expected: PASS on all four.

- [ ] **Step 5: Commit**

```bash
git add ai/prompts/brainstorm.md.tmpl prompts_golden_test.go
git commit -m "fix: add sentinel-ordering rule to brainstorm.md.tmpl"
```

---

## Task 6: Tiebreaker line for `triage.md.tmpl`

Closes spec finding 6 / design item 6: the "bug" and "feature" descriptions given to the model can both apply to the same issue, with no stated tiebreaker. This is a prompt-only change — `TriageDecision.Kind` validation in `triage.go:32` (`dec.Kind != "bug" && dec.Kind != "feature"`) is untouched, since the JSON output contract doesn't change.

**Files:**
- Modify: `ai/prompts/triage.md.tmpl`
- Test: `prompts_golden_test.go` (`TestGoldenTriagePrompt`)

**Interfaces:**
- Consumes: `.List` — already the only variable `triagePrompt` in `triage.go:56` sets.
- Produces: no interface change.

- [ ] **Step 1: Write the failing test**

In `prompts_golden_test.go`, in `TestGoldenTriagePrompt`, change:

```go
	want := `You are a triage agent for an automated development pipeline.

Open eligible issues:
[LIST]

Decide from the issue text alone — do NOT read the repository. Pick the single
best issue to work on next and classify it:
- "bug": a small, well-scoped defect that can be fixed by reproducing and debugging
- "feature": anything that needs design work (new functionality, refactors, unclear scope)

Respond with ONLY a JSON object, no other text:
{"issueNumber": <int>, "kind": "bug" or "feature", "reason": "<one sentence>"}`
```

to:

```go
	want := `You are a triage agent for an automated development pipeline.

Open eligible issues:
[LIST]

Decide from the issue text alone — do NOT read the repository. Pick the single
best issue to work on next and classify it:
- "bug": a small, well-scoped defect that can be fixed by reproducing and debugging
- "feature": anything that needs design work (new functionality, refactors, unclear scope)
If an issue plausibly fits both, classify it "bug" when the fix needs no design
decision, otherwise "feature".

Respond with ONLY a JSON object, no other text:
{"issueNumber": <int>, "kind": "bug" or "feature", "reason": "<one sentence>"}`
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestGoldenTriagePrompt -v`
Expected: FAIL — `got` is missing the tiebreaker sentence.

- [ ] **Step 3: Add the tiebreaker to `triage.md.tmpl`**

Replace the full contents of `ai/prompts/triage.md.tmpl` with:

```
You are a triage agent for an automated development pipeline.

Open eligible issues:
{{.List}}

Decide from the issue text alone — do NOT read the repository. Pick the single
best issue to work on next and classify it:
- "bug": a small, well-scoped defect that can be fixed by reproducing and debugging
- "feature": anything that needs design work (new functionality, refactors, unclear scope)
If an issue plausibly fits both, classify it "bug" when the fix needs no design
decision, otherwise "feature".

Respond with ONLY a JSON object, no other text:
{"issueNumber": <int>, "kind": "bug" or "feature", "reason": "<one sentence>"}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run 'TestGoldenTriagePrompt|TestEveryTemplateRenders' -v`
Expected: PASS on both.

- [ ] **Step 5: Commit**

```bash
git add ai/prompts/triage.md.tmpl prompts_golden_test.go
git commit -m "fix: add bug/feature tiebreaker to triage.md.tmpl"
```

---

## Task 7: Full-suite verification

Confirms the whole set — including the generic tests the spec's Testing section says need no changes (`TestEveryTemplateRenders`, `TestEveryPromptFileOnDiskIsParsed`, `TestNoSentinelIsHardcodedInATemplate`) — passes together, and that formatting/vet are clean.

**Files:** none (verification only).

**Interfaces:** none.

- [ ] **Step 1: Run the full test suite**

Run: `go test ./... -v`
Expected: PASS, no failures. In particular confirm these pass without having been edited: `TestEveryTemplateRenders`, `TestEveryPromptFileOnDiskIsParsed`, `TestNoSentinelIsHardcodedInATemplate`, `TestAskFormatBlockCarriesItsRules`, `TestBothRoutesShareTheAskFormatBlock`, `TestGoldenPlanPrompt`, `TestGoldenAnswererPrompt`, `TestGoldenDoneConfirmPrompt`, `TestGoldenUATBugPrompt`, `TestGoldenExecutePrompt`.

- [ ] **Step 2: Check formatting and vet**

Run: `gofmt -l .`
Expected: no output (nothing to reformat).

Run: `go vet ./...`
Expected: no output.

- [ ] **Step 3: Confirm no stray files**

Run: `git status --porcelain`
Expected: clean (everything from Tasks 1–6 already committed).

No commit for this task — it's verification-only, nothing to stage.

---

## Self-review notes (for the plan author, not the implementer)

- **Spec coverage:** design items 1–6 map to Tasks 1, 2, 3, 4, 5, 6 respectively. Testing-section items map to: `TestBothRoutesShareTheProxyPreambleBlock` (Task 1), `TestBothRoutesShareTheHeadlessCommitNotesBlock` (Task 3), `TestUATFormatBlockCarriesItsRules` extension (Task 2), golden updates for answerer/done-confirm (Task 1, verify-only), uat-bug/uat-feature (Task 2, verify-only, then Task 4 adds the guard), debug/execute (Task 3), brainstorm (Task 5), triage (Task 6). `TestEveryTemplateRenders`, `TestEveryPromptFileOnDiskIsParsed`, `TestNoSentinelIsHardcodedInATemplate` are confirmed unchanged in Task 7.
- **No placeholders:** every "want" string and every full-file replacement in this plan was copied verbatim from an actual `go test` / file-write run against this repository during planning, not written from memory or guessed.
- **Type/name consistency:** `promptData()`, `mustRender(name string, data map[string]any) string`, `answererPrompt(issue, persona, architectMsg string) string`, `doneConfirmPrompt(issue, persona, reason string) string`, `bugPrompt(issue string, threshold int) string`, `executePrompt(planPath string) string`, `uatFeaturePrompt(specPath string) string`, `uatBugPrompt(issue, base string) string`, `triagePrompt(list string) string`, `brainstormPrompt(issue string, threshold int) string` are all pre-existing and unchanged by this plan — verified against `pipeline_feature.go`, `pipeline_bug.go`, `uat.go`, `triage.go`.
