# UAT checklist published to the issue body — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After the feature pipeline commits its spec, and after the bug pipeline fixes the bug, a short ephemeral Claude session writes a human-verifiable UAT checklist and appends it to the GitHub issue body — once per issue, never blocking the pipeline.

**Architecture:** A new `uat.go` holds a `*UAT` value (nil-safe, injected by `handleIssue`) whose two entry points — `RunFeature` and `RunBug` — share one unexported `run` that fetches the issue body, skips if the idempotency marker is already there, runs one read-only `claude` session with `Label: "uat"`, extracts the checklist between two sentinels, size-guards it, and appends a rendered section to the issue body. Every failure path logs and returns; nothing propagates to the pipeline. Two new GitHub methods (`IssueBody`, `AppendIssueBody`) do the read-modify-write through the existing retrying `g.gh` helper.

**Tech Stack:** Go (stdlib only — `go.mod` has no dependencies), `text/template` prompts embedded from `ai/prompts/`, the `gh` CLI behind the `Runner` seam, table-free hand-rolled Go tests with the existing `fakeRunner`.

## Global Constraints

- Package is flat `package main` at the repo root; every new file is a sibling of `claude.go`.
- No new third-party dependencies. `go.mod` stays at its current two lines.
- Prompt sentinels are **never** written as literal text inside a `.tmpl` file — they are injected through `promptData()` (enforced by `TestNoSentinelIsHardcodedInATemplate` in `prompts_test.go`).
- `ai/prompts/` stays **flat**: `template.ParseFS` names templates by base filename, and a nested file would ship unparsed (enforced by `TestEveryPromptFileOnDiskIsParsed`).
- Every new renderable template needs an entry in `promptTestData` in `prompts_test.go`, or `TestEveryTemplateRenders` fails.
- The UAT step **never** returns an error, never parks an issue, never applies `ai-failed`, and never calls `c.RecordSession` (that would clobber the resumable primary session).
- `models.uat` has **no** fallback to `architect` — it is used exactly as written (decision 5 in the spec).
- Exact literals: marker `<!-- loope:uat -->`, sentinels `UAT_BEGIN` / `UAT_END`, checklist cap `8000` characters, resulting-body cap `60000` characters, recommended config block `"uat": {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 2, "maxTurns": 30}`.
- Log lines follow the existing convention in `loop.go`: `log.Printf("issue #%d: ...", n, ...)`.
- Verification for every task: `gofmt -l .` prints nothing, `go vet ./...` is clean, `go test ./...` passes.

## Assumptions (spec gaps resolved here)

1. **`run`'s `label` parameter.** The spec's signature is `run(ctx, c, cfg, wtPath, label, prompt string)` and both callers pass `"uat"`. Kept as written, with a `uatLabel = "uat"` const so the two call sites cannot drift.
2. **Marker injection.** The spec only requires `UATBeginSentinel`/`UATEndSentinel` in `promptData()`. `UATMarker` is added too, so `comments.md.tmpl` does not hardcode the marker string that `run`'s idempotency check greps for — the same drift argument the sentinel rule is built on. The marker is therefore also added to the `sentinels` list in `TestNoSentinelIsHardcodedInATemplate`.
3. **Truncation and UTF-8.** Truncating at a byte offset can split a rune, and an invalid-UTF-8 issue body would be rejected by the API. The truncation is followed by `strings.ToValidUTF8(..., "")`.
4. **Empty existing body.** `AppendIssueBody` on an issue with an empty body writes just the section, with no leading blank lines.
5. **Parameter order.** `RunBugPipeline` gains `base` before the trailing `*UAT`, since the spec calls `*UAT` the trailing parameter.
6. **Existing loop-level tests.** `newFakeEnv` in `loop_test.go` answers every non-triage `claude` call with `"Fixed and committed."`, which carries no `UAT_BEGIN`, so the new step self-skips at the parse stage and those tests stay green. Task 8 verifies this against the real suite rather than assuming it.

---

## File Structure

**Created:**
- `uat.go` — the whole UAT step: constants, `UATTarget`, `UAT`, `RunFeature`/`RunBug`/`run`, `parseUAT`, the two prompt builders, and `uatSection`. One responsibility (publish a checklist), one file, mirroring how `triage.go` / `rework.go` / `done.go` each own one step.
- `uat_test.go` — `parseUAT` unit tests, the `fakeUATTarget`, and the `run`-sequence tests.
- `ai/prompts/uat-feature.md.tmpl` — feature-route session prompt.
- `ai/prompts/uat-bug.md.tmpl` — bug-route session prompt.

**Modified:**
- `config.go` — `Models` gains `UAT ModelConfig`.
- `github.go` — `IssueBody`, `AppendIssueBody`.
- `prompts.go` — `promptData()` gains the two sentinels and the marker.
- `ai/prompts/comments.md.tmpl` — new `uat-section` define block.
- `pipeline_feature.go` — `RunFeaturePipeline` signature + call the step after the spec resolves.
- `pipeline_bug.go` — `RunBugPipeline` signature (`base`, `*UAT`) + call the step after the gates pass.
- `loop.go` — `handleIssue` constructs `&UAT{Target: o.gh, Num: n}` and passes it.
- `config_test.go`, `github_test.go`, `prompts_test.go`, `prompts_golden_test.go`, `pipeline_feature_test.go`, `pipeline_bug_test.go` — new tests and call-site updates.
- `loope.json.example`, `docs/configuration.md`, `docs/how-it-works.md` — docs.

---

### Task 1: `models.uat` config block

The UAT session's model config. Deliberately **not** an `executeConfig()`-style inheriting helper: an absent block means the `claude` CLI's own defaults, with no budget or turn cap.

**Files:**
- Modify: `config.go:28-38` (the `Models` struct)
- Modify: `loope.json.example:16-21` (the `models` object)
- Modify: `docs/configuration.md:84-99` (the `models` section)
- Test: `config_test.go` (append at end of file)

**Interfaces:**
- Consumes: nothing.
- Produces: `Models.UAT ModelConfig` — read as `cfg.Models.UAT` by Task 5.

- [ ] **Step 1: Write the failing tests**

Append to `config_test.go`:

```go
// The uat block is used exactly as written: unlike execute, it must NOT inherit
// anything from architect. An absent block means the claude CLI's own defaults.
func TestLoadConfigUATBlockRoundTrips(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone",
		"repoSlug": "org/repo",
		"workDir": "/tmp/work",
		"models": {
			"architect": {"model": "opus", "effort": "high", "maxBudgetUSD": 15, "maxTurns": 100},
			"uat": {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 2, "maxTurns": 30}
		}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := ModelConfig{Model: "sonnet", Effort: "medium", MaxBudgetUSD: 2, MaxTurns: 30}
	if cfg.Models.UAT != want {
		t.Errorf("Models.UAT = %+v, want %+v", cfg.Models.UAT, want)
	}
}

func TestLoadConfigUATDoesNotInheritFromArchitect(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone",
		"repoSlug": "org/repo",
		"workDir": "/tmp/work",
		"models": {"architect": {"model": "opus", "effort": "high", "maxBudgetUSD": 15, "maxTurns": 100}}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.UAT != (ModelConfig{}) {
		t.Errorf("Models.UAT = %+v, want the zero value — uat must not inherit from architect", cfg.Models.UAT)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestLoadConfigUAT' ./...`
Expected: FAIL — compile error, `cfg.Models.UAT undefined (type Models has no field or method UAT)`.

- [ ] **Step 3: Add the field**

In `config.go`, inside `type Models struct`, after the `Execute` field:

```go
	// UAT is the config for the UAT-checklist session. Unlike Execute it has no
	// fallback helper: the block is used exactly as written, so an absent block
	// means the claude CLI's own defaults with no budget or turn cap. The session
	// is short and read-only, so a cheap model with a low cap is the right shape.
	UAT ModelConfig `json:"uat"`
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestLoadConfigUAT' ./...`
Expected: PASS (both tests).

- [ ] **Step 5: Document the block**

In `loope.json.example`, change the `models` object so it reads exactly:

```json
  "models": {
    "architect": {"model": "opus", "effort": "high", "maxBudgetUSD": 15, "maxTurns": 100},
    "answerer":  {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 2, "maxTurns": 5},
    "triage":    {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 1, "maxTurns": 5},
    "execute":   {"model": "opus", "effort": "high", "maxBudgetUSD": 40, "maxTurns": 400},
    "uat":       {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 2, "maxTurns": 30}
  }
```

In `docs/configuration.md`, replace the line `Four roles, each {model, effort, maxBudgetUSD, maxTurns}:` with `Five roles, each {model, effort, maxBudgetUSD, maxTurns}:` and add this bullet after the `execute` bullet:

```markdown
- `uat` — optional. The short read-only session that writes the UAT checklist
  appended to the issue body. Unlike `execute`, it does **not** inherit from
  `architect`: the block is used exactly as written, so leaving it out means the
  `claude` CLI's own defaults with no budget or turn cap. A cheap model with a
  low cap is the right shape here (`{"model": "sonnet", "effort": "medium",
  "maxBudgetUSD": 2, "maxTurns": 30}`). The step never blocks the pipeline — if
  the session fails or hits its cap, the checklist is simply skipped and the
  reason is logged.
```

- [ ] **Step 6: Verify and commit**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: no gofmt output, no vet output, all tests pass.

```bash
git add config.go config_test.go loope.json.example docs/configuration.md
git commit -m "feat: add models.uat config block for the UAT session"
```

---

### Task 2: `GitHub.IssueBody` and `GitHub.AppendIssueBody`

The two GitHub surfaces the UAT step reads from and publishes to. Both go through `g.gh`, so they inherit the retry policy.

**Files:**
- Modify: `github.go` (add after `IssueTitle`, around line 180)
- Test: `github_test.go` (append at end of file)

**Interfaces:**
- Consumes: `g.gh(ctx, args...)` (existing, `github.go:36`), `g.slug`.
- Produces:
  - `func (g *GitHub) IssueBody(ctx context.Context, n int) (string, error)`
  - `func (g *GitHub) AppendIssueBody(ctx context.Context, n int, text string) error`

  Together these satisfy the `UATTarget` interface defined in Task 5, so `*GitHub` is passed directly as the target.

- [ ] **Step 1: Write the failing tests**

Append to `github_test.go`:

```go
func TestIssueBodyReturnsBody(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `{"body": "the original body"}`}}}
	g := testGitHub(f)
	g.retry = testRetry
	body, err := g.IssueBody(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if body != "the original body" {
		t.Errorf("body = %q", body)
	}
	joined := strings.Join(f.calls[0].args, " ")
	if !strings.Contains(joined, "issue view 7") || !strings.Contains(joined, "--repo org/repo") ||
		!strings.Contains(joined, "--json body") {
		t.Errorf("gh args = %q", joined)
	}
}

func TestIssueBodyPropagatesParseError(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `not json`}}}
	g := testGitHub(f)
	g.retry = testRetry
	if _, err := g.IssueBody(context.Background(), 7); err == nil {
		t.Error("want a parse error, got nil")
	}
}

// AppendIssueBody is a read-modify-write: it re-reads the body, joins the new
// text after a blank line, and edits the issue with the whole result.
func TestAppendIssueBodyReadModifyWrite(t *testing.T) {
	f := &fakeRunner{queue: []rresp{
		{stdout: `{"body": "original"}`},
		{stdout: ""},
	}}
	g := testGitHub(f)
	g.retry = testRetry
	if err := g.AppendIssueBody(context.Background(), 7, "APPENDED"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want a read then a write", len(f.calls))
	}
	edit := f.calls[1]
	joined := strings.Join(edit.args, " ")
	if !strings.Contains(joined, "issue edit 7") || !strings.Contains(joined, "--repo org/repo") {
		t.Errorf("edit args = %q", joined)
	}
	if got := argAfter(edit.args, "--body"); got != "original\n\nAPPENDED" {
		t.Errorf("--body = %q, want the original plus a blank line plus the new text", got)
	}
}

// An issue with an empty body must not gain leading blank lines.
func TestAppendIssueBodyEmptyOriginal(t *testing.T) {
	f := &fakeRunner{queue: []rresp{
		{stdout: `{"body": ""}`},
		{stdout: ""},
	}}
	g := testGitHub(f)
	g.retry = testRetry
	if err := g.AppendIssueBody(context.Background(), 7, "APPENDED"); err != nil {
		t.Fatal(err)
	}
	if got := argAfter(f.calls[1].args, "--body"); got != "APPENDED" {
		t.Errorf("--body = %q, want no leading blank lines", got)
	}
}

// A failed read must not be followed by a write: half-appending is worse than
// not appending.
func TestAppendIssueBodyFailedReadDoesNotWrite(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `not json`}}}
	g := testGitHub(f)
	g.retry = testRetry
	if err := g.AppendIssueBody(context.Background(), 7, "APPENDED"); err == nil {
		t.Fatal("want the read error propagated")
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.args, " "), "issue edit") {
			t.Error("a failed read must not be followed by an edit")
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestIssueBody|TestAppendIssueBody' ./...`
Expected: FAIL — `g.IssueBody undefined`, `g.AppendIssueBody undefined`.

- [ ] **Step 3: Implement both methods**

In `github.go`, after `IssueTitle`:

```go
// IssueBody returns just the issue's body, used by the UAT step to detect an
// already-published checklist and to build the appended body.
func (g *GitHub) IssueBody(ctx context.Context, n int) (string, error) {
	out, err := g.gh(ctx, "issue", "view", strconv.Itoa(n), "--repo", g.slug, "--json", "body")
	if err != nil {
		return "", err
	}
	var v struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", fmt.Errorf("parse issue body: %w", err)
	}
	return v.Body, nil
}

// AppendIssueBody appends text to the issue body via a read-modify-write
// `gh issue edit --body`. The read-modify-write is not atomic; the loop is the
// only writer of the body, and the UAT marker check makes a lost update at worst
// a missing checklist, never a duplicated one.
func (g *GitHub) AppendIssueBody(ctx context.Context, n int, text string) error {
	body, err := g.IssueBody(ctx, n)
	if err != nil {
		return err
	}
	updated := text
	if trimmed := strings.TrimRight(body, "\n"); trimmed != "" {
		updated = trimmed + "\n\n" + text
	}
	_, err = g.gh(ctx, "issue", "edit", strconv.Itoa(n), "--repo", g.slug, "--body", updated)
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run 'TestIssueBody|TestAppendIssueBody' ./...`
Expected: PASS (all five tests).

- [ ] **Step 5: Verify and commit**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

```bash
git add github.go github_test.go
git commit -m "feat: add GitHub.IssueBody and AppendIssueBody"
```

---

### Task 3: `uat.go` scaffolding — constants, `parseUAT`, rendered section

The pure, dependency-free half of the step: the literals, the extractor, and the outbound markdown. No Claude call and no GitHub call yet.

**Files:**
- Create: `uat.go`
- Create: `uat_test.go`
- Modify: `prompts.go:54-62` (`promptData`)
- Modify: `ai/prompts/comments.md.tmpl` (append a define block)
- Modify: `prompts_test.go:11-31` (`promptTestData`) and `prompts_test.go:105` (the `sentinels` slice)
- Modify: `prompts_golden_test.go` (append a golden test)

**Interfaces:**
- Consumes: `promptData()` and `mustRender(name string, data map[string]any) string` from `prompts.go`.
- Produces:
  - `uatMarker`, `uatBeginSentinel`, `uatEndSentinel`, `uatLabel`, `maxUATChars`, `maxIssueBodyChars` constants
  - `func parseUAT(s string) (string, bool)`
  - `func uatSection(checklist string) string`

  Task 5 calls all of these.

- [ ] **Step 1: Write the failing tests**

Create `uat_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestParseUATBeginAndEnd(t *testing.T) {
	got, ok := parseUAT("Here you go:\nUAT_BEGIN\n- [ ] click it\nUAT_END\nHope that helps!")
	if !ok {
		t.Fatal("want ok=true")
	}
	if got != "- [ ] click it" {
		t.Errorf("got %q, want the checklist with the surrounding prose stripped", got)
	}
}

// A session that opened the checklist but forgot the closing sentinel still
// publishes: take everything to the end of the result.
func TestParseUATBeginWithoutEnd(t *testing.T) {
	got, ok := parseUAT("UAT_BEGIN\n- [ ] one\n- [ ] two\n")
	if !ok {
		t.Fatal("want ok=true")
	}
	if got != "- [ ] one\n- [ ] two" {
		t.Errorf("got %q, want both items", got)
	}
}

func TestParseUATNoBegin(t *testing.T) {
	if got, ok := parseUAT("I could not find anything to verify."); ok {
		t.Errorf("want ok=false with no begin sentinel, got %q", got)
	}
}

// The bug route self-skips by emitting nothing at all, so an empty result must
// read as "nothing to publish".
func TestParseUATEmptyResult(t *testing.T) {
	if _, ok := parseUAT(""); ok {
		t.Error("want ok=false for an empty result")
	}
}

// Sentinels present but nothing between them is also "nothing to publish".
func TestParseUATEmptyContent(t *testing.T) {
	if got, ok := parseUAT("UAT_BEGIN\n\n   \nUAT_END"); ok {
		t.Errorf("want ok=false for an empty body between the sentinels, got %q", got)
	}
}

func TestUATSectionCarriesMarkerAndHeading(t *testing.T) {
	got := uatSection("- [ ] click it")
	if !strings.HasPrefix(got, uatMarker) {
		t.Errorf("section must lead with the idempotency marker:\n%s", got)
	}
	if !strings.Contains(got, "UAT checklist") || !strings.Contains(got, "- [ ] click it") {
		t.Errorf("section = %q", got)
	}
}
```

Append to `prompts_golden_test.go`:

```go
func TestGoldenUATSection(t *testing.T) {
	check(t, "uatSection", uatSection("- [ ] Run the thing and see the thing."),
		"<!-- loope:uat -->\n\n## 🤖 UAT checklist\n\n- [ ] Run the thing and see the thing.")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestParseUAT|TestUATSection|TestGoldenUATSection' ./...`
Expected: FAIL — compile error, `undefined: parseUAT`, `undefined: uatSection`, `undefined: uatMarker`.

- [ ] **Step 3: Create `uat.go` with the constants, `parseUAT`, and `uatSection`**

Create `uat.go`:

```go
package main

import (
	"strings"
)

const (
	// uatMarker is the idempotency marker. It is an HTML comment, so it is
	// invisible in rendered markdown while still being greppable in the raw body.
	uatMarker = "<!-- loope:uat -->"
	// uatBeginSentinel / uatEndSentinel fence the checklist inside the session's
	// result text. Injected into the prompts via promptData(), never hardcoded in
	// a template.
	uatBeginSentinel = "UAT_BEGIN"
	uatEndSentinel   = "UAT_END"
	// uatLabel is the Claude call label, and so the <seq>-uat.* log file prefix.
	uatLabel = "uat"
	// maxUATChars caps the checklist itself. maxIssueBodyChars keeps the resulting
	// body clear of GitHub's 65536-character issue body limit: past it, the step
	// skips rather than risk a rejected edit.
	maxUATChars       = 8000
	maxIssueBodyChars = 60000
)

// parseUAT extracts the checklist from a UAT session's result: the text after
// uatBeginSentinel, up to uatEndSentinel if present or to the end of the result
// if the session omitted it. ok is false when the begin sentinel is absent or
// the content between the sentinels is blank — both mean "nothing to publish",
// which is also how the bug route self-skips a branch with no commits.
func parseUAT(s string) (string, bool) {
	i := strings.Index(s, uatBeginSentinel)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(uatBeginSentinel):]
	if j := strings.Index(rest, uatEndSentinel); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

// uatSection renders the outbound issue-body section: the marker, the heading,
// and the checklist. It lives with the other human-facing outbound text in
// ai/prompts/comments.md.tmpl.
func uatSection(checklist string) string {
	d := promptData()
	d["Checklist"] = checklist
	return mustRender("uat-section", d)
}
```

- [ ] **Step 4: Add the sentinels and the marker to `promptData()`**

In `prompts.go`, inside the returned map in `promptData()`, after `"DoneConfirmSentinel"`:

```go
		"UATBeginSentinel":    uatBeginSentinel,
		"UATEndSentinel":      uatEndSentinel,
		"UATMarker":           uatMarker,
```

- [ ] **Step 5: Add the `uat-section` template**

Append to `ai/prompts/comments.md.tmpl` (keeping the file's one-blank-line-between-blocks style):

```
{{define "uat-section"}}{{.UATMarker}}

## 🤖 UAT checklist

{{.Checklist}}{{end}}
```

- [ ] **Step 6: Register the template with the prompt-hygiene tests**

In `prompts_test.go`, add to `promptTestData` (after the `"guidance-network"` entry):

```go
	"uat-section":          {"Checklist": "- [ ] C"},
```

and extend the `sentinels` slice in `TestNoSentinelIsHardcodedInATemplate`:

```go
	sentinels := []string{confidenceSentinel, specReadySentinel, readySentinel, alreadyDoneSentinel, doneConfirmSentinel,
		uatBeginSentinel, uatEndSentinel, uatMarker}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test -run 'TestParseUAT|TestUATSection|TestGoldenUATSection|TestEveryTemplateRenders|TestNoSentinelIsHardcoded|TestEveryPromptFileOnDisk' ./...`
Expected: PASS.

- [ ] **Step 8: Verify and commit**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

```bash
git add uat.go uat_test.go prompts.go prompts_test.go prompts_golden_test.go ai/prompts/comments.md.tmpl
git commit -m "feat: add UAT sentinels, parseUAT, and the issue-body section template"
```

---

### Task 4: The two UAT session prompts

**Files:**
- Create: `ai/prompts/uat-feature.md.tmpl`
- Create: `ai/prompts/uat-bug.md.tmpl`
- Modify: `uat.go` (add the two builders)
- Modify: `prompts_test.go` (`promptTestData`)
- Test: `prompts_golden_test.go` (append two golden tests)

**Interfaces:**
- Consumes: `promptData()`, `mustRender` (`prompts.go`); `UATBeginSentinel` / `UATEndSentinel` from Task 3.
- Produces:
  - `func uatFeaturePrompt(specPath string) string`
  - `func uatBugPrompt(issue, base string) string`

  Task 5 calls both.

- [ ] **Step 1: Write the failing golden tests**

Append to `prompts_golden_test.go`:

```go
func TestGoldenUATFeaturePrompt(t *testing.T) {
	want := `Read the approved spec at docs/spec.md and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Output ONLY the checklist, between a line reading UAT_BEGIN and a line reading
UAT_END. Print nothing before or after those two lines.

Rules for the checklist:
- Markdown ` + "`- [ ]`" + ` checkboxes, grouped under short ` + "`###`" + ` headings when there is
  more than one area to verify.
- Each item is one concrete action a human performs plus the one observable
  result they should see.
- No implementation detail, no file paths, no code.
- Short: aim for under 20 items. Cover every behavior the spec describes,
  including the error and edge cases it specifies, but do not invent scope
  beyond it.
- Do not modify, create, or commit any file.`
	check(t, "uatFeaturePrompt", uatFeaturePrompt("docs/spec.md"), want)
}

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
- Markdown ` + "`- [ ]`" + ` checkboxes, grouped under short ` + "`###`" + ` headings when there is
  more than one area to verify.
- Each item is one concrete action a human performs plus the one observable
  result they should see.
- No implementation detail, no file paths, no code.
- Short: aim for under 20 items. Cover the reported bug and every behavior the
  fix touches, including its error and edge cases, but do not invent scope
  beyond them.
- Do not modify, create, or commit any file.`
	check(t, "uatBugPrompt", uatBugPrompt("ISSUE BODY", "main"), want)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestGoldenUAT(Feature|Bug)Prompt' ./...`
Expected: FAIL — compile error, `undefined: uatFeaturePrompt`, `undefined: uatBugPrompt`.

- [ ] **Step 3: Write the two template files**

Create `ai/prompts/uat-feature.md.tmpl` with exactly this content (one trailing newline, which `mustRender` trims):

```
Read the approved spec at {{.SpecPath}} and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Output ONLY the checklist, between a line reading {{.UATBeginSentinel}} and a line reading
{{.UATEndSentinel}}. Print nothing before or after those two lines.

Rules for the checklist:
- Markdown `- [ ]` checkboxes, grouped under short `###` headings when there is
  more than one area to verify.
- Each item is one concrete action a human performs plus the one observable
  result they should see.
- No implementation detail, no file paths, no code.
- Short: aim for under 20 items. Cover every behavior the spec describes,
  including the error and edge cases it specifies, but do not invent scope
  beyond it.
- Do not modify, create, or commit any file.
```

Create `ai/prompts/uat-bug.md.tmpl` with exactly this content:

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

Rules for the checklist:
- Markdown `- [ ]` checkboxes, grouped under short `###` headings when there is
  more than one area to verify.
- Each item is one concrete action a human performs plus the one observable
  result they should see.
- No implementation detail, no file paths, no code.
- Short: aim for under 20 items. Cover the reported bug and every behavior the
  fix touches, including its error and edge cases, but do not invent scope
  beyond them.
- Do not modify, create, or commit any file.
```

- [ ] **Step 4: Add the two builders**

In `uat.go`, after `uatSection`:

```go
// uatFeaturePrompt drives the feature route's UAT session from the committed
// spec: the checklist describes the behavior the spec promises, and is published
// before any code exists.
func uatFeaturePrompt(specPath string) string {
	d := promptData()
	d["SpecPath"] = specPath
	return mustRender("uat-feature.md.tmpl", d)
}

// uatBugPrompt drives the bug route's UAT session from the issue plus the diff
// the fix actually produced. An empty diff means the session prints nothing,
// which parseUAT reads as "nothing to publish" — that is how the step self-skips
// a branch with no commits, with no commit-count plumbing in the pipeline.
func uatBugPrompt(issue, base string) string {
	d := promptData()
	d["Issue"] = issue
	d["Base"] = base
	return mustRender("uat-bug.md.tmpl", d)
}
```

- [ ] **Step 5: Register both templates in `promptTestData`**

In `prompts_test.go`, add to `promptTestData`:

```go
	"uat-feature.md.tmpl":  {"SpecPath": "docs/spec.md"},
	"uat-bug.md.tmpl":      {"Issue": "I", "Base": "main"},
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -run 'TestGoldenUAT|TestEveryTemplateRenders|TestEveryPromptFileOnDiskIsParsed|TestNoSentinelIsHardcodedInATemplate' ./...`
Expected: PASS. If a golden test fails on whitespace, diff the reported got/want and fix the **template file** to match the golden string in Step 1 — the golden string is the contract.

- [ ] **Step 7: Verify and commit**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

```bash
git add ai/prompts/uat-feature.md.tmpl ai/prompts/uat-bug.md.tmpl uat.go prompts_test.go prompts_golden_test.go
git commit -m "feat: add uat-feature and uat-bug session prompts"
```

---

### Task 5: The `UAT` step itself

The `run` sequence: idempotency check → Claude session → extract → size guard → append. Every failure path logs and returns.

**Files:**
- Modify: `uat.go` (add `UATTarget`, `UAT`, `RunFeature`, `RunBug`, `run`)
- Test: `uat_test.go` (append)

**Interfaces:**
- Consumes: `parseUAT`, `uatSection`, `uatFeaturePrompt`, `uatBugPrompt`, the constants (Tasks 3–4); `Claude.Call(ctx, ClaudeCall) (*ClaudeResult, error)` (`claude.go:82`); `cfg.Models.UAT` (Task 1); `*GitHub`'s `IssueBody`/`AppendIssueBody` (Task 2).
- Produces:
  - `type UATTarget interface { IssueBody(ctx context.Context, n int) (string, error); AppendIssueBody(ctx context.Context, n int, text string) error }`
  - `type UAT struct { Target UATTarget; Num int }`
  - `func (u *UAT) RunFeature(ctx context.Context, c *Claude, cfg *Config, wtPath, specPath string)`
  - `func (u *UAT) RunBug(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string)`

  Both return nothing and are nil-receiver safe. Tasks 6–7 call them.

- [ ] **Step 1: Write the failing tests**

Append to `uat_test.go` (and add `"context"`, `"fmt"`, `"os"`, `"path/filepath"` to its imports):

```go
// fakeUATTarget stands in for *GitHub: it records what was appended and can be
// scripted to fail either operation.
type fakeUATTarget struct {
	body      string
	bodyErr   error
	appendErr error
	bodyCalls int
	appended  []string
}

func (f *fakeUATTarget) IssueBody(ctx context.Context, n int) (string, error) {
	f.bodyCalls++
	return f.body, f.bodyErr
}

func (f *fakeUATTarget) AppendIssueBody(ctx context.Context, n int, text string) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.appended = append(f.appended, text)
	f.body += "\n\n" + text
	return nil
}

func uatTestConfig() *Config {
	return &Config{Models: Models{UAT: ModelConfig{Model: "sonnet", Effort: "medium", MaxTurns: 30}}}
}

// uatResult builds a fake claude payload whose result carries a fenced checklist.
func uatResult(checklist string) string {
	return claudeJSON("Sure thing.\n"+uatBeginSentinel+"\n"+checklist+"\n"+uatEndSentinel, "uat-1")
}

func TestUATPublishesChecklist(t *testing.T) {
	tgt := &fakeUATTarget{body: "the original body"}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}
	c := &Claude{runner: f}
	u := &UAT{Target: tgt, Num: 7}
	u.RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")

	if len(tgt.appended) != 1 {
		t.Fatalf("appended %d sections, want 1", len(tgt.appended))
	}
	if !strings.Contains(tgt.appended[0], uatMarker) || !strings.Contains(tgt.appended[0], "- [ ] click it") {
		t.Errorf("appended = %q", tgt.appended[0])
	}
	if len(f.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1", len(f.calls))
	}
	call := f.calls[0]
	if call.dir != "/wt" || argAfter(call.args, "--model") != "sonnet" {
		t.Errorf("call = %+v", call)
	}
	// Read-only: the session inspects and reports, it must not edit or commit.
	tools := argAfter(call.args, "--disallowedTools")
	for _, want := range []string{"AskUserQuestion", "Write", "Edit", "NotebookEdit"} {
		if !strings.Contains(tools, want) {
			t.Errorf("--disallowedTools = %q, must include %s", tools, want)
		}
	}
	if !strings.Contains(call.stdin, "docs/spec.md") {
		t.Errorf("prompt should carry the spec path: %s", call.stdin)
	}
}

// Idempotency: a body that already carries the marker costs nothing — no session,
// no append.
func TestUATSkipsWhenMarkerPresent(t *testing.T) {
	tgt := &fakeUATTarget{body: "body\n\n" + uatMarker + "\n\n## 🤖 UAT checklist\n\n- [ ] old"}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] new")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	if len(f.calls) != 0 {
		t.Errorf("claude calls = %d, want 0 — the marker check must run before the session", len(f.calls))
	}
	if len(tgt.appended) != 0 {
		t.Errorf("appended %d sections, want 0", len(tgt.appended))
	}
}

// A failed body fetch also skips: a duplicated UAT section is worse than a
// missing one, and the next run gets another chance.
func TestUATSkipsWhenBodyFetchFails(t *testing.T) {
	tgt := &fakeUATTarget{bodyErr: fmt.Errorf("gh: 503")}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] new")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	if len(f.calls) != 0 {
		t.Errorf("claude calls = %d, want 0", len(f.calls))
	}
	if len(tgt.appended) != 0 {
		t.Errorf("appended %d sections, want 0", len(tgt.appended))
	}
}

func TestUATSkipsWhenSessionErrors(t *testing.T) {
	tgt := &fakeUATTarget{body: "body"}
	f := &fakeRunner{queue: []rresp{{err: fmt.Errorf("exit 1")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	if len(tgt.appended) != 0 {
		t.Errorf("a failed session must publish nothing, got %q", tgt.appended)
	}
}

// The bug route self-skips a branch with no commits: the session prints nothing,
// so there is no begin sentinel to parse.
func TestUATSkipsWhenResultHasNoSentinel(t *testing.T) {
	tgt := &fakeUATTarget{body: "body"}
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("", "uat-1")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	if len(tgt.appended) != 0 {
		t.Errorf("appended %d sections, want 0", len(tgt.appended))
	}
}

// The UAT session is ephemeral: it must never overwrite the resumable primary
// session recorded by the debug/architect/execute call.
func TestUATDoesNotRecordSession(t *testing.T) {
	logDir := t.TempDir()
	c := &Claude{runner: &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}, logDir: logDir}
	c.RecordSession("primary-sess", "bug")
	(&UAT{Target: &fakeUATTarget{body: "body"}, Num: 7}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	si, err := readSession(logDir)
	if err != nil {
		t.Fatal(err)
	}
	if si.SessionID != "primary-sess" || si.Kind != "bug" {
		t.Errorf("session = %+v, want the primary session untouched", si)
	}
}

// The result text is persisted by Claude.Call as <seq>-uat.output.md — that is
// the "logged as a file" half of the requirement, with no extra plumbing.
func TestUATLogsResultAsOutputFile(t *testing.T) {
	logDir := t.TempDir()
	c := &Claude{runner: &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}, logDir: logDir}
	(&UAT{Target: &fakeUATTarget{body: "body"}, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	matches, err := filepath.Glob(filepath.Join(logDir, "*-uat.output.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("uat output files = %v, want exactly one", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- [ ] click it") {
		t.Errorf("output file = %q", data)
	}
}

func TestUATTruncatesOversizedChecklist(t *testing.T) {
	tgt := &fakeUATTarget{body: "body"}
	huge := strings.Repeat("x", maxUATChars+500)
	f := &fakeRunner{queue: []rresp{{stdout: uatResult(huge)}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	if len(tgt.appended) != 1 {
		t.Fatalf("appended %d sections, want 1 (oversized is truncated, not skipped)", len(tgt.appended))
	}
	if n := strings.Count(tgt.appended[0], "x"); n != maxUATChars {
		t.Errorf("checklist kept %d chars, want it truncated to %d", n, maxUATChars)
	}
}

func TestUATSkipsWhenResultingBodyTooLarge(t *testing.T) {
	tgt := &fakeUATTarget{body: strings.Repeat("y", maxIssueBodyChars)}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	if len(tgt.appended) != 0 {
		t.Errorf("a body that would exceed the cap must be skipped, not appended: %q", tgt.appended)
	}
}

func TestUATSurvivesAppendFailure(t *testing.T) {
	tgt := &fakeUATTarget{body: "body", appendErr: fmt.Errorf("gh: 422")}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}
	c := &Claude{runner: f}
	// No panic, no error to propagate: the pipeline continues.
	(&UAT{Target: tgt, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
}

// A nil *UAT (and a UAT with no target) disables the step entirely, so callers
// never need a nil guard.
func TestUATNilReceiverIsSafe(t *testing.T) {
	var u *UAT
	f := &fakeRunner{}
	c := &Claude{runner: f}
	u.RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	u.RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	(&UAT{}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	(&UAT{}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	if len(f.calls) != 0 {
		t.Errorf("a disabled UAT must make no calls, got %d", len(f.calls))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run TestUAT ./...`
Expected: FAIL — compile error, `undefined: UAT`.

- [ ] **Step 3: Implement the step**

In `uat.go`, add `"context"` and `"log"` to the imports, and add after the constants block:

```go
// UATTarget is the GitHub surface the UAT step reads from and publishes to.
// *GitHub satisfies it; tests substitute a fake.
type UATTarget interface {
	IssueBody(ctx context.Context, n int) (string, error)
	AppendIssueBody(ctx context.Context, n int, text string) error
}

// UAT publishes a human-verifiable acceptance checklist onto the issue body.
// A nil *UAT (or one with no Target) disables the step entirely, so callers
// never need a nil guard.
type UAT struct {
	Target UATTarget
	Num    int
}

// RunFeature publishes the checklist for the feature route, from the committed
// spec. It returns nothing: every failure path logs and continues, because a
// missing checklist must never cost a shipped feature.
func (u *UAT) RunFeature(ctx context.Context, c *Claude, cfg *Config, wtPath, specPath string) {
	if u == nil || u.Target == nil {
		return
	}
	u.run(ctx, c, cfg, wtPath, uatLabel, uatFeaturePrompt(specPath))
}

// RunBug publishes the checklist for the bug route, from the issue content plus
// the diff the fix produced. Same non-blocking contract as RunFeature.
func (u *UAT) RunBug(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string) {
	if u == nil || u.Target == nil {
		return
	}
	u.run(ctx, c, cfg, wtPath, uatLabel, uatBugPrompt(issueContent, base))
}

// run is the whole sequence, shared by both routes: idempotency check, session,
// extract, size guard, append. Every early return logs the issue number and the
// reason, so a missing checklist is diagnosable from the daemon log alone.
func (u *UAT) run(ctx context.Context, c *Claude, cfg *Config, wtPath, label, prompt string) {
	if u == nil || u.Target == nil {
		return
	}
	// Check before spending a session. A failed fetch skips too: publishing a
	// second UAT section is worse than publishing none, and the next run on this
	// issue gets another chance.
	body, err := u.Target.IssueBody(ctx, u.Num)
	if err != nil {
		log.Printf("issue #%d: UAT skipped, issue body fetch failed: %v", u.Num, err)
		return
	}
	if strings.Contains(body, uatMarker) {
		log.Printf("issue #%d: UAT already present, skipping", u.Num)
		return
	}

	// No RecordSession: the UAT session is ephemeral and must never overwrite the
	// resumable primary session that `loop -rework` resumes.
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: label, Prompt: prompt,
		Model:           cfg.Models.UAT,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion", "Write", "Edit", "NotebookEdit"},
	})
	if err != nil {
		log.Printf("issue #%d: UAT skipped, session failed: %v", u.Num, err)
		return
	}

	checklist, ok := parseUAT(res.Result)
	if !ok {
		log.Printf("issue #%d: UAT skipped, session produced no checklist", u.Num)
		return
	}
	if len(checklist) > maxUATChars {
		// ToValidUTF8 drops the partial rune a byte-offset cut can leave behind:
		// an invalid-UTF-8 body would be rejected by the API.
		checklist = strings.ToValidUTF8(checklist[:maxUATChars], "")
		log.Printf("issue #%d: UAT checklist truncated to %d chars", u.Num, maxUATChars)
	}

	section := uatSection(checklist)
	if len(body)+len(section) > maxIssueBodyChars {
		log.Printf("issue #%d: UAT skipped, the resulting issue body would exceed %d chars", u.Num, maxIssueBodyChars)
		return
	}
	if err := u.Target.AppendIssueBody(ctx, u.Num, section); err != nil {
		log.Printf("issue #%d: UAT append failed: %v", u.Num, err)
		return
	}
	log.Printf("issue #%d: UAT checklist published to the issue body", u.Num)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -run TestUAT ./...`
Expected: PASS (all tests).

- [ ] **Step 5: Verify and commit**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

```bash
git add uat.go uat_test.go
git commit -m "feat: add the non-blocking UAT step"
```

---

### Task 6: Wire the feature route

The UAT session runs immediately after the spec resolves, before plan and execute — the checklist describes what the spec promises, published before any code exists.

**Files:**
- Modify: `pipeline_feature.go:24` (signature) and `pipeline_feature.go:59-63` (the `parseSpecReady` branch)
- Modify: `loop.go:275-281` (`handleIssue`)
- Test: `pipeline_feature_test.go` (update the 11 existing call sites; append two new tests)

**Interfaces:**
- Consumes: `(*UAT).RunFeature(ctx, c, cfg, wtPath, specPath)` (Task 5).
- Produces: `func RunFeaturePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT) error` — one new trailing parameter. Task 7 leaves it alone.

- [ ] **Step 1: Write the failing tests**

Append to `pipeline_feature_test.go`:

```go
// Decision 2, taken literally: the UAT session runs immediately after the spec,
// before plan and execute.
func TestFeaturePipelineRunsUATBeforePlan(t *testing.T) {
	wt := t.TempDir()
	var labels []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		labels = append(labels, argAfter(c.args, "--model"))
		switch len(labels) {
		case 1: // architect: commits the spec straight away
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 2: // the UAT session
			if !strings.Contains(c.stdin, "2026-07-13-thing-design.md") {
				t.Errorf("the second call should be the UAT session on the spec, got: %s", c.stdin)
			}
			return claudeJSON(uatBeginSentinel+"\n- [ ] click it\n"+uatEndSentinel, "uat-1"), "", nil
		case 3: // plan
			if !strings.Contains(c.stdin, "/superpowers:writing-plans") {
				t.Errorf("the third call should be the plan session, got: %s", c.stdin)
			}
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 4: // execute
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d: %v", len(labels), c.args)
		return "", "", nil
	}
	tgt := &fakeUATTarget{body: "the issue body"}
	cfg := featureConfig()
	cfg.Models.UAT = ModelConfig{Model: "sonnet"}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, cfg, wt, "ISSUE", "PERSONA", &UAT{Target: tgt, Num: 7}); err != nil {
		t.Fatal(err)
	}
	if len(labels) != 4 {
		t.Fatalf("calls = %d, want architect, uat, plan, execute", len(labels))
	}
	if len(tgt.appended) != 1 {
		t.Errorf("appended %d sections, want 1", len(tgt.appended))
	}
}

// Non-blocking: a UAT session that errors must not stop plan and execute.
func TestFeaturePipelineContinuesWhenUATFails(t *testing.T) {
	wt := t.TempDir()
	var n int
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		n++
		switch {
		case n == 1:
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case n == 2:
			return "", "boom", fmt.Errorf("exit 1") // the UAT session fails
		case n == 3:
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		default:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
	}
	c := &Claude{runner: f}
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "PERSONA",
		&UAT{Target: &fakeUATTarget{body: "body"}, Num: 7}); err != nil {
		t.Fatalf("a failed UAT session must never block the pipeline: %v", err)
	}
	if n != 4 {
		t.Errorf("calls = %d, want the pipeline to have run plan and execute anyway", n)
	}
}
```

`pipeline_feature_test.go` already imports `strings` and `context`; add `"fmt"` to its import block for the test above.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestFeaturePipelineRunsUATBeforePlan|TestFeaturePipelineContinuesWhenUATFails' ./...`
Expected: FAIL — compile error, too many arguments to `RunFeaturePipeline`.

- [ ] **Step 3: Change the signature and call the step**

In `pipeline_feature.go`, change the signature (line 24) to:

```go
func RunFeaturePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT) error {
```

and extend its doc comment with one sentence before the closing line:

```go
// Immediately after the spec is committed — before plan and execute — the
// non-blocking UAT step publishes a human-verifiable checklist onto the issue.
```

Then, in the loop body, replace the `parseSpecReady` branch:

```go
		if rel, ok := parseSpecReady(output); ok {
			if specPath, ok := resolveSpec(wtPath, rel, start); ok {
				uat.RunFeature(ctx, c, cfg, wtPath, specPath)
				return runPlanThenExecute(ctx, c, cfg, wtPath, specPath, start)
			}
		}
```

- [ ] **Step 4: Update `handleIssue`**

In `loop.go`, in `handleIssue`, replace the pipeline dispatch:

```go
	c := &Claude{runner: o.runner, logDir: o.issueLogDir(n), configDir: o.cfg.ClaudeConfigDir}
	uat := &UAT{Target: o.gh, Num: n}
	var perr error
	if kind == "bug" {
		perr = RunBugPipeline(ctx, c, o.cfg, wtPath, content)
	} else {
		perr = RunFeaturePipeline(ctx, c, o.cfg, wtPath, content, readPersona(o.cfg.PersonaPath), uat)
	}
```

(The bug call gains its parameters in Task 7.)

- [ ] **Step 5: Update the existing `RunFeaturePipeline` call sites**

In `pipeline_feature_test.go`, append `, nil` to the argument list of every pre-existing `RunFeaturePipeline(...)` call — the 11 calls at (pre-edit) lines 135, 180, 225, 270, 292, 316, 356, 386, 429, 458, 478, 518. A `nil *UAT` disables the step, so those tests keep their exact call scripts and counts.

Run: `grep -n 'RunFeaturePipeline(' pipeline_feature_test.go` and confirm every call ends in `nil)` or passes an explicit `&UAT{...}`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -run TestFeaturePipeline ./...`
Expected: PASS, including the two new tests.

- [ ] **Step 7: Verify and commit**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: clean.

```bash
git add pipeline_feature.go pipeline_feature_test.go loop.go
git commit -m "feat: run the UAT step after the spec on the feature route"
```

---

### Task 7: Wire the bug route

The step runs only on the outcome where a fix was actually produced — after the confidence gate and the already-done gate. Neither `ai-needs-info` nor `already done` reaches it.

**Files:**
- Modify: `pipeline_bug.go:7-37`
- Modify: `loop.go` (the `RunBugPipeline` call from Task 6)
- Test: `pipeline_bug_test.go` (update the 12 existing call sites; append three new tests)

**Interfaces:**
- Consumes: `(*UAT).RunBug(ctx, c, cfg, wtPath, issueContent, base)` (Task 5).
- Produces: `func RunBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT) error` — `base` is the base branch (already available in `handleIssue`), so the prompt can name `origin/<base>` for the diff.

- [ ] **Step 1: Write the failing tests**

Append to `pipeline_bug_test.go`:

```go
func TestBugPipelineRunsUATAfterDebug(t *testing.T) {
	var prompts []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		if len(prompts) == 1 {
			return claudeJSON("Fixed and committed.", "debug-1"), "", nil
		}
		return claudeJSON(uatBeginSentinel+"\n- [ ] reproduce the old crash and see it gone\n"+uatEndSentinel, "uat-1"), "", nil
	}
	tgt := &fakeUATTarget{body: "the issue body"}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}, UAT: ModelConfig{Model: "sonnet"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", &UAT{Target: tgt, Num: 7}); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("calls = %d, want debug then uat", len(prompts))
	}
	// The UAT prompt carries the issue content and names the base for the diff.
	if !strings.Contains(prompts[1], "ISSUE") || !strings.Contains(prompts[1], "origin/main") {
		t.Errorf("uat prompt = %s", prompts[1])
	}
	if argAfter(f.calls[1].args, "--model") != "sonnet" {
		t.Errorf("the uat call must use models.uat, got %v", f.calls[1].args)
	}
	if len(tgt.appended) != 1 {
		t.Errorf("appended %d sections, want 1", len(tgt.appended))
	}
}

// A low-confidence outcome escalates to ai-needs-info with no fix, so there is
// nothing to accept: the UAT step must not run.
func TestBugPipelineSkipsUATOnLowConfidence(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("CONFIDENCE: 40\nNo repro steps.", "s1")}}}
	tgt := &fakeUATTarget{body: "body"}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", &UAT{Target: tgt, Num: 7})
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %d, want only the debug turn", len(f.calls))
	}
	if tgt.bodyCalls != 0 || len(tgt.appended) != 0 {
		t.Error("the UAT step must not run on the low-confidence outcome")
	}
}

// Already-done closes the issue without a fix — again nothing to accept.
func TestBugPipelineSkipsUATOnAlreadyDone(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("PIPELINE_ALREADY_DONE: the guard already exists", "s1")}}}
	tgt := &fakeUATTarget{body: "body"}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", &UAT{Target: tgt, Num: 7})
	var done *alreadyDoneError
	if !errors.As(err, &done) {
		t.Fatalf("want *alreadyDoneError, got %v", err)
	}
	if tgt.bodyCalls != 0 || len(tgt.appended) != 0 {
		t.Error("the UAT step must not run on the already-done outcome")
	}
}

// Non-blocking: a UAT session that errors still leaves the pipeline successful.
func TestBugPipelineReturnsNilWhenUATFails(t *testing.T) {
	var n int
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		n++
		if n == 1 {
			return claudeJSON("Fixed and committed.", "debug-1"), "", nil
		}
		return "", "boom", fmt.Errorf("exit 1")
	}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main",
		&UAT{Target: &fakeUATTarget{body: "body"}, Num: 7}); err != nil {
		t.Fatalf("a failed UAT session must never fail the pipeline: %v", err)
	}
}
```

`pipeline_bug_test.go` already imports `context`, `errors`, `fmt`, `strings`, `testing` — no import changes needed.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -run 'TestBugPipelineRunsUAT|TestBugPipelineSkipsUAT|TestBugPipelineReturnsNilWhenUATFails' ./...`
Expected: FAIL — compile error, too many arguments to `RunBugPipeline`.

- [ ] **Step 3: Change the signature and call the step**

Rewrite the head and tail of `RunBugPipeline` in `pipeline_bug.go`:

```go
// RunBugPipeline drives one systematic-debugging session, gated on confidence
// and the already-done claim. base is the base branch: on the outcome where a
// fix was actually produced, the non-blocking UAT step diffs against
// origin/<base> to build a human-verifiable checklist for the issue body.
func RunBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT) error {
```

and replace the final `return nil` (line 36) with:

```go
	// Only this outcome produced a fix — neither the needs-info nor the
	// already-done return above reaches here, so neither publishes a checklist.
	uat.RunBug(ctx, c, cfg, wtPath, issueContent, base)
	return nil
```

- [ ] **Step 4: Update `handleIssue`**

In `loop.go`, in `handleIssue`, change the bug branch to:

```go
		perr = RunBugPipeline(ctx, c, o.cfg, wtPath, content, base, uat)
```

- [ ] **Step 5: Update the existing `RunBugPipeline` call sites**

In `pipeline_bug_test.go`, change every pre-existing `RunBugPipeline(...)` call from `..., "/wt", "ISSUE")` to `..., "/wt", "ISSUE", "main", nil)` — the 12 calls at (pre-edit) lines 15, 37, 47, 64, 85, 101, 117, 137, 147, 158, 168, 180, preserving each call's own issue string. A `nil *UAT` disables the step, so those tests keep their exact call counts.

Run: `grep -n 'RunBugPipeline(' pipeline_bug_test.go` and confirm every call passes a base and a `*UAT`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -run TestBugPipeline ./...`
Expected: PASS, including the four new tests.

- [ ] **Step 7: Verify the whole suite, including the loop-level tests**

Run: `go test ./...`
Expected: PASS. `handleIssue` now runs the step for real against `newFakeEnv`'s fake `gh`/`claude`, whose generic non-triage response carries no `UAT_BEGIN`, so the step self-skips at the parse stage. If a loop-level test fails on an unexpected extra call, fix that test's handler (script the `uat` call explicitly) rather than weakening the step.

- [ ] **Step 8: Commit**

```bash
git add pipeline_bug.go pipeline_bug_test.go loop.go
git commit -m "feat: run the UAT step after a bug fix, gated on a real fix"
```

---

### Task 8: Document the step end to end and verify the whole feature

**Files:**
- Modify: `docs/how-it-works.md:7-20` (the triage bullets)
- Test: the full suite

**Interfaces:**
- Consumes: everything above. Produces nothing new.

- [ ] **Step 1: Document both routes**

In `docs/how-it-works.md`, in the `bug` bullet, replace the trailing clause `otherwise it reproduces with a failing test, fixes, and commits.` with:

```markdown
     otherwise it reproduces with a failing test, fixes, and commits. A short
     read-only session then reads the issue and the resulting diff and appends a
     UAT checklist to the issue body.
```

In the `feature` bullet, replace the sentence `A **fresh** session turns that spec into a committed implementation plan, and a third session executes the plan.` with:

```markdown
     spec. A short read-only session then turns that spec into a UAT checklist
     appended to the issue body. A **fresh** session turns the spec into a
     committed implementation plan, and a third session executes the plan.
```

Then append this section immediately before `## Concurrency and scheduling`:

```markdown
## UAT checklist

Both routes publish a hand-verification checklist onto the **issue body** (not a
comment), under a `## 🤖 UAT checklist` heading preceded by an invisible
`<!-- loope:uat -->` marker. The marker is the idempotency key: an issue that
already carries it is never given a second checklist, so re-runs, resumes and
reworks leave the first one in place.

The step is deliberately non-blocking. It runs in its own ephemeral session
(`models.uat`, no inheritance from `architect`) with `Write`, `Edit` and
`NotebookEdit` disabled, and it never records a resumable session. If anything
goes wrong — the body fetch fails, the session errors or hits its cap, the
result carries no checklist, the body would grow past GitHub's size limit — the
step logs the issue number and the reason and returns, and the pipeline carries
on to plan/execute or to shipping. The session's full output is kept as
`<seq>-uat.output.md` in the issue's log directory either way.

On the feature route it runs immediately after the spec is committed, before the
plan exists: the checklist describes the behavior the spec promises. On the bug
route it runs after the fix, from the issue plus `git diff origin/<base>...HEAD`,
and only on the outcome where a fix was actually produced — an `ai-needs-info`
escalation or an already-done close publishes nothing. The rework flow does not
run it: the feature route already published one, and the marker check would skip
it anyway.
```

- [ ] **Step 2: Verify the whole feature**

Run: `gofmt -l . && go vet ./... && go test ./...`
Expected: no gofmt output, no vet output, all tests pass.

Run: `go test -run 'UAT|uat' -v ./... 2>&1 | grep -c '^=== RUN'`
Expected: a non-zero count — confirms the new tests are actually being selected and run, not silently skipped by a typo'd name.

- [ ] **Step 3: Confirm the config example is valid JSON**

Run: `python3 -c "import json,sys; json.load(open('loope.json.example')); print('ok')"`
Expected: `ok`.

- [ ] **Step 4: Commit**

```bash
git add docs/how-it-works.md
git commit -m "docs: describe the UAT checklist step on both routes"
```

---

## Self-Review

**Spec coverage:**

| Spec requirement | Task |
|---|---|
| `uat.go` constants (`uatMarker`, begin/end sentinels) | 3 |
| `UATTarget` interface | 5 |
| `UAT` struct, nil-receiver safety, `RunFeature`/`RunBug` returning nothing | 5 |
| Shared `run(ctx, c, cfg, wtPath, label, prompt)` | 5 |
| Idempotency check before spending a session; skip on fetch error | 5 |
| Claude call shape (`Label: "uat"`, `Model: cfg.Models.UAT`, `Dir`, `SkipPermissions`, four disallowed tools) | 5 |
| Result persisted as `<seq>-uat.output.md` (no extra plumbing) | 5 (asserted by `TestUATLogsResultAsOutputFile`) |
| `parseUAT` semantics | 3 |
| Size guards: 8000-char truncate, 60000-char skip | 5 |
| Append via `Target.AppendIssueBody` | 5 |
| Every skip path logs issue number + reason | 5 |
| Rendered section = marker, blank line, heading, checklist, via a `comments.md.tmpl` define block | 3 |
| `GitHub.IssueBody` / `AppendIssueBody` through `g.gh` | 2 |
| `RunFeaturePipeline` / `RunBugPipeline` trailing `*UAT`; `handleIssue` constructs `&UAT{Target: o.gh, Num: n}` | 6, 7 |
| Feature route placement (after `resolveSpec`, before `runPlanThenExecute`) | 6 |
| Bug route placement (after both gates); `base` parameter | 7 |
| Rework runs no UAT step | untouched by design — `rework.go` is not modified, and Task 8 documents why |
| Two flat prompt templates with the shared output contract | 4 |
| `uatFeaturePrompt` / `uatBugPrompt` builders | 4 |
| `promptData()` gains the sentinels | 3 |
| `Models.UAT`, no fallback helper | 1 |
| `loope.json.example` + `docs/configuration.md` (incl. the no-inheritance note) | 1 |
| `docs/how-it-works.md` sentence per route | 8 |
| Error-handling table (all seven rows) | 5 (one test per row) |
| Testing list: parseUAT cases, idempotency, non-blocking both routes, ordering both routes, needs-info/already-done exclusion, session hygiene, size guards, `AppendIssueBody` arg shape, golden prompts, config round-trip | 1–7 |

No gaps.

**Placeholder scan:** every code step carries the literal code to write; every test step carries the actual test body; every doc step quotes the exact prose. No TBDs, no "handle errors appropriately", no "similar to Task N".

**Type consistency:** `parseUAT` returns `(string, bool)` and is called that way in Task 5. `uatSection(checklist string) string` is defined in Task 3 and called in Task 5. `uatFeaturePrompt(specPath)` / `uatBugPrompt(issue, base)` are defined in Task 4 and called in Task 5. `UATTarget`'s two methods match `GitHub.IssueBody` / `GitHub.AppendIssueBody` from Task 2 exactly (`(ctx, int) (string, error)` and `(ctx, int, string) error`), so `o.gh` satisfies the interface in Task 6. `Models.UAT` (Task 1) is read as `cfg.Models.UAT` in Task 5. `uatLabel`, `maxUATChars`, `maxIssueBodyChars` are defined once in Task 3 and used in Task 5 and in Task 5's tests. `fakeUATTarget` and `uatTestConfig()` are defined in Task 5's test file and reused in Tasks 6 and 7 (same package, so they are in scope).
