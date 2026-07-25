# UAT Checklist Brevity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the published `## 🤖 UAT checklist` a flat, one-line-per-item list by replacing the two duplicated rule lists in the UAT prompts with a single shared `uat-format` template block.

**Architecture:** `ai/prompts/uat-format.md.tmpl` holds a `{{define "uat-format"}}` block, exactly the way `ask-format.md.tmpl` already does for the confidence pushback comment. `uat-feature.md.tmpl` and `uat-bug.md.tmpl` end with `{{template "uat-format" .}}` instead of their own rules. The one route-specific phrase is passed in as a `UATCoverage` template value set by `uatFeaturePrompt` / `uatBugPrompt` in `uat.go`. No Go control flow, no new enforcement code — brevity comes from the prompt rules alone.

**Tech Stack:** Go 1.x, `text/template` + `embed` (see `prompts.go`), standard `testing` package. No new dependencies.

## Global Constraints

Copied from the spec (`docs/superpowers/specs/2026-07-25-uat-checklist-brevity-design.md`); every task's requirements implicitly include these:

- Coverage is never traded for length. Every behavior the spec (or fix) describes still gets an item, including error and edge cases. Only the wording is compressed.
- Item shape is `Action → expected result`, 15 words or fewer, not a prose sentence.
- The checklist is a single flat ungrouped list of `- [ ]` checkboxes; no `###` group headings.
- No enforcement outside the prompt. `maxUATChars` stays at `8000`. No new "too long, drop it" path.
- Both routes follow identical rules; `## 🤖 UAT checklist` gains no lead sentence.
- Do **not** touch: `maxUATChars`, `maxIssueBodyChars`, `parseUAT`, `uatSection`, the `## 🤖 UAT checklist` heading, or the pipeline's non-blocking contract.
- Sentinels are never written as literal text in a `.tmpl` file; they come from `promptData()` (enforced by `TestNoSentinelIsHardcodedInATemplate`).
- `ai/prompts/` stays flat — no subdirectories (enforced by `TestEveryPromptFileOnDiskIsParsed`).

## Assumptions

Recorded because the spec left them open and headless mode forbids asking:

1. **Bullet line wrapping.** The spec quotes the new rules block with hard wraps at ~76 columns, but that is the spec's own prose wrapping. `ask-format.md.tmpl` — which this block is explicitly modelled on — writes each bullet as one unwrapped line. This plan writes each rule as **one unwrapped line**, matching `ask-format`. The rendered text is what the model reads; wrapping is cosmetic.
2. **Coverage phrasing.** `UATCoverage` renders as `every behavior the spec describes` (feature) and `the reported bug and every behavior the fix touches` (bug). The trailing `, including its error and edge cases, but do not invent scope beyond it.` lives in the shared block, so it is written once and reads correctly for both values.
3. **Template name.** The `{{define}}` name is `uat-format`, the file is `uat-format.md.tmpl` — same file/define naming relationship as `ask-format`.

---

## File Structure

| File | Responsibility |
|---|---|
| `ai/prompts/uat-format.md.tmpl` (new) | The single copy of the checklist rules, as `{{define "uat-format"}}`. Consumes `.UATCoverage`. |
| `ai/prompts/uat-feature.md.tmpl` (modify) | Feature framing + sentinel contract; delegates rules to `{{template "uat-format" .}}`. |
| `ai/prompts/uat-bug.md.tmpl` (modify) | Bug framing, diff inspection, empty-diff self-skip, sentinel contract; delegates rules to `{{template "uat-format" .}}`. |
| `uat.go` (modify) | `uatFeaturePrompt` / `uatBugPrompt` each set `d["UATCoverage"]`. Nothing else changes. |
| `prompts_test.go` (modify) | Register `uat-format` in `promptTestData`, add `uat-format.md.tmpl` to `skipTemplates`, add `TestUATFormatBlockCarriesItsRules` and `TestBothRoutesShareTheUATFormatBlock`. |
| `prompts_golden_test.go` (modify) | Update the two golden prompt strings to the new rendered text. |
| `docs/how-it-works.md` (modify) | One sentence describing the checklist's shape. |

---

### Task 1: Shared `uat-format` block wired into both routes

Everything in this task ships together: a half-applied change (new file, prompts not switched over) leaves duplicated rules on disk, which is the exact defect being fixed.

**Files:**
- Create: `ai/prompts/uat-format.md.tmpl`
- Modify: `ai/prompts/uat-feature.md.tmpl` (replace the whole `Rules for the checklist:` list)
- Modify: `ai/prompts/uat-bug.md.tmpl` (replace the whole `Rules for the checklist:` list)
- Modify: `uat.go` — `uatFeaturePrompt`, `uatBugPrompt`
- Test: `prompts_test.go`, `prompts_golden_test.go`

**Interfaces:**
- Consumes: `mustRender(name string, data map[string]any) string` and `promptData() map[string]any` from `prompts.go`.
- Produces: template `uat-format`, requiring key `UATCoverage` (string). `uatFeaturePrompt(specPath string) string` and `uatBugPrompt(issue, base string) string` keep their existing signatures.

- [ ] **Step 1: Write the failing rules-block test**

Add to `prompts_test.go`, immediately after `TestAskFormatBlockCarriesItsRules`:

```go
// The checklist-format instruction lives in exactly one place. This asserts the
// block exists and still carries the rules that keep the published checklist
// scannable — a silent deletion of one bullet would otherwise pass every other
// test in this file.
func TestUATFormatBlockCarriesItsRules(t *testing.T) {
	d := promptData()
	d["UATCoverage"] = "every behavior the spec describes"
	got := mustRender("uat-format", d)
	for _, want := range []string{
		"single flat list",
		"No headings",
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
	if strings.Contains(got, "###") {
		t.Errorf("uat-format block still asks for `###` group headings:\n%s", got)
	}
	if strings.Contains(got, "under 20 items") {
		t.Errorf("uat-format block still caps the item count:\n%s", got)
	}
}

// Both routes must ask in the same shape, from the same source. This is what
// catches an edit made to one UAT prompt that should have been an edit to the
// shared block.
func TestBothRoutesShareTheUATFormatBlock(t *testing.T) {
	feature := promptData()
	feature["UATCoverage"] = "every behavior the spec describes"
	if block := mustRender("uat-format", feature); !strings.Contains(uatFeaturePrompt("docs/spec.md"), block) {
		t.Error("uatFeaturePrompt does not contain the uat-format block")
	}
	bug := promptData()
	bug["UATCoverage"] = "the reported bug and every behavior the fix touches"
	if block := mustRender("uat-format", bug); !strings.Contains(uatBugPrompt("I", "main"), block) {
		t.Error("uatBugPrompt does not contain the uat-format block")
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test ./... -run 'TestUATFormatBlockCarriesItsRules|TestBothRoutesShareTheUATFormatBlock' -v`

Expected: FAIL — `mustRender` panics with `render prompt "uat-format": html/template: "uat-format" is undefined` (the template does not exist yet).

- [ ] **Step 3: Create the shared format block**

Create `ai/prompts/uat-format.md.tmpl` with exactly this content (one trailing newline at end of file; `mustRender` trims it):

```
{{define "uat-format"}}Rules for the checklist:
- A single flat list of Markdown `- [ ]` checkboxes. No headings, no grouping, no intro line, no closing line.
- Each item is `Action → expected result`: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover {{.UATCoverage}}, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.{{end}}
```

Note the `→` is U+2192 RIGHTWARDS ARROW, and there is no newline between `{{define "uat-format"}}` and `Rules` — the block must start flush, the same way `ask-format.md.tmpl` does.

- [ ] **Step 4: Register the new template in the test fixtures**

In `prompts_test.go`, add an entry to `promptTestData` (keep it next to the other `uat-` entries):

```go
	"uat-format":           {"UATCoverage": "C"},
	"uat-section":          {"Checklist": "- [ ] C"},
	"uat-feature.md.tmpl":  {"SpecPath": "docs/spec.md"},
	"uat-bug.md.tmpl":      {"Issue": "I", "Base": "main"},
```

And add the container file to `skipTemplates`:

```go
var skipTemplates = map[string]bool{
	"prompts":             true,
	"comments.md.tmpl":    true,
	"ask-format.md.tmpl":  true,
	"uat-format.md.tmpl":  true,
}
```

(`uat-format.md.tmpl` — the *file* template — renders to just the whitespace around its `{{define}}`, so it is skipped; `uat-format` — the *block* — is rendered and asserted.)

- [ ] **Step 5: Point `uat-feature.md.tmpl` at the shared block**

Replace the whole `Rules for the checklist:` list at the end of `ai/prompts/uat-feature.md.tmpl`, so the full file reads:

```
Read the approved spec at {{.SpecPath}} and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Output ONLY the checklist, between a line reading {{.UATBeginSentinel}} and a line reading
{{.UATEndSentinel}}. Print nothing before or after those two lines.

{{template "uat-format" .}}
```

- [ ] **Step 6: Point `uat-bug.md.tmpl` at the shared block**

Replace the whole `Rules for the checklist:` list at the end of `ai/prompts/uat-bug.md.tmpl`, so the full file reads:

```
A bug fix has just been committed on this branch. Write a UAT (user acceptance
test) checklist for a human who will verify the fix by hand.

The GitHub issue being fixed:
{{.Issue}}

Read the issue above, then inspect what actually changed with
`git diff origin/{{.Base}}...HEAD` and `git log origin/{{.Base}}..HEAD`, so the checklist
describes the real fix. If that diff is empty — nothing was committed — print
nothing at all: no markers, no checklist, no explanation.

Output ONLY the checklist, between a line reading {{.UATBeginSentinel}} and a line reading
{{.UATEndSentinel}}. Print nothing before or after those two lines.

{{template "uat-format" .}}
```

The framing paragraph, the diff-inspection instruction, the empty-diff self-skip and the sentinel contract are all unchanged — only the rules list is replaced.

- [ ] **Step 7: Set `UATCoverage` in both prompt builders**

In `uat.go`, update the two builders (the doc comments above them stay as they are):

```go
func uatFeaturePrompt(specPath string) string {
	d := promptData()
	d["SpecPath"] = specPath
	d["UATCoverage"] = "every behavior the spec describes"
	return mustRender("uat-feature.md.tmpl", d)
}
```

```go
func uatBugPrompt(issue, base string) string {
	d := promptData()
	d["Issue"] = issue
	d["Base"] = base
	d["UATCoverage"] = "the reported bug and every behavior the fix touches"
	return mustRender("uat-bug.md.tmpl", d)
}
```

- [ ] **Step 8: Run the new tests to verify they pass**

Run: `go test ./... -run 'TestUATFormatBlockCarriesItsRules|TestBothRoutesShareTheUATFormatBlock|TestEveryTemplateRenders|TestEveryPromptFileOnDiskIsParsed|TestNoSentinelIsHardcodedInATemplate' -v`

Expected: PASS for all five. If `TestEveryTemplateRenders` fails with "has no entry in promptTestData", Step 4 was missed.

- [ ] **Step 9: Run the golden prompt tests to see them fail**

Run: `go test ./... -run 'TestGoldenUAT' -v`

Expected: FAIL for `TestGoldenUATFeaturePrompt` and `TestGoldenUATBugPrompt` — they still pin the old `###`-headings rules text. `TestGoldenUATSection` must still PASS; if it does not, `uatSection` was touched, which the Global Constraints forbid.

- [ ] **Step 10: Update the two golden prompt strings**

In `prompts_golden_test.go`, replace the body of `TestGoldenUATFeaturePrompt` with:

```go
func TestGoldenUATFeaturePrompt(t *testing.T) {
	want := `Read the approved spec at docs/spec.md and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Output ONLY the checklist, between a line reading UAT_BEGIN and a line reading
UAT_END. Print nothing before or after those two lines.

Rules for the checklist:
- A single flat list of Markdown ` + "`- [ ]`" + ` checkboxes. No headings, no grouping, no intro line, no closing line.
- Each item is ` + "`Action → expected result`" + `: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover every behavior the spec describes, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.`
	check(t, "uatFeaturePrompt", uatFeaturePrompt("docs/spec.md"), want)
}
```

And the body of `TestGoldenUATBugPrompt` with:

```go
func TestGoldenUATBugPrompt(t *testing.T) {
	want := `A bug fix has just been committed on this branch. Write a UAT (user acceptance
test) checklist for a human who will verify the fix by hand.

The GitHub issue being fixed:
ISSUE BODY

Read the issue above, then inspect what actually changed with
` + "`git diff origin/main...HEAD`" + ` and ` + "`git log origin/main..HEAD`" + `, so the checklist
describes the real fix. If that diff is empty — nothing was committed — print
nothing at all: no markers, no checklist, no explanation.

Output ONLY the checklist, between a line reading UAT_BEGIN and a line reading
UAT_END. Print nothing before or after those two lines.

Rules for the checklist:
- A single flat list of Markdown ` + "`- [ ]`" + ` checkboxes. No headings, no grouping, no intro line, no closing line.
- Each item is ` + "`Action → expected result`" + `: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover the reported bug and every behavior the fix touches, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.`
	check(t, "uatBugPrompt", uatBugPrompt("ISSUE BODY", "main"), want)
}
```

Backticks inside a Go raw string literal must be concatenated as `+ "`...`" +`, which is why the checkbox and arrow fragments are spliced in — follow the existing style in this file exactly.

- [ ] **Step 11: Run the golden tests to verify they pass**

Run: `go test ./... -run 'TestGoldenUAT' -v`

Expected: PASS. `check` prints a diff of got vs want on failure; if it fails, copy the *got* text into the literal only after confirming it matches the block in Step 3 — a mismatch usually means a stray blank line between the sentinel paragraph and `{{template "uat-format" .}}`.

- [ ] **Step 12: Run the whole suite**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: all PASS. `uat_test.go` exercises `parseUAT` / `RunFeature` / `RunBug` against fakes and does not assert prompt text, so it should be untouched by this change; if it fails, something outside the prompt text was modified.

- [ ] **Step 13: Commit**

```bash
git add ai/prompts/uat-format.md.tmpl ai/prompts/uat-feature.md.tmpl ai/prompts/uat-bug.md.tmpl uat.go prompts_test.go prompts_golden_test.go
git commit -m "feat: flat, one-line UAT checklist items via a shared uat-format block"
```

---

### Task 2: Document the checklist's shape

**Files:**
- Modify: `docs/how-it-works.md:29-35` (the `## UAT checklist` section)

**Interfaces:**
- Consumes: nothing. Documentation only.
- Produces: nothing other tasks depend on.

- [ ] **Step 1: Add the shape sentence**

In `docs/how-it-works.md`, in the `## UAT checklist` section, insert a new paragraph after the paragraph ending "…so a re-queued run leaves the first one in place." and before "The step is deliberately non-blocking.":

```markdown
The checklist itself is a single flat list of `- [ ]` items — no group headings —
each written as `Action → expected result` in 15 words or fewer, so the whole
verification pass fits on one screen. Both routes render the same rules from
`ai/prompts/uat-format.md.tmpl`; brevity is a prompt rule, not a length cap in
code, and coverage is never traded for it.
```

- [ ] **Step 2: Verify nothing else in the doc contradicts it**

Run: `grep -n -i 'uat\|checklist\|###' docs/how-it-works.md`

Expected: no remaining sentence describing the checklist as grouped, headed, or capped at a number of items. If one exists, delete or reword it in place.

- [ ] **Step 3: Commit**

```bash
git add docs/how-it-works.md
git commit -m "docs: describe the flat, one-line UAT checklist shape"
```

---

## Manual acceptance evidence

The checklist is model-generated, so its output is not unit-testable. Per the spec, the golden prompt is the contract and the checklist published on issue #44 is the acceptance evidence: it should be a flat list, no `###` headings, each item one line of the form `Action → expected result`.

## Self-Review

**Spec coverage:**

| Spec requirement | Task |
|---|---|
| New `ai/prompts/uat-format.md.tmpl` with `{{define "uat-format"}}` | 1, Step 3 |
| Both UAT prompts end with `{{template "uat-format" .}}` | 1, Steps 5–6 |
| `UATCoverage` set by `uatFeaturePrompt` / `uatBugPrompt` | 1, Step 7 |
| New rules text, flat list, `Action → expected result`, 15 words | 1, Step 3 |
| "aim for under 20 items" removed | 1, Step 3 (asserted in Step 1's test) |
| `uat-format` registered in `promptTestData`, file in `skipTemplates` | 1, Step 4 |
| `TestUATFormatBlockCarriesItsRules` | 1, Step 1 |
| Shared-not-duplicated test paralleling the `ask-format` one | 1, Step 1 (`TestBothRoutesShareTheUATFormatBlock`) |
| Two golden prompt strings updated | 1, Step 10 |
| `docs/how-it-works.md` sentence | 2 |
| Nothing else touched (`maxUATChars`, `parseUAT`, `uatSection`, heading, non-blocking contract) | Global Constraints; guarded by Step 9 and Step 12 |

No gaps.

**Placeholder scan:** no TBD / "handle edge cases" / "similar to Task N" — every template and Go body is written out in full, and both golden literals are given complete rather than described.

**Type consistency:** the template name is `uat-format` and the data key is `UATCoverage` in every place they appear (Steps 1, 3, 4, 7, 10). The file name is `uat-format.md.tmpl` in the embed glob, `skipTemplates`, the docs sentence, and the commit. `uatFeaturePrompt` / `uatBugPrompt` signatures are unchanged, so no caller in `uat.go` or the pipeline files needs updating.
