# Externalize Prompts into `ai/prompts/` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move every prompt and every human-facing outbound text template out of the Go source into `ai/prompts/*.md.tmpl`, embedded into the binary with `go:embed`, without changing a single byte of the text that is produced.

**Architecture:** A new `prompts.go` embeds `ai/prompts/` and parses it once at package init into a single `text/template` set with `missingkey=error`. A `mustRender(name, data)` helper executes a named template and trims the file's trailing newline. Each existing prompt builder keeps its exact signature and collapses to one `mustRender` call. Golden tests are written **first**, against the current `fmt.Sprintf` code, so the relocation is a verified byte-for-byte move rather than a claimed one.

**Tech Stack:** Go 1.25.5, standard library only (`embed`, `text/template`, `bytes`, `strings`). No new dependencies; `go.mod` stays dependency-free.

## Global Constraints

- Module is `loope`, package `main`, all Go files at the repository root. Go 1.25.5.
- **No new dependencies.** `go.mod` must remain dependency-free.
- **Pure relocation:** no rewording of any prompt or comment text. Output must be byte-identical to today's.
- Prompt directory is **flat**: `ai/prompts/<name>.md.tmpl`. No subdirectories — `template.ParseFS` names templates by base filename, so same-named files in different directories would silently shadow one another.
- **Sentinels are never written as literal text in a `.tmpl` file.** `confidenceSentinel`, `specReadySentinel`, `readySentinel`, `alreadyDoneSentinel`, `doneConfirmSentinel` are always injected from the Go constants via `promptData()`.
- `mustRender` **panics** on error. A render failure is a static defect, not a runtime condition; the full-FS test in Task 7 is what makes that safe.
- Template set must be created with `Option("missingkey=error")`.
- Out of scope, do not touch: `serve.go` (`pageHead`, `railTmpl`, `detailTmpl`, `stepcardTmpl`), `ui.go` (`uiJS`), `.goreleaser.yaml`.
- No runtime override of the embedded prompts (no config key pointing at a prompts directory). Deliberately omitted as YAGNI.
- Existing substring assertions in `pipeline_feature_test.go` and `pipeline_bug_test.go` stay untouched and must keep passing.

## Assumptions

Recorded here because the spec was approved headlessly and these calls were made without asking:

1. **Part-B golden ordering.** The spec's "golden-first" rule is trivially satisfiable for the seven existing prompt builders (they already exist). The `loop.go` comment/PR strings are built inline at their call sites, so there is nothing to golden-test first. Task 5 therefore does a *pure Go extraction* (literals moved verbatim into named builder functions) with goldens, and Task 6 moves those same builders to templates against the unchanged goldens. Same guarantee, one extra step.
2. **Park's error tail.** Today the `Error: ...` tail is unconditional. The spec asks for it to become an `{{if}}` block. The template guards it with `{{if .Error}}`, which is behaviour-identical for every current caller (the error is never empty) and additionally defined for the empty case. Both branches are tested.
3. **`pr-comment` template.** The current code is `"🤖 PR: " + url` — a concatenation, not a `Sprintf`. It is still templated, for consistency with the other outbound strings named in the spec.
4. **goreleaser availability.** Task 7's final verification prefers `goreleaser release --snapshot --clean`. If `goreleaser` is not installed, a plain `go build` plus the "rename `ai/` away and run the binary" check proves the same property (nothing read from disk at runtime). Both commands are given.

---

## File Structure

**Created:**

| Path | Responsibility |
| --- | --- |
| `prompts.go` | `go:embed` of `ai/prompts`, the parsed template set, `mustRender`, `promptData`. Nothing else. |
| `ai/prompts/brainstorm.md.tmpl` | Brainstorm prompt incl. the conditional confidence paragraph. |
| `ai/prompts/answerer.md.tmpl` | Product-owner-proxy answerer prompt. |
| `ai/prompts/done-confirm.md.tmpl` | Already-done confirmation prompt. |
| `ai/prompts/plan.md.tmpl` | Plan-writing prompt. |
| `ai/prompts/execute.md.tmpl` | Plan-execution prompt. |
| `ai/prompts/debug.md.tmpl` | Bug pipeline debug prompt. |
| `ai/prompts/rework.md.tmpl` | Rework resume prompt. |
| `ai/prompts/triage.md.tmpl` | Triage prompt. |
| `ai/prompts/comments.md.tmpl` | Multi-template file: `pickup`, `already-done`, `needs-info`, `park`, `pr-comment`, `pr-title`, `pr-body`, and `guidance-usage-limit`, `guidance-budget`, `guidance-interrupted`, `guidance-network`. |
| `prompts_golden_test.go` | One golden per builder, both branches of every conditional. Written before the move; unchanged by it. |
| `prompts_test.go` | Full-FS table test rendering every template in the embedded set. |

**Modified:**

| Path | Change |
| --- | --- |
| `pipeline_feature.go:258-336` | Five builders collapse to `mustRender` calls. |
| `pipeline_bug.go:29-39` | `bugPrompt` collapses. |
| `rework.go:61-69` | `reworkPrompt` collapses. |
| `triage.go:17-52` | Inline prompt extracted to `triagePrompt(list string) string`, then collapses. |
| `loop.go` | Inline comment/PR strings extracted to builders (Task 5), then collapse (Task 6). `classifyCause` returns `mustRender` calls. |

---

## Task 1: Golden tests for the seven existing prompt builders, plus `triagePrompt` extraction

Locks today's output into literal expectations *before* anything moves. Also extracts `triage.go`'s inline prompt into a builder so all eight prompts have the same shape.

**Files:**
- Create: `prompts_golden_test.go`
- Modify: `triage.go:17-52`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `func triagePrompt(list string) string` in `triage.go`. All other builder signatures are unchanged and remain:
  `brainstormPrompt(issue string, threshold int) string`, `answererPrompt(issue, persona, architectMsg string) string`, `doneConfirmPrompt(issue, persona, reason string) string`, `planPrompt(specPath string) string`, `executePrompt(planPath string) string`, `bugPrompt(issue string) string`, `reworkPrompt() string`.

- [ ] **Step 1: Extract the triage prompt into a builder**

In `triage.go`, replace the inline `prompt := fmt.Sprintf(...)` in `Triage` with a call, and add the builder at the bottom of the file.

In `Triage`, the block currently at lines 22–33 becomes:

```go
	prompt := triagePrompt(string(list))
```

Note `list` is `[]byte` from `json.MarshalIndent`; the builder takes a `string`.

Add at the end of `triage.go`:

```go
func triagePrompt(list string) string {
	return fmt.Sprintf(`You are a triage agent for an automated development pipeline.

Open eligible issues:
%s

Decide from the issue text alone — do NOT read the repository. Pick the single
best issue to work on next and classify it:
- "bug": a small, well-scoped defect that can be fixed by reproducing and debugging
- "feature": anything that needs design work (new functionality, refactors, unclear scope)

Respond with ONLY a JSON object, no other text:
{"issueNumber": <int>, "kind": "bug" or "feature", "reason": "<one sentence>"}`, list)
}
```

- [ ] **Step 2: Verify the extraction compiles and the existing suite is green**

Run: `go build ./... && go test ./... -run Triage -v`
Expected: PASS. `fmt` is still imported by `triage.go` (used by `parseTriage` and the error paths), so no import change is needed.

- [ ] **Step 3: Write the golden test file**

Create `prompts_golden_test.go`. These expectations are transcribed from the current source; they must pass **now**, against the `fmt.Sprintf` implementations.

```go
package main

import "testing"

// Golden expectations for every prompt builder, written against the original
// fmt.Sprintf implementations. Externalizing the text into ai/prompts/ must
// leave every one of them byte-identical.

func check(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestGoldenBrainstormPromptWithThreshold(t *testing.T) {
	want := `/superpowers:brainstorming ISSUE BODY

Before anything else, assess how confidently this issue can be implemented as
written and print CONFIDENCE: <0-100> as the FIRST line of your reply. If that score is
below 70, the issue is too under-specified or ambiguous to implement
responsibly: do NOT design or write a spec. Instead, list what is missing and
the specific questions the author must answer, then stop.

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
	check(t, "brainstormPrompt(threshold=70)", brainstormPrompt("ISSUE BODY", 70), want)
}

func TestGoldenBrainstormPromptWithoutThreshold(t *testing.T) {
	want := `/superpowers:brainstorming ISSUE BODY

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
	check(t, "brainstormPrompt(threshold=0)", brainstormPrompt("ISSUE BODY", 0), want)
}

func TestGoldenAnswererPrompt(t *testing.T) {
	want := `You are the product owner's proxy in an automated development pipeline.

The GitHub issue being implemented:
ISSUE BODY

Product owner preferences (persona):
PERSONA TEXT

The architect agent said:
ARCHITECT MSG

Instructions: if the architect asked questions, answer them decisively.
If it presented a design or spec for approval, approve it or give concise feedback.
Reply with your answer only.`
	check(t, "answererPrompt", answererPrompt("ISSUE BODY", "PERSONA TEXT", "ARCHITECT MSG"), want)
}

func TestGoldenDoneConfirmPrompt(t *testing.T) {
	want := `You are the product owner's proxy in an automated development pipeline.

The GitHub issue being implemented:
ISSUE BODY

Product owner preferences (persona):
PERSONA TEXT

The architect claims this issue is ALREADY fully implemented, for this reason:
REASON TEXT

Instructions: judge whether that claim is consistent with the issue and the
product owner's intent. If you agree the work is already done, reply with
exactly DONE_CONFIRMED and nothing else. If you disagree or have doubts, do NOT print that
token — instead reply with one concise sentence telling the architect what is
still missing or must be designed.`
	check(t, "doneConfirmPrompt", doneConfirmPrompt("ISSUE BODY", "PERSONA TEXT", "REASON TEXT"), want)
}

func TestGoldenPlanPrompt(t *testing.T) {
	want := `/superpowers:writing-plans Read the approved spec at docs/spec.md and
write a detailed implementation plan for it. Commit the plan into this branch.
HEADLESS MODE: do not ask questions; the spec is approved and complete — make
reasonable calls and note any assumptions in the plan.
When the implementation plan file is written and committed, print PIPELINE_READY on its own
line.`
	check(t, "planPrompt", planPrompt("docs/spec.md"), want)
}

func TestGoldenExecutePrompt(t *testing.T) {
	want := `/superpowers:executing-plans Execute the plan at docs/plan.md.
Use the execution style the plan recommends (subagent-driven or inline).
Follow TDD per the plan. Commit as you complete tasks.
HEADLESS: do not ask questions; make reasonable calls and note them in commit messages.`
	check(t, "executePrompt", executePrompt("docs/plan.md"), want)
}

func TestGoldenBugPrompt(t *testing.T) {
	want := `/superpowers:systematic-debugging ISSUE BODY

Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
PIPELINE_ALREADY_DONE: <one-sentence reason> on its own line and stop.`
	check(t, "bugPrompt", bugPrompt("ISSUE BODY"), want)
}

func TestGoldenReworkPrompt(t *testing.T) {
	want := `Continue the work on this issue where the previous session left off.
Complete the remaining implementation, make the full test suite pass, and commit
all changes. HEADLESS: do not ask questions; make reasonable calls and note them
in commit messages.

If you find the work is already fully implemented, do not fabricate changes:
print PIPELINE_ALREADY_DONE: <one-sentence reason> on its own line and stop.`
	check(t, "reworkPrompt", reworkPrompt(), want)
}

func TestGoldenTriagePrompt(t *testing.T) {
	want := `You are a triage agent for an automated development pipeline.

Open eligible issues:
[LIST]

Decide from the issue text alone — do NOT read the repository. Pick the single
best issue to work on next and classify it:
- "bug": a small, well-scoped defect that can be fixed by reproducing and debugging
- "feature": anything that needs design work (new functionality, refactors, unclear scope)

Respond with ONLY a JSON object, no other text:
{"issueNumber": <int>, "kind": "bug" or "feature", "reason": "<one sentence>"}`
	check(t, "triagePrompt", triagePrompt("[LIST]"), want)
}
```

- [ ] **Step 4: Run the goldens against the current implementation**

Run: `go test ./... -run TestGolden -v`
Expected: all nine tests PASS. **If any fail, the expectation was mis-transcribed — fix the expectation to match the current source, not the source to match the expectation.** This test file is the contract for the rest of the plan.

- [ ] **Step 5: Run the full suite**

Run: `go test ./...`
Expected: PASS (ok loope).

- [ ] **Step 6: Commit**

```bash
git add prompts_golden_test.go triage.go
git commit -m "test: golden expectations for every prompt builder"
```

---

## Task 2: `prompts.go` infrastructure and the brainstorm template

Builds the embed/render machinery and moves the hardest prompt (the only one with a Go-side conditional) first, so the infrastructure is proven against the case most likely to drift.

**Files:**
- Create: `prompts.go`, `ai/prompts/brainstorm.md.tmpl`
- Modify: `pipeline_feature.go:258-282`
- Test: `prompts_golden_test.go` (unchanged — it must keep passing)

**Interfaces:**
- Consumes: `confidenceSentinel` (`confidence.go:11`), `alreadyDoneSentinel` (`done.go:7`), `readySentinel` (`pipeline_feature.go:13`), `specReadySentinel` (`pipeline_feature.go:15`), `doneConfirmSentinel` (`pipeline_feature.go:301`).
- Produces:
  - `func mustRender(name string, data map[string]any) string` — executes the named template, panics on error, returns output with one trailing `\n` trimmed.
  - `func promptData() map[string]any` — a fresh map pre-populated with keys `ConfidenceSentinel`, `SpecReadySentinel`, `ReadySentinel`, `AlreadyDoneSentinel`, `DoneConfirmSentinel`.
  - `var prompts *template.Template` — the parsed set.
  - `var promptFS embed.FS` — the embedded `ai/prompts` directory.

- [ ] **Step 1: Create the brainstorm template**

Create `ai/prompts/brainstorm.md.tmpl` with exactly this content (the file ends with a single trailing newline, which `mustRender` trims):

```
/superpowers:brainstorming {{.Issue}}
{{if gt .Threshold 0}}
Before anything else, assess how confidently this issue can be implemented as
written and print {{.ConfidenceSentinel}} <0-100> as the FIRST line of your reply. If that score is
below {{.Threshold}}, the issue is too under-specified or ambiguous to implement
responsibly: do NOT design or write a spec. Instead, list what is missing and
the specific questions the author must answer, then stop.
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

Why no `{{-` trim markers: the text between `{{if …}}` and `Before` is exactly one `\n`, and the text between `{{end}}` and `HEADLESS` is exactly one `\n`. With the threshold on that yields `…{{.Issue}}\n` + `\nBefore…stop.\n` + `\nHEADLESS…`; with it off, `…{{.Issue}}\n` + `\nHEADLESS…`. Both match the goldens exactly.

- [ ] **Step 2: Create `prompts.go`**

```go
package main

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

// All model-facing prompts and human-facing outbound text live in ai/prompts/
// and are compiled into the binary, so a release is still one self-contained
// file that reads nothing from disk at runtime.
//
// The directory is flat on purpose: template.ParseFS names each template by its
// base filename, so two files with the same name in different subdirectories
// would silently shadow one another.
//
//go:embed ai/prompts
var promptFS embed.FS

// missingkey=error is load-bearing, not incidental: without it a typo'd
// placeholder renders as the literal "<no value>" inside a prompt that then
// gets sent to Claude — a silent, expensive failure. With it, the same typo is
// a loud render error that the tests in prompts_test.go catch.
var prompts = template.Must(
	template.New("prompts").
		Option("missingkey=error").
		ParseFS(promptFS, "ai/prompts/*.md.tmpl"),
)

// mustRender executes a named template and trims the file's trailing newline —
// editors end files with one, the string literals this replaced did not.
//
// It panics because a render failure is a static defect (unknown template name,
// missing key), not a runtime condition: prompts_test.go renders every template
// in the embedded FS, so such a defect fails the build instead of reaching a
// running daemon.
func mustRender(name string, data map[string]any) string {
	var buf bytes.Buffer
	if err := prompts.ExecuteTemplate(&buf, name, data); err != nil {
		panic(fmt.Sprintf("render prompt %q: %v", name, err))
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// promptData seeds template data with every sentinel constant. The sentinels are
// never written as literal text in a .tmpl file: the same constants drive the
// parsers in confidence.go, done.go, and pipeline_feature.go, and hardcoding
// them in the prompts would let the instruction and the parser drift apart.
func promptData() map[string]any {
	return map[string]any{
		"ConfidenceSentinel":  confidenceSentinel,
		"SpecReadySentinel":   specReadySentinel,
		"ReadySentinel":       readySentinel,
		"AlreadyDoneSentinel": alreadyDoneSentinel,
		"DoneConfirmSentinel": doneConfirmSentinel,
	}
}
```

- [ ] **Step 3: Rewrite `brainstormPrompt`**

In `pipeline_feature.go`, replace the whole of `brainstormPrompt` (lines 258–282) with:

```go
func brainstormPrompt(issue string, threshold int) string {
	d := promptData()
	d["Issue"] = issue
	d["Threshold"] = threshold
	return mustRender("brainstorm.md.tmpl", d)
}
```

- [ ] **Step 4: Run the brainstorm goldens**

Run: `go test ./... -run TestGoldenBrainstorm -v`
Expected: both `TestGoldenBrainstormPromptWithThreshold` and `TestGoldenBrainstormPromptWithoutThreshold` PASS. A diff here is whitespace drift in the template — fix the template, never the golden.

- [ ] **Step 5: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS. `pipeline_feature.go` still imports `fmt` (used elsewhere in the file); if `go vet` reports it unused, remove the import.

- [ ] **Step 6: Commit**

```bash
git add prompts.go ai/prompts/brainstorm.md.tmpl pipeline_feature.go
git commit -m "feat: embed ai/prompts and move the brainstorm prompt"
```

---

## Task 3: Move the remaining `pipeline_feature.go` prompts

**Files:**
- Create: `ai/prompts/answerer.md.tmpl`, `ai/prompts/done-confirm.md.tmpl`, `ai/prompts/plan.md.tmpl`, `ai/prompts/execute.md.tmpl`
- Modify: `pipeline_feature.go:284-336`
- Test: `prompts_golden_test.go` (unchanged)

**Interfaces:**
- Consumes: `mustRender(name string, data map[string]any) string`, `promptData() map[string]any` (Task 2).
- Produces: nothing new — `answererPrompt`, `doneConfirmPrompt`, `planPrompt`, `executePrompt` keep their current signatures.

- [ ] **Step 1: Create `ai/prompts/answerer.md.tmpl`**

```
You are the product owner's proxy in an automated development pipeline.

The GitHub issue being implemented:
{{.Issue}}

Product owner preferences (persona):
{{.Persona}}

The architect agent said:
{{.ArchitectMsg}}

Instructions: if the architect asked questions, answer them decisively.
If it presented a design or spec for approval, approve it or give concise feedback.
Reply with your answer only.
```

- [ ] **Step 2: Create `ai/prompts/done-confirm.md.tmpl`**

```
You are the product owner's proxy in an automated development pipeline.

The GitHub issue being implemented:
{{.Issue}}

Product owner preferences (persona):
{{.Persona}}

The architect claims this issue is ALREADY fully implemented, for this reason:
{{.Reason}}

Instructions: judge whether that claim is consistent with the issue and the
product owner's intent. If you agree the work is already done, reply with
exactly {{.DoneConfirmSentinel}} and nothing else. If you disagree or have doubts, do NOT print that
token — instead reply with one concise sentence telling the architect what is
still missing or must be designed.
```

- [ ] **Step 3: Create `ai/prompts/plan.md.tmpl`**

```
/superpowers:writing-plans Read the approved spec at {{.SpecPath}} and
write a detailed implementation plan for it. Commit the plan into this branch.
HEADLESS MODE: do not ask questions; the spec is approved and complete — make
reasonable calls and note any assumptions in the plan.
When the implementation plan file is written and committed, print {{.ReadySentinel}} on its own
line.
```

- [ ] **Step 4: Create `ai/prompts/execute.md.tmpl`**

```
/superpowers:executing-plans Execute the plan at {{.PlanPath}}.
Use the execution style the plan recommends (subagent-driven or inline).
Follow TDD per the plan. Commit as you complete tasks.
HEADLESS: do not ask questions; make reasonable calls and note them in commit messages.
```

- [ ] **Step 5: Rewrite the four builders**

In `pipeline_feature.go`, replace the bodies of `answererPrompt` (284–299), `doneConfirmPrompt` (303–320), `planPrompt` (322–329), and `executePrompt` (331–336). Leave `const doneConfirmSentinel = "DONE_CONFIRMED"` at line 301 exactly where it is.

```go
func answererPrompt(issue, persona, architectMsg string) string {
	d := promptData()
	d["Issue"] = issue
	d["Persona"] = persona
	d["ArchitectMsg"] = architectMsg
	return mustRender("answerer.md.tmpl", d)
}

const doneConfirmSentinel = "DONE_CONFIRMED"

func doneConfirmPrompt(issue, persona, reason string) string {
	d := promptData()
	d["Issue"] = issue
	d["Persona"] = persona
	d["Reason"] = reason
	return mustRender("done-confirm.md.tmpl", d)
}

func planPrompt(specPath string) string {
	d := promptData()
	d["SpecPath"] = specPath
	return mustRender("plan.md.tmpl", d)
}

func executePrompt(planPath string) string {
	d := promptData()
	d["PlanPath"] = planPath
	return mustRender("execute.md.tmpl", d)
}
```

- [ ] **Step 6: Run the goldens and the pipeline tests**

Run: `go test ./... -run 'TestGolden|TestFeaturePipeline' -v`
Expected: PASS, including the pre-existing substring assertions in `pipeline_feature_test.go`.

- [ ] **Step 7: Run build, vet, and the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS. If `fmt` is now unused in `pipeline_feature.go`, remove it from the import block (it is still used by `RunFeaturePipeline`'s error paths, so this is unlikely).

- [ ] **Step 8: Commit**

```bash
git add ai/prompts pipeline_feature.go
git commit -m "feat: move the feature pipeline prompts into ai/prompts"
```

---

## Task 4: Move the debug, rework, and triage prompts

**Files:**
- Create: `ai/prompts/debug.md.tmpl`, `ai/prompts/rework.md.tmpl`, `ai/prompts/triage.md.tmpl`
- Modify: `pipeline_bug.go:29-39`, `rework.go:61-69`, `triage.go` (the `triagePrompt` builder added in Task 1)
- Test: `prompts_golden_test.go` (unchanged)

**Interfaces:**
- Consumes: `mustRender`, `promptData` (Task 2); `triagePrompt(list string) string` (Task 1).
- Produces: nothing new.

- [ ] **Step 1: Create `ai/prompts/debug.md.tmpl`**

```
/superpowers:systematic-debugging {{.Issue}}

Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
{{.AlreadyDoneSentinel}} <one-sentence reason> on its own line and stop.
```

- [ ] **Step 2: Create `ai/prompts/rework.md.tmpl`**

```
Continue the work on this issue where the previous session left off.
Complete the remaining implementation, make the full test suite pass, and commit
all changes. HEADLESS: do not ask questions; make reasonable calls and note them
in commit messages.

If you find the work is already fully implemented, do not fabricate changes:
print {{.AlreadyDoneSentinel}} <one-sentence reason> on its own line and stop.
```

- [ ] **Step 3: Create `ai/prompts/triage.md.tmpl`**

```
You are a triage agent for an automated development pipeline.

Open eligible issues:
{{.List}}

Decide from the issue text alone — do NOT read the repository. Pick the single
best issue to work on next and classify it:
- "bug": a small, well-scoped defect that can be fixed by reproducing and debugging
- "feature": anything that needs design work (new functionality, refactors, unclear scope)

Respond with ONLY a JSON object, no other text:
{"issueNumber": <int>, "kind": "bug" or "feature", "reason": "<one sentence>"}
```

The JSON line uses single braces only, so `text/template` passes it through untouched.

- [ ] **Step 4: Rewrite the three builders**

`pipeline_bug.go` — replace lines 29–39:

```go
func bugPrompt(issue string) string {
	d := promptData()
	d["Issue"] = issue
	return mustRender("debug.md.tmpl", d)
}
```

`rework.go` — replace lines 61–69:

```go
func reworkPrompt() string {
	return mustRender("rework.md.tmpl", promptData())
}
```

`triage.go` — replace the `triagePrompt` body:

```go
func triagePrompt(list string) string {
	d := promptData()
	d["List"] = list
	return mustRender("triage.md.tmpl", d)
}
```

- [ ] **Step 5: Fix imports**

`pipeline_bug.go` no longer uses `fmt` — its import block becomes:

```go
import (
	"context"
)
```

`rework.go` still uses `fmt` (error wrapping in `Rework`) and `os`; leave its imports alone. `triage.go` still uses `fmt`; leave it alone.

Run: `go build ./... && go vet ./...`
Expected: no output (success). Any "imported and not used" error names the file to fix.

- [ ] **Step 6: Run the goldens and the affected suites**

Run: `go test ./... -run 'TestGolden|TestBugPipeline|Triage' -v`
Expected: PASS, including `pipeline_bug_test.go`'s substring assertions.

- [ ] **Step 7: Run the full suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add ai/prompts pipeline_bug.go rework.go triage.go
git commit -m "feat: move the debug, rework, and triage prompts into ai/prompts"
```

---

## Task 5: Extract `loop.go`'s outbound text into builders (Go literals, no templates yet)

Pure Go refactor: the inline `fmt.Sprintf` calls in `loop.go` move verbatim into named builders and get goldens. This is what makes Task 6's move a *checked* relocation — there is nothing to golden-test while the strings are inline at their call sites.

**Files:**
- Modify: `loop.go` (`handleIssue`, `finishDone`, `finishNeedsInfo`, `classifyCause`, `park`, `ship`)
- Modify: `prompts_golden_test.go` (append)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, all in `loop.go`:
  - `func pickupComment(kind, branch string) string`
  - `func alreadyDoneComment(reason string) string`
  - `func needsInfoComment(score int, label, feedback string) string`
  - `func parkComment(n int, guidance, errText string) string`
  - `func prComment(url string) string`
  - `func prTitle(title string, n int) string`
  - `func prBody(n int, kind string) string`

- [ ] **Step 1: Add the builders at the bottom of `loop.go`**

```go
func pickupComment(kind, branch string) string {
	return fmt.Sprintf("🤖 Picked up (%s flow). Branch: `%s`", kind, branch)
}

func alreadyDoneComment(reason string) string {
	return fmt.Sprintf("🤖 Already implemented — closing. %s", reason)
}

func needsInfoComment(score int, label, feedback string) string {
	return fmt.Sprintf("🤖 Not confident enough to implement (confidence %d/100). Please clarify and remove the `%s` label to re-queue:\n\n%s",
		score, label, feedback)
}

func parkComment(n int, guidance, errText string) string {
	c := fmt.Sprintf("🤖 Parked for rework — run `loop -rework %d -config <cfg>`.", n)
	if guidance != "" {
		c += "\n" + guidance
	}
	if errText != "" {
		c += fmt.Sprintf("\nError: %s", errText)
	}
	return c
}

func prComment(url string) string {
	return "🤖 PR: " + url
}

func prTitle(title string, n int) string {
	return fmt.Sprintf("%s (#%d)", title, n)
}

func prBody(n int, kind string) string {
	return fmt.Sprintf("Closes #%d\n\nAutomated by loope (%s flow). Spec and plan, if any, are committed in this branch under docs/.", n, kind)
}
```

The `if errText != ""` guard is new (today the tail is unconditional). It is behaviour-identical for every current caller — `park` always passes a non-empty `tail(cause.Error(), 800)` — and gives the template in Task 6 a defined empty case.

- [ ] **Step 2: Point the six call sites at the builders**

`handleIssue` (`loop.go:166`):

```go
	_ = o.gh.Comment(ctx, n, pickupComment(kind, branch))
```

`finishDone`:

```go
	_ = o.gh.Comment(cctx, n, alreadyDoneComment(reason))
```

`finishNeedsInfo` — replace the `body := fmt.Sprintf(...)` and the `Comment` call with:

```go
	_ = o.gh.Comment(cctx, n, needsInfoComment(lc.score, o.cfg.StateLabels.NeedsInfo, lc.feedback))
```

`park` — replace the `comment := ...` block inside the `if !(fromLabel == … && resumable)` guard with:

```go
		_ = o.gh.Comment(cctx, n, parkComment(n, guidance, tail(cause.Error(), 800)))
```

`ship` — the `CreatePR` call and the PR comment:

```go
	url, err := o.gh.CreatePR(ctx, branch, prTitle(issue.Title, n), prBody(n, kind))
	if err != nil {
		return onInfra(err)
	}
	_ = o.gh.Comment(ctx, n, prComment(url))
```

- [ ] **Step 3: Append goldens for the new builders**

Add to `prompts_golden_test.go`. These use interpreted (double-quoted) string literals because the expected text contains backticks.

```go
func TestGoldenPickupComment(t *testing.T) {
	check(t, "pickupComment", pickupComment("feature", "ai/issue-12"),
		"🤖 Picked up (feature flow). Branch: `ai/issue-12`")
}

func TestGoldenAlreadyDoneComment(t *testing.T) {
	check(t, "alreadyDoneComment", alreadyDoneComment("The flag already exists."),
		"🤖 Already implemented — closing. The flag already exists.")
}

func TestGoldenNeedsInfoComment(t *testing.T) {
	check(t, "needsInfoComment", needsInfoComment(42, "ai-needs-info", "Which database?"),
		"🤖 Not confident enough to implement (confidence 42/100). Please clarify and remove the `ai-needs-info` label to re-queue:\n\nWhich database?")
}

func TestGoldenParkCommentFull(t *testing.T) {
	check(t, "parkComment(guidance+error)", parkComment(12, "Cause: network outage — the loop auto-resumes when connectivity returns.", "dial tcp: i/o timeout"),
		"🤖 Parked for rework — run `loop -rework 12 -config <cfg>`.\nCause: network outage — the loop auto-resumes when connectivity returns.\nError: dial tcp: i/o timeout")
}

func TestGoldenParkCommentNoGuidance(t *testing.T) {
	check(t, "parkComment(error only)", parkComment(12, "", "boom"),
		"🤖 Parked for rework — run `loop -rework 12 -config <cfg>`.\nError: boom")
}

func TestGoldenParkCommentNoError(t *testing.T) {
	check(t, "parkComment(guidance only)", parkComment(12, "Cause: x.", ""),
		"🤖 Parked for rework — run `loop -rework 12 -config <cfg>`.\nCause: x.")
}

func TestGoldenParkCommentBare(t *testing.T) {
	check(t, "parkComment(bare)", parkComment(12, "", ""),
		"🤖 Parked for rework — run `loop -rework 12 -config <cfg>`.")
}

func TestGoldenPRComment(t *testing.T) {
	check(t, "prComment", prComment("https://example.test/pr/1"), "🤖 PR: https://example.test/pr/1")
}

func TestGoldenPRTitle(t *testing.T) {
	check(t, "prTitle", prTitle("Externalize prompts", 12), "Externalize prompts (#12)")
}

func TestGoldenPRBody(t *testing.T) {
	check(t, "prBody", prBody(12, "feature"),
		"Closes #12\n\nAutomated by loope (feature flow). Spec and plan, if any, are committed in this branch under docs/.")
}

func TestGoldenClassifyCauseGuidance(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"session limit reached", "Cause: Claude usage/rate limit — the loop auto-resumes it (with backoff) once the limit resets."},
		{"hit max_turns", "Cause: hit the turn/budget ceiling mid-run — the loop auto-resumes where it stopped (raise the execute maxTurns/maxBudgetUSD if this recurs)."},
		{"interrupted mid-run", "Cause: the daemon restarted while this issue was mid-run — the loop auto-resumes the preserved session."},
		{"dial tcp: i/o timeout", "Cause: network outage — the loop auto-resumes when connectivity returns."},
	}
	for _, tc := range cases {
		got, resumable := classifyCause(tc.msg)
		if !resumable {
			t.Errorf("classifyCause(%q) resumable = false, want true", tc.msg)
		}
		check(t, "classifyCause("+tc.msg+")", got, tc.want)
	}
}
```

- [ ] **Step 4: Run the new goldens**

Run: `go test ./... -run TestGolden -v`
Expected: all PASS. `TestGoldenClassifyCauseGuidance` uses `i/o timeout`, which must be one of `transientSignatures` in `retry.go` — if that case fails on `resumable`, substitute a signature that is actually in that slice and keep the guidance text identical.

- [ ] **Step 5: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add loop.go prompts_golden_test.go
git commit -m "refactor: extract loop.go outbound text into builders with goldens"
```

---

## Task 6: Move the outbound text into `comments.md.tmpl`

**Files:**
- Create: `ai/prompts/comments.md.tmpl`
- Modify: `loop.go` (the seven builders from Task 5, plus `classifyCause`)
- Test: `prompts_golden_test.go` (unchanged — the Task 5 goldens are the contract)

**Interfaces:**
- Consumes: `mustRender`, `promptData` (Task 2); the seven builders from Task 5.
- Produces: template names `pickup`, `already-done`, `needs-info`, `park`, `pr-comment`, `pr-title`, `pr-body`, `guidance-usage-limit`, `guidance-budget`, `guidance-interrupted`, `guidance-network`. Builder signatures are unchanged.

- [ ] **Step 1: Create `ai/prompts/comments.md.tmpl`**

One file rather than eleven, because each block is a single line and a file per one-liner would be noise. Each `{{define}}` body starts immediately after the opening action and ends immediately before `{{end}}`, so no block carries a stray newline.

```
{{define "pickup"}}🤖 Picked up ({{.Kind}} flow). Branch: `{{.Branch}}`{{end}}

{{define "already-done"}}🤖 Already implemented — closing. {{.Reason}}{{end}}

{{define "needs-info"}}🤖 Not confident enough to implement (confidence {{.Score}}/100). Please clarify and remove the `{{.Label}}` label to re-queue:

{{.Feedback}}{{end}}

{{define "park"}}🤖 Parked for rework — run `loop -rework {{.Number}} -config <cfg>`.{{if .Guidance}}
{{.Guidance}}{{end}}{{if .Error}}
Error: {{.Error}}{{end}}{{end}}

{{define "pr-comment"}}🤖 PR: {{.URL}}{{end}}

{{define "pr-title"}}{{.Title}} (#{{.Number}}){{end}}

{{define "pr-body"}}Closes #{{.Number}}

Automated by loope ({{.Kind}} flow). Spec and plan, if any, are committed in this branch under docs/.{{end}}

{{define "guidance-usage-limit"}}Cause: Claude usage/rate limit — the loop auto-resumes it (with backoff) once the limit resets.{{end}}

{{define "guidance-budget"}}Cause: hit the turn/budget ceiling mid-run — the loop auto-resumes where it stopped (raise the execute maxTurns/maxBudgetUSD if this recurs).{{end}}

{{define "guidance-interrupted"}}Cause: the daemon restarted while this issue was mid-run — the loop auto-resumes the preserved session.{{end}}

{{define "guidance-network"}}Cause: network outage — the loop auto-resumes when connectivity returns.{{end}}
```

- [ ] **Step 2: Rewrite the seven builders**

Replace the Task 5 bodies in `loop.go`:

```go
func pickupComment(kind, branch string) string {
	d := promptData()
	d["Kind"] = kind
	d["Branch"] = branch
	return mustRender("pickup", d)
}

func alreadyDoneComment(reason string) string {
	d := promptData()
	d["Reason"] = reason
	return mustRender("already-done", d)
}

func needsInfoComment(score int, label, feedback string) string {
	d := promptData()
	d["Score"] = score
	d["Label"] = label
	d["Feedback"] = feedback
	return mustRender("needs-info", d)
}

func parkComment(n int, guidance, errText string) string {
	d := promptData()
	d["Number"] = n
	d["Guidance"] = guidance
	d["Error"] = errText
	return mustRender("park", d)
}

func prComment(url string) string {
	d := promptData()
	d["URL"] = url
	return mustRender("pr-comment", d)
}

func prTitle(title string, n int) string {
	d := promptData()
	d["Title"] = title
	d["Number"] = n
	return mustRender("pr-title", d)
}

func prBody(n int, kind string) string {
	d := promptData()
	d["Number"] = n
	d["Kind"] = kind
	return mustRender("pr-body", d)
}
```

- [ ] **Step 3: Rewrite `classifyCause`'s returned sentences**

Only the returned strings change. The `switch`, the match order, the panic guard, and every `resumable` boolean stay exactly as they are:

```go
	switch {
	case strings.Contains(m, "session limit") || strings.Contains(m, "usage limit") ||
		strings.Contains(m, "rate limit") || strings.Contains(m, "api status 429"):
		return mustRender("guidance-usage-limit", promptData()), true
	case strings.Contains(m, "max_turns") || strings.Contains(m, "max turns") ||
		strings.Contains(m, "max-budget") || strings.Contains(m, "budget"):
		return mustRender("guidance-budget", promptData()), true
	case strings.Contains(m, "interrupted mid-run"):
		return mustRender("guidance-interrupted", promptData()), true
	}
	for _, sig := range transientSignatures {
		if strings.Contains(m, sig) {
			return mustRender("guidance-network", promptData()), true
		}
	}
	return "", false
```

- [ ] **Step 4: Run the goldens**

Run: `go test ./... -run TestGolden -v`
Expected: all PASS, unchanged from Task 5. A mismatch here is whitespace inside a `{{define}}` block — fix the template.

- [ ] **Step 5: Run build, vet, and the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS. If `fmt` is now unused in `loop.go`, remove it from the import block (it is still used by the error-wrapping paths in `finishDone`, `finishNeedsInfo`, and `ship`, so this is unlikely).

- [ ] **Step 6: Commit**

```bash
git add ai/prompts/comments.md.tmpl loop.go
git commit -m "feat: move GitHub comment and PR text into ai/prompts"
```

---

## Task 7: Full-FS render test, README note, and end-to-end single-binary verification

Closes the two risks in the spec: a prompt file added but never wired up, and a runtime read from disk.

**Files:**
- Create: `prompts_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `prompts` (`*template.Template`), `promptFS`, `mustRender`, `promptData` (Task 2), and every template created in Tasks 2–6.
- Produces: nothing consumed by later tasks (final task).

- [ ] **Step 1: Write the full-FS render test**

Create `prompts_test.go`. It renders **every** template in the embedded set with representative data. That is what makes `mustRender`'s panic safe, and a new `.tmpl` file with no entry in `promptTestData` fails the test.

```go
package main

import (
	"strings"
	"testing"
)

// Representative data for every renderable template in the embedded FS. A new
// prompt file or {{define}} block with no entry here fails TestEveryTemplateRenders —
// which is the point: it catches a prompt that was added but never wired up.
var promptTestData = map[string]map[string]any{
	"brainstorm.md.tmpl":   {"Issue": "I", "Threshold": 70},
	"answerer.md.tmpl":     {"Issue": "I", "Persona": "P", "ArchitectMsg": "A"},
	"done-confirm.md.tmpl": {"Issue": "I", "Persona": "P", "Reason": "R"},
	"plan.md.tmpl":         {"SpecPath": "docs/spec.md"},
	"execute.md.tmpl":      {"PlanPath": "docs/plan.md"},
	"debug.md.tmpl":        {"Issue": "I"},
	"rework.md.tmpl":       {},
	"triage.md.tmpl":       {"List": "[]"},
	"pickup":               {"Kind": "feature", "Branch": "b"},
	"already-done":         {"Reason": "R"},
	"needs-info":           {"Score": 1, "Label": "l", "Feedback": "F"},
	"park":                 {"Number": 1, "Guidance": "G", "Error": "E"},
	"pr-comment":           {"URL": "u"},
	"pr-title":             {"Title": "T", "Number": 1},
	"pr-body":              {"Number": 1, "Kind": "bug"},
	"guidance-usage-limit": {},
	"guidance-budget":      {},
	"guidance-interrupted": {},
	"guidance-network":     {},
}

// skipTemplates are the two names in the set that are not prompts: the root
// template ParseFS was seeded with, and the container file whose own body is
// just the whitespace between its {{define}} blocks.
var skipTemplates = map[string]bool{"prompts": true, "comments.md.tmpl": true}

func TestEveryTemplateRenders(t *testing.T) {
	for _, tmpl := range prompts.Templates() {
		name := tmpl.Name()
		if skipTemplates[name] {
			continue
		}
		data, ok := promptTestData[name]
		if !ok {
			t.Errorf("template %q has no entry in promptTestData — add one (a prompt with no test data is a prompt nobody renders)", name)
			continue
		}
		d := promptData()
		for k, v := range data {
			d[k] = v
		}
		got := mustRender(name, d)
		if strings.TrimSpace(got) == "" {
			t.Errorf("template %q rendered empty", name)
		}
		if strings.Contains(got, "<no value>") {
			t.Errorf("template %q rendered a <no value> placeholder:\n%s", name, got)
		}
		if strings.HasSuffix(got, "\n") {
			t.Errorf("template %q kept its trailing newline; mustRender must trim it", name)
		}
	}
}

// Every .md.tmpl file on disk must have made it into the binary.
func TestEveryPromptFileIsEmbedded(t *testing.T) {
	entries, err := promptFS.ReadDir("ai/prompts")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("ai/prompts embedded empty")
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md.tmpl") {
			t.Errorf("unexpected file in ai/prompts: %s (only .md.tmpl files are parsed)", e.Name())
		}
	}
}

// Sentinels come from the Go constants, never from literal text in a template.
func TestNoSentinelIsHardcodedInATemplate(t *testing.T) {
	entries, err := promptFS.ReadDir("ai/prompts")
	if err != nil {
		t.Fatal(err)
	}
	sentinels := []string{confidenceSentinel, specReadySentinel, readySentinel, alreadyDoneSentinel, doneConfirmSentinel}
	for _, e := range entries {
		b, err := promptFS.ReadFile("ai/prompts/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sentinels {
			if strings.Contains(string(b), s) {
				t.Errorf("%s hardcodes the sentinel %q — inject it via promptData() instead", e.Name(), s)
			}
		}
	}
}
```

- [ ] **Step 2: Run the new tests**

Run: `go test ./... -run 'TestEvery|TestNoSentinel' -v`
Expected: all PASS.

If `TestEveryTemplateRenders` reports a name you did not expect, reconcile it — that is the test doing its job. If it reports `comments.md.tmpl` rendering empty, confirm it is in `skipTemplates`.

- [ ] **Step 3: Run the whole suite with the race detector**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: PASS. The template set is parsed once at init and only ever executed (never re-parsed), so concurrent pipeline goroutines rendering prompts is safe; `-race` confirms it.

- [ ] **Step 4: Document the directory in the README**

Add this section to `README.md`, immediately after the existing configuration section (place it wherever the surrounding structure reads naturally; match the file's existing heading level for a top-level section):

```markdown
## Prompts

Every prompt loope sends to Claude, and every comment it posts to GitHub, lives
in [`ai/prompts/`](ai/prompts) as a `text/template` file — no prompt text is in
the Go source. The directory is embedded into the binary with `go:embed`, so a
release is still a single self-contained file that reads nothing from disk at
runtime; editing a prompt means rebuilding.

Sentinel tokens (`CONFIDENCE:`, `SPEC_READY:`, `PIPELINE_READY`,
`PIPELINE_ALREADY_DONE:`, `DONE_CONFIRMED`) are injected from the Go constants
rather than written in the templates, so the instruction given to the model and
the parser reading its reply cannot drift apart. Rewording a prompt is safe;
adding a placeholder means adding the matching key in the builder, and the
tests in `prompts_test.go` will fail loudly if you forget.
```

- [ ] **Step 5: Prove the binary reads nothing from disk**

Build a release-shaped binary, move `ai/` out of the way, and run it:

```bash
goreleaser release --snapshot --clean
```

If `goreleaser` is not installed, this equivalent proves the same property:

```bash
go build -o /tmp/loope-embedcheck .
```

Then, with the binary built by either command:

```bash
mv ai /tmp/ai-moved-away
/tmp/loope-embedcheck -h 2>&1 | head -5
mv /tmp/ai-moved-away ai
```

Expected: the binary prints its usage/flags exactly as before, with no error about a missing `ai/prompts` directory. (`go test` cannot be used for this check — the test binary is built from the same package and embeds the same files, but running `go build` requires the source tree, so the `mv` must happen after the build.)

**Restore `ai/` before continuing** — `git status` must show it back in place.

Run: `git status --short && go test ./...`
Expected: `ai/` present, full suite PASS.

- [ ] **Step 6: Confirm no prompt text is left in the Go source**

Run:

```bash
grep -n "superpowers:\|HEADLESS\|🤖" *.go | grep -v _test.go
```

Expected: **no matches.** Any hit is a prompt fragment that was missed. (`_test.go` files legitimately contain this text — they are the goldens.)

Run: `grep -c "" go.mod && cat go.mod`
Expected: `go.mod` still has only the `module` and `go` lines — no `require` block.

- [ ] **Step 7: Commit**

```bash
git add prompts_test.go README.md
git commit -m "test: render every embedded prompt template; document ai/prompts"
```

---

## Final verification checklist

Run all of these from the repository root before calling the work done:

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

All must be green. In addition:

- `git status --short` is clean and `ai/prompts/` contains exactly nine `.md.tmpl` files.
- `grep -n "superpowers:\|HEADLESS\|🤖" *.go | grep -v _test.go` returns nothing.
- `go.mod` has no `require` block.
- `.goreleaser.yaml` is unmodified (`git diff main -- .goreleaser.yaml` is empty).
- `serve.go` and `ui.go` are unmodified (`git diff main -- serve.go ui.go` is empty).
