# Post-ship Code Review Rounds Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After `ship()` opens a PR, run a blocking, config-gated loop of Claude
sessions that invoke `/code-review --fix` against the diff, push their fixes,
and post findings as PR review comments, before the issue's label swaps
`ai-wip` → `ai-done`.

**Architecture:** A new `CodeReview` type (`codereview.go`) mirrors the
existing `UAT` type's shape (nil-safe receiver, a small GitHub-facing
interface, a Claude session per round) but is blocking and round-tracked
instead of fire-and-forget. `ship()` (`loop.go`) constructs and runs it
between `recordPR(...)` and `SwapLabels(...Done)`, logging (never
propagating) any error it returns. Two new `*GitHub` methods
(`PRNumberForBranch`, `ReviewComment`) give it a GitHub surface, one new
`Models.CodeReview *CodeReviewConfig` config block gates it on/off, and one
new prompt template drives the review-and-fix session.

**Tech Stack:** Go 1.25 (see `go.mod`), stdlib `encoding/json` + `text/template`
+ `os`/`path/filepath` for the same log-file patterns already used by
`uat.go`/`tracker.go`/`claude.go`, `gh`/`git` CLIs shelled via the existing
`Runner` interface.

**Spec:**
`docs/superpowers/specs/2026-08-19-post-ship-code-review-design.md`

## Global Constraints

- `Models.CodeReview` is `*CodeReviewConfig` (a pointer): JSON presence is the
  on/off switch, matching `Config.Telemetry *TelemetryConfig` — absence means
  the whole step is skipped, not "use defaults" (unlike `UAT`/`Execute`, which
  are value-typed).
- `CodeReviewConfig.Rounds <= 0` is treated as `1` (a config with `rounds: 0`
  or an omitted `rounds` field still runs one round).
- The review loop's own Claude calls never call `Claude.RecordSession` for the
  primary session file — same rule as `UAT` (`uat.go:107-108`): it must never
  clobber the resumable primary session pointer that `loop -rework` resumes.
  Its own resume state is the separate `<logDir>/codereview-round` file.
- Every review-loop Claude call uses `Label` values distinct from the primary
  resumable session: `codereview-<round>`.
- A review-loop failure (PR lookup error, session error, push error, a
  `STATUS: blocked` finding) is logged and stops the loop early, but **never**
  reverts or blocks the `ai-wip` → `ai-done` transition. `ship()` always
  proceeds to `SwapLabels` afterward.
- **Assumption (not fully specified in the spec):** the spec's sketch of
  `type CodeReview struct { Target CodeReviewTarget; Num int }` and
  `Run(ctx, c, cfg, wtPath, branch, base, logDir)` has no way to reach
  `Worktree.Push` (`worktree.go:104-109`), which step 3 of the design
  requires calling every round. This plan adds a `Push func(ctx
  context.Context, wtPath, branch string) error` field to `CodeReview`,
  populated by `ship()` as `o.wt.Push`. This mirrors how `Target` is already
  injected (real `*GitHub` in production, a fake in tests) rather than adding
  a `*Worktree` field and a `Runner`-shaped fake to every test.

---

## Task 1: Config — `CodeReviewConfig` and `Models.CodeReview`

**Files:**
- Modify: `config.go` (add `CodeReviewConfig` type and `Models.CodeReview` field)
- Test: `config_test.go`

**Interfaces:**
- Produces: `type CodeReviewConfig struct { ModelConfig; Rounds int }` (JSON
  tag `rounds` on `Rounds`; `ModelConfig`'s own fields — `model`, `effort`,
  `maxBudgetUSD`, `maxTurns` — flatten into the same JSON object via Go's
  anonymous-field embedding, since `ModelConfig` carries no `json` tag on
  itself). `Models.CodeReview *CodeReviewConfig` (JSON tag `codeReview`).

- [ ] **Step 1: Write the failing tests**

Add to `config_test.go`, right after `TestLoadConfigTelemetryAbsentByDefault`:

```go
func TestLoadConfigCodeReviewRoundTrips(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone",
		"repoSlug": "org/repo",
		"workDir": "/tmp/work",
		"models": {"codeReview": {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 5, "maxTurns": 40, "rounds": 2}}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.CodeReview == nil {
		t.Fatal("Models.CodeReview = nil, want the parsed block")
	}
	want := CodeReviewConfig{ModelConfig: ModelConfig{Model: "sonnet", Effort: "medium", MaxBudgetUSD: 5, MaxTurns: 40}, Rounds: 2}
	if *cfg.Models.CodeReview != want {
		t.Errorf("Models.CodeReview = %+v, want %+v", *cfg.Models.CodeReview, want)
	}
}

func TestLoadConfigCodeReviewAbsentByDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath":"/r","repoSlug":"o/r","workDir":"/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.CodeReview != nil {
		t.Fatalf("Models.CodeReview = %+v, want nil when the block is absent — an absent block must skip the step entirely, not run with defaults", cfg.Models.CodeReview)
	}
}

func TestLoadConfigCodeReviewZeroRoundsTreatedAsOne(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone", "repoSlug": "org/repo", "workDir": "/tmp/work",
		"models": {"codeReview": {"model": "sonnet"}}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.CodeReview == nil || cfg.Models.CodeReview.Rounds != 0 {
		t.Fatalf("Models.CodeReview.Rounds = %+v, want 0 as parsed — the <=0-means-1 default is CodeReview.Run's job, not LoadConfig's", cfg.Models.CodeReview)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail to compile**

Run: `go test ./... -run TestLoadConfigCodeReview -v`
Expected: FAIL — `undefined: CodeReviewConfig` (the type doesn't exist yet).

- [ ] **Step 3: Add the config type and field**

In `config.go`, add right after the `Models` struct's closing brace (after
line 42, before `func (m Models) executeConfig()`):

```go
// CodeReviewConfig is the config for the post-ship review-and-fix loop.
// Rounds <= 0 is treated as 1 by CodeReview.Run — LoadConfig does not apply
// that default itself, so a config that never sets rounds and one that
// explicitly sets "rounds": 0 are indistinguishable, and both mean "run once".
type CodeReviewConfig struct {
	ModelConfig
	Rounds int `json:"rounds"`
}
```

Then change the `Models` struct's `UAT` field block to add `CodeReview` right
after it:

```go
	// UAT is the config for the UAT-checklist session. Unlike Execute it has no
	// fallback helper: the block is used exactly as written, so an absent block
	// means the claude CLI's own defaults with no budget or turn cap. The session
	// is short and read-only, so a cheap model with a low cap is the right shape.
	UAT ModelConfig `json:"uat"`
	// CodeReview is the config for the post-ship review-and-fix loop. Unlike
	// Execute it has no fallback to Architect: a real model choice here matters
	// (the session must both find and fix issues), so an absent block means the
	// whole step is skipped, not "use defaults" — hence the pointer, mirroring
	// Telemetry rather than UAT's always-constructed value field.
	CodeReview *CodeReviewConfig `json:"codeReview"`
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run TestLoadConfigCodeReview -v`
Expected: PASS (all three new tests).

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add Models.CodeReview config block for post-ship review loop"
```

---

## Task 2: GitHub — `PRNumberForBranch` and `ReviewComment`

**Files:**
- Modify: `github.go`
- Test: `github_test.go`

**Interfaces:**
- Consumes: `g.gh(ctx, args...) (string, error)` (`github.go:36-47`, existing
  helper).
- Produces: `func (g *GitHub) PRNumberForBranch(ctx context.Context, branch
  string) (int, error)`; `func (g *GitHub) ReviewComment(ctx context.Context,
  prNumber int, body string) error`. Both used by `CodeReviewTarget` in Task
  3 — `*GitHub` must satisfy that interface once Task 3 lands.

- [ ] **Step 1: Write the failing tests**

Add to `github_test.go`, right after `TestPRURLForBranch`:

```go
func TestPRNumberForBranch(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `{"number":42}`}}}
	g := testGitHub(f)
	n, err := g.PRNumberForBranch(context.Background(), "ai/issue-5")
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Errorf("number = %d, want 42", n)
	}
	if !hasArg(f.calls[0].args, "view") || !hasArg(f.calls[0].args, "ai/issue-5") {
		t.Errorf("call args = %v", f.calls[0].args)
	}
	if got := argAfter(f.calls[0].args, "--json"); got != "number" {
		t.Errorf("--json = %q, want \"number\"", got)
	}
}

func TestPRNumberForBranchPropagatesError(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{err: errors.New("exit 1"), stderr: "no pull requests found"}}}
	g := testGitHub(f)
	if _, err := g.PRNumberForBranch(context.Background(), "ai/issue-5"); err == nil {
		t.Fatal("want an error when gh pr view fails")
	}
}

func TestReviewComment(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: ""}}}
	g := testGitHub(f)
	if err := g.ReviewComment(context.Background(), 42, "looks good"); err != nil {
		t.Fatal(err)
	}
	call := f.calls[0]
	if !hasArg(call.args, "review") || !hasArg(call.args, "42") || !hasArg(call.args, "--comment") {
		t.Errorf("args = %v, want a `gh pr review 42 --comment ...` call", call.args)
	}
	if argAfter(call.args, "--body") != "looks good" {
		t.Errorf("--body = %q", argAfter(call.args, "--body"))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestPRNumberForBranch|TestReviewComment' -v`
Expected: FAIL — `g.PRNumberForBranch undefined` / `g.ReviewComment undefined`.

- [ ] **Step 3: Implement the two methods**

In `github.go`, add at the end of the file, after `PRURLForBranch`:

```go
// PRNumberForBranch returns the number of the open PR whose head is branch,
// for CodeReview.Run to know where to post review findings.
func (g *GitHub) PRNumberForBranch(ctx context.Context, branch string) (int, error) {
	out, err := g.gh(ctx, "pr", "view", branch, "--repo", g.slug, "--json", "number")
	if err != nil {
		return 0, err
	}
	var v struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return 0, fmt.Errorf("parse pr view: %w", err)
	}
	return v.Number, nil
}

// ReviewComment posts a top-level PR review comment via `gh pr review
// --comment`, distinct from Comment (an issue-style comment): the post-ship
// code review loop's findings belong on the PR, not the issue.
func (g *GitHub) ReviewComment(ctx context.Context, prNumber int, body string) error {
	_, err := g.gh(ctx, "pr", "review", strconv.Itoa(prNumber), "--repo", g.slug, "--comment", "--body", body)
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run 'TestPRNumberForBranch|TestReviewComment' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add github.go github_test.go
git commit -m "feat: add GitHub.PRNumberForBranch and ReviewComment"
```

---

## Task 3: `codereview.go`, its prompt, and its comment template

This task is intentionally one unit, not split further: the prompt builders
(`codeReviewPrompt`, `codeReviewComment`) need the sentinel and status
constants that `codereview.go` owns, and `codereview.go`'s `Run` calls those
same builders — the two halves cannot compile or be tested independently of
each other, so splitting them would leave a task a reviewer could not
meaningfully approve or reject on its own.

**Files:**
- Create: `ai/prompts/codereview.md.tmpl`
- Create: `codereview.go`
- Create: `codereview_test.go`
- Modify: `ai/prompts/comments.md.tmpl` (add a `codereview-comment` define block)
- Modify: `prompts.go` (`promptData()` gains the two new sentinels, plus the
  two builder functions)
- Modify: `prompts_golden_test.go` (golden tests for both builders)
- Modify: `prompts_test.go` (register the two new templates + the two new
  sentinels for the no-hardcoding check)

**Interfaces:**
- Consumes: `c.Call(ctx, ClaudeCall{...}) (*ClaudeResult, error)`
  (`claude.go:103`, existing), `mustRender(name string, data map[string]any)
  string` (`prompts.go:42-48`, existing), `promptData() map[string]any`
  (`prompts.go:54-66`, existing).
- Produces:
  - `const codeReviewBeginSentinel = "CODEREVIEW_BEGIN"`
  - `const codeReviewEndSentinel = "CODEREVIEW_END"`
  - `const codeReviewRoundFile = "codereview-round"`
  - `type codeReviewStatus string` with `codeReviewClean`, `codeReviewFixed`,
    `codeReviewBlocked` constants
  - `type CodeReviewTarget interface { PRNumberForBranch(ctx
    context.Context, branch string) (int, error); ReviewComment(ctx
    context.Context, prNumber int, body string) error }`
  - `type CodeReview struct { Target CodeReviewTarget; Push func(ctx
    context.Context, wtPath, branch string) error; Num int }`
  - `func (r *CodeReview) Run(ctx context.Context, c *Claude, cfg *Config,
    wtPath, branch, base, logDir string) error` — used by `ship()` in Task 4.
  - `func parseCodeReview(s string) (codeReviewStatus, string)`
  - `func recordCodeReviewRound(logDir string, i int)` /
    `func lastCompletedRound(logDir string) int`
  - `func codeReviewPrompt(round, rounds int, base string) string`
  - `func codeReviewComment(round, rounds int, status codeReviewStatus,
    summary string) string`

- [ ] **Step 1: Create the prompt template**

Create `ai/prompts/codereview.md.tmpl`:

```
Run /code-review against origin/{{.Base}}...HEAD with --fix applied (round {{.Round}} of {{.Rounds}}), then commit any changes it makes.

Output ONLY a status line and summary between a line reading {{.CodeReviewBeginSentinel}} and a line reading
{{.CodeReviewEndSentinel}}. Print nothing before or after those two lines. The
first line between them is one of:
- STATUS: clean — /code-review found nothing to fix.
- STATUS: fixed — followed by a short bullet summary of what was fixed.
- STATUS: blocked — followed by a short explanation of what can't be safely auto-fixed.

HEADLESS MODE: do not ask questions; make reasonable calls.
```

- [ ] **Step 2: Add the comment template**

In `ai/prompts/comments.md.tmpl`, add at the end of the file (after the
`uat-section` define block, keeping the existing blank-line-between-defines
style):

```
{{define "codereview-comment"}}<!-- loope:codereview:{{.Round}} -->
🤖 Code review round {{.Round}}/{{.Rounds}}: {{.Status}}

{{.Summary}}{{end}}
```

- [ ] **Step 3: Add the sentinel constants and inject them into `promptData()`**

In `prompts.go`, add to the map literal in `promptData()` (`prompts.go:54-66`):

```go
func promptData() map[string]any {
	return map[string]any{
		"ConfidenceSentinel":      confidenceSentinel,
		"SpecReadySentinel":       specReadySentinel,
		"ReadySentinel":           readySentinel,
		"AlreadyDoneSentinel":     alreadyDoneSentinel,
		"DoneConfirmSentinel":     doneConfirmSentinel,
		"UATBeginSentinel":        uatBeginSentinel,
		"UATEndSentinel":          uatEndSentinel,
		"UATMarker":               uatMarker,
		"BotMarker":               botMarker,
		"CodeReviewBeginSentinel": codeReviewBeginSentinel,
		"CodeReviewEndSentinel":   codeReviewEndSentinel,
	}
}
```

The constants `codeReviewBeginSentinel`/`codeReviewEndSentinel` themselves are
declared in `codereview.go` (Step 8 below), not here — this file only
references them, the same way `promptData()` already references
`uatBeginSentinel`/`uatEndSentinel`, which live in `uat.go`. This file will
not compile until Step 8 lands; that's expected within a single task and is
resolved by Step 10's whole-package test run.

- [ ] **Step 4: Add the builder functions**

Add to `prompts.go`, at the end of the file:

```go
func codeReviewPrompt(round, rounds int, base string) string {
	d := promptData()
	d["Round"] = round
	d["Rounds"] = rounds
	d["Base"] = base
	return mustRender("codereview.md.tmpl", d)
}

func codeReviewComment(round, rounds int, status codeReviewStatus, summary string) string {
	d := promptData()
	d["Round"] = round
	d["Rounds"] = rounds
	d["Status"] = string(status)
	d["Summary"] = summary
	return mustRender("codereview-comment", d)
}
```

(`codeReviewStatus` is declared in Step 8's `codereview.go`; this function
does not compile until that lands, same dependency as Step 3.)

- [ ] **Step 5: Register the new templates in `prompts_test.go`**

In `prompts_test.go`, add two entries to `promptTestData` (after the
`uat-bug.md.tmpl` entry):

```go
	"codereview.md.tmpl": {"Round": 1, "Rounds": 2, "Base": "main"},
	"codereview-comment": {"Round": 1, "Rounds": 2, "Status": "fixed", "Summary": "S"},
```

And add the two sentinels to `TestNoSentinelIsHardcodedInATemplate`'s
`sentinels` slice:

```go
	sentinels := []string{confidenceSentinel, specReadySentinel, readySentinel, alreadyDoneSentinel, doneConfirmSentinel,
		uatBeginSentinel, uatEndSentinel, uatMarker, codeReviewBeginSentinel, codeReviewEndSentinel}
```

- [ ] **Step 6: Write the golden tests**

Add to `prompts_golden_test.go`, at the end of the file:

```go
func TestGoldenCodeReviewPrompt(t *testing.T) {
	want := `Run /code-review against origin/main...HEAD with --fix applied (round 1 of 2), then commit any changes it makes.

Output ONLY a status line and summary between a line reading CODEREVIEW_BEGIN and a line reading
CODEREVIEW_END. Print nothing before or after those two lines. The
first line between them is one of:
- STATUS: clean — /code-review found nothing to fix.
- STATUS: fixed — followed by a short bullet summary of what was fixed.
- STATUS: blocked — followed by a short explanation of what can't be safely auto-fixed.

HEADLESS MODE: do not ask questions; make reasonable calls.`
	check(t, "codeReviewPrompt", codeReviewPrompt(1, 2, "main"), want)
}

func TestGoldenCodeReviewComment(t *testing.T) {
	check(t, "codeReviewComment", codeReviewComment(1, 2, codeReviewFixed, "- Fixed a null check."),
		"<!-- loope:codereview:1 -->\n🤖 Code review round 1/2: fixed\n\n- Fixed a null check.")
}
```

- [ ] **Step 7: Write the `codereview.go` unit tests**

Create `codereview_test.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// fakeCodeReviewTarget stands in for *GitHub: it records what was posted and
// can be scripted to fail either operation, mirroring fakeUATTarget.
type fakeCodeReviewTarget struct {
	prNum      int
	prErr      error
	comments   []string
	commentErr error
}

func (f *fakeCodeReviewTarget) PRNumberForBranch(ctx context.Context, branch string) (int, error) {
	if f.prErr != nil {
		return 0, f.prErr
	}
	return f.prNum, nil
}

func (f *fakeCodeReviewTarget) ReviewComment(ctx context.Context, prNumber int, body string) error {
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments = append(f.comments, body)
	return nil
}

func codeReviewTestConfig(rounds int) *Config {
	return &Config{Models: Models{CodeReview: &CodeReviewConfig{
		ModelConfig: ModelConfig{Model: "sonnet", Effort: "medium", MaxTurns: 30},
		Rounds:      rounds,
	}}}
}

// codeReviewResult builds a fake claude payload whose result carries a
// fenced STATUS line and summary.
func codeReviewResult(status, summary string) string {
	return claudeJSON("Reviewing...\n"+codeReviewBeginSentinel+"\nSTATUS: "+status+"\n"+summary+"\n"+codeReviewEndSentinel, "cr-1")
}

func noopPush(ctx context.Context, wtPath, branch string) error { return nil }

func TestParseCodeReviewClean(t *testing.T) {
	status, summary := parseCodeReview(codeReviewBeginSentinel + "\nSTATUS: clean\nNothing to fix.\n" + codeReviewEndSentinel)
	if status != codeReviewClean || summary != "Nothing to fix." {
		t.Errorf("status=%q summary=%q", status, summary)
	}
}

func TestParseCodeReviewFixed(t *testing.T) {
	status, summary := parseCodeReview(codeReviewBeginSentinel + "\nSTATUS: fixed\n- fixed A\n- fixed B\n" + codeReviewEndSentinel)
	if status != codeReviewFixed || summary != "- fixed A\n- fixed B" {
		t.Errorf("status=%q summary=%q", status, summary)
	}
}

func TestParseCodeReviewBlocked(t *testing.T) {
	status, summary := parseCodeReview(codeReviewBeginSentinel + "\nSTATUS: blocked\nCan't safely fix X.\n" + codeReviewEndSentinel)
	if status != codeReviewBlocked || summary != "Can't safely fix X." {
		t.Errorf("status=%q summary=%q", status, summary)
	}
}

func TestParseCodeReviewFencePresentNoStatusLine(t *testing.T) {
	raw := codeReviewBeginSentinel + "\nI reviewed the code.\n" + codeReviewEndSentinel
	status, summary := parseCodeReview(raw)
	if status != codeReviewBlocked || summary != raw {
		t.Errorf("status=%q summary=%q, want blocked with the raw result verbatim", status, summary)
	}
}

func TestParseCodeReviewNoFence(t *testing.T) {
	status, summary := parseCodeReview("I could not find the marker.")
	if status != codeReviewBlocked || summary != "I could not find the marker." {
		t.Errorf("status=%q summary=%q, want blocked with the raw result verbatim", status, summary)
	}
}

func TestCodeReviewStopsOnClean(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := &Claude{runner: f}
	var pushCalls int
	cr := &CodeReview{Target: tgt, Push: func(ctx context.Context, wtPath, branch string) error {
		pushCalls++
		return nil
	}, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (stop after round 1's clean, even though Rounds=3)", len(f.calls))
	}
	if pushCalls != 1 {
		t.Errorf("push calls = %d, want 1", pushCalls)
	}
	if len(tgt.comments) != 1 || !strings.Contains(tgt.comments[0], "clean") {
		t.Errorf("comments = %v", tgt.comments)
	}
	if lastCompletedRound(logDir) != 1 {
		t.Errorf("lastCompletedRound = %d, want 1", lastCompletedRound(logDir))
	}
}

func TestCodeReviewRunsAllRoundsWhenAlwaysFixed(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{
		{stdout: codeReviewResult("fixed", "- fixed A")},
		{stdout: codeReviewResult("fixed", "- fixed B")},
	}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(2), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("claude calls = %d, want 2 (exactly Rounds, since status never stops the loop)", len(f.calls))
	}
	if len(tgt.comments) != 2 {
		t.Errorf("comments = %d, want 2", len(tgt.comments))
	}
	if lastCompletedRound(logDir) != 2 {
		t.Errorf("lastCompletedRound = %d, want 2", lastCompletedRound(logDir))
	}
}

func TestCodeReviewStopsOnBlocked(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("blocked", "Can't safely fix X.")}}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (stop on blocked, even though Rounds=3)", len(f.calls))
	}
	if len(tgt.comments) != 1 || !strings.Contains(tgt.comments[0], "Can't safely fix X.") {
		t.Errorf("comments = %v", tgt.comments)
	}
}

func TestCodeReviewResumesFromRoundFile(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	logDir := t.TempDir()
	recordCodeReviewRound(logDir, 1)
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (round 1 already recorded done, this call is round 2)", len(f.calls))
	}
	if len(tgt.comments) != 1 || !strings.Contains(tgt.comments[0], "round 2/3") {
		t.Errorf("comments = %v, want a round-2 comment", tgt.comments)
	}
}

func TestCodeReviewPRLookupFailureSkipsLoop(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prErr: fmt.Errorf("gh: 404")}
	f := &fakeRunner{}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir())
	if err == nil {
		t.Fatal("want an error when the PR lookup fails")
	}
	if len(f.calls) != 0 {
		t.Errorf("claude calls = %d, want 0 — there is nowhere to post findings", len(f.calls))
	}
}

func TestCodeReviewPushFailureStopsLoop(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("fixed", "- fixed A")}}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: func(ctx context.Context, wtPath, branch string) error {
		return fmt.Errorf("push failed")
	}, Num: 7}
	err := cr.Run(context.Background(), c, codeReviewTestConfig(2), "/wt", "b", "main", t.TempDir())
	if err == nil {
		t.Fatal("want an error when push fails")
	}
	if len(tgt.comments) != 0 {
		t.Errorf("comments = %d, want 0 — push failed before anything was posted", len(tgt.comments))
	}
}

func TestCodeReviewSurvivesCommentFailure(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42, commentErr: fmt.Errorf("gh: 422")}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := &Claude{runner: f}
	logDir := t.TempDir()
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if lastCompletedRound(logDir) != 1 {
		t.Errorf("lastCompletedRound = %d, want 1 even though the comment post failed — the round isn't repeated just because the comment failed", lastCompletedRound(logDir))
	}
}

func TestCodeReviewNilReceiverIsSafe(t *testing.T) {
	var cr *CodeReview
	f := &fakeRunner{}
	c := &Claude{runner: f}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %d, want 0", len(f.calls))
	}
}

func TestCodeReviewNoTargetIsSafe(t *testing.T) {
	cr := &CodeReview{}
	f := &fakeRunner{}
	c := &Claude{runner: f}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %d, want 0", len(f.calls))
	}
}

func TestCodeReviewNilConfigBlockIsSafe(t *testing.T) {
	cr := &CodeReview{Target: &fakeCodeReviewTarget{prNum: 42}, Push: noopPush}
	f := &fakeRunner{}
	c := &Claude{runner: f}
	if err := cr.Run(context.Background(), c, &Config{}, "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %d, want 0 — a nil Models.CodeReview must disable the step entirely", len(f.calls))
	}
}

func TestCodeReviewCallUsesConfiguredModelAndDistinctLabel(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	call := f.calls[0]
	if call.dir != "/wt" || argAfter(call.args, "--model") != "sonnet" {
		t.Errorf("call = %+v", call)
	}
	if !strings.Contains(call.stdin, "/code-review") {
		t.Errorf("prompt should invoke /code-review: %s", call.stdin)
	}
}
```

- [ ] **Step 8: Run the tests to verify they fail to compile**

Run: `go test ./... -run 'CodeReview|Prompt|Template|Sentinel' -v`
Expected: FAIL — `undefined: CodeReview` (the type doesn't exist yet; this
also covers Steps 1-6's prompt/golden tests, which are equally uncompilable
until this step lands).

- [ ] **Step 9: Implement `codereview.go`**

Create `codereview.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// codeReviewBeginSentinel / codeReviewEndSentinel fence the status line and
	// summary inside the session's result text. Injected into the prompt via
	// promptData(), never hardcoded in a template.
	codeReviewBeginSentinel = "CODEREVIEW_BEGIN"
	codeReviewEndSentinel   = "CODEREVIEW_END"
	// codeReviewRoundFile holds the last completed round number, so a daemon
	// restart mid-loop resumes at the next round instead of redoing finished
	// ones (the CLAUDE.md "continue from existing state" principle).
	codeReviewRoundFile = "codereview-round"
)

// codeReviewStatus is the outcome a review-and-fix session reports for one
// round, parsed from its STATUS: line.
type codeReviewStatus string

const (
	codeReviewClean   codeReviewStatus = "clean"
	codeReviewFixed   codeReviewStatus = "fixed"
	codeReviewBlocked codeReviewStatus = "blocked"
)

// CodeReviewTarget is the GitHub surface the review loop reads the PR number
// from and posts findings to. *GitHub satisfies it; tests substitute a fake.
type CodeReviewTarget interface {
	PRNumberForBranch(ctx context.Context, branch string) (int, error)
	ReviewComment(ctx context.Context, prNumber int, body string) error
}

// CodeReview runs the post-ship review-and-fix loop: one or more Claude
// sessions that invoke /code-review --fix against the shipped diff, push
// whatever they commit, and post the finding as a PR review comment. A nil
// *CodeReview, a nil Target, or a nil cfg.Models.CodeReview all disable the
// step entirely, so callers never need a nil guard.
type CodeReview struct {
	Target CodeReviewTarget
	// Push pushes wtPath's branch to origin. Set to (*Worktree).Push by the
	// wiring in ship() — injected the same way Target is, so tests can fake it
	// without a real Worktree/Runner.
	Push func(ctx context.Context, wtPath, branch string) error
	Num  int
}

// Run drives the round loop: resolve the PR once, then for each round from
// lastCompletedRound(logDir)+1 through cfg.Models.CodeReview.Rounds (<=0
// treated as 1), run one Claude session, push whatever it committed, parse
// its status, post the finding, and persist progress. It stops early on
// STATUS: clean or STATUS: blocked, and stops (returning an error) if the PR
// lookup, the Claude call, or the push fails — but every one of those errors
// is the caller's (ship()'s) to log, never to propagate into a park: the PR
// already shipped successfully.
func (r *CodeReview) Run(ctx context.Context, c *Claude, cfg *Config, wtPath, branch, base, logDir string) error {
	if r == nil || r.Target == nil || cfg.Models.CodeReview == nil {
		return nil
	}
	rounds := cfg.Models.CodeReview.Rounds
	if rounds <= 0 {
		rounds = 1
	}
	prNum, err := r.Target.PRNumberForBranch(ctx, branch)
	if err != nil {
		return fmt.Errorf("issue #%d: code review PR lookup failed: %w", r.Num, err)
	}
	for i := lastCompletedRound(logDir) + 1; i <= rounds; i++ {
		res, err := c.Call(ctx, ClaudeCall{
			Dir: wtPath, Label: fmt.Sprintf("codereview-%d", i), Prompt: codeReviewPrompt(i, rounds, base),
			Model:           cfg.Models.CodeReview.ModelConfig,
			SkipPermissions: true,
		})
		if err != nil {
			return fmt.Errorf("issue #%d: code review round %d session failed: %w", r.Num, i, err)
		}
		if err := r.Push(ctx, wtPath, branch); err != nil {
			return fmt.Errorf("issue #%d: code review round %d push failed: %w", r.Num, i, err)
		}
		status, summary := parseCodeReview(res.Result)
		if err := r.Target.ReviewComment(ctx, prNum, codeReviewComment(i, rounds, status, summary)); err != nil {
			log.Printf("issue #%d: code review round %d comment failed: %v", r.Num, i, err)
		}
		recordCodeReviewRound(logDir, i)
		if status != codeReviewFixed {
			// clean: nothing left to do. blocked: the session says it can't
			// safely fix the finding, so another round won't help either.
			break
		}
	}
	return nil
}

// parseCodeReview extracts the status and summary from a review session's
// result: the text between codeReviewBeginSentinel/codeReviewEndSentinel,
// whose first line is "STATUS: clean|fixed|blocked" and the rest is the
// summary. A missing fence, a missing/unrecognized STATUS line, is treated as
// blocked with the raw result as the summary, so an off-contract session
// still surfaces something rather than being silently dropped.
func parseCodeReview(s string) (codeReviewStatus, string) {
	i := strings.Index(s, codeReviewBeginSentinel)
	if i < 0 {
		return codeReviewBlocked, strings.TrimSpace(s)
	}
	rest := s[i+len(codeReviewBeginSentinel):]
	if j := strings.Index(rest, codeReviewEndSentinel); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSpace(rest)
	lines := strings.SplitN(rest, "\n", 2)
	first := strings.TrimSpace(lines[0])
	statusText, ok := strings.CutPrefix(first, "STATUS:")
	if !ok {
		return codeReviewBlocked, strings.TrimSpace(s)
	}
	summary := ""
	if len(lines) > 1 {
		summary = strings.TrimSpace(lines[1])
	}
	switch strings.TrimSpace(statusText) {
	case string(codeReviewClean):
		return codeReviewClean, summary
	case string(codeReviewFixed):
		return codeReviewFixed, summary
	case string(codeReviewBlocked):
		return codeReviewBlocked, summary
	default:
		return codeReviewBlocked, strings.TrimSpace(s)
	}
}

// recordCodeReviewRound writes the last completed round number to
// <logDir>/codereview-round. Best-effort, like the other log-writers in
// tracker.go: a no-op on an empty logDir.
func recordCodeReviewRound(logDir string, i int) {
	if logDir == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, codeReviewRoundFile), []byte(strconv.Itoa(i)), 0o644)
}

// lastCompletedRound reads the round progress written by
// recordCodeReviewRound, or 0 if none is recorded (fresh loop, or the file is
// missing/corrupt).
func lastCompletedRound(logDir string) int {
	b, err := os.ReadFile(filepath.Join(logDir, codeReviewRoundFile))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}
```

- [ ] **Step 10: Run every new and existing test**

Run: `go build ./... && go test ./... -v`
Expected: PASS across the whole package — this is the point where the
prompt/golden tests from Steps 1-6 (which depend on this step's constants)
and this step's own `codereview_test.go` tests all compile and pass together.

- [ ] **Step 11: Commit**

```bash
git add ai/prompts/codereview.md.tmpl ai/prompts/comments.md.tmpl prompts.go prompts_test.go prompts_golden_test.go codereview.go codereview_test.go
git commit -m "feat: add the code-review-and-fix loop (codereview.go) and its prompt"
```

---

## Task 4: Wire the loop into `ship()`

**Files:**
- Modify: `loop.go` (`ship()` signature and body, `handleIssue`'s call site)
- Modify: `loop_test.go` (extend the shared `fakeEnv` gh/claude handlers,
  integration tests)

**Interfaces:**
- Consumes: `CodeReview.Run(ctx, c, cfg, wtPath, branch, base, logDir) error`
  (Task 3). `o.wt.Push` (`worktree.go:104-109`, existing) as the `Push` field.

- [ ] **Step 1: Write the failing integration tests**

In `loop_test.go`, extend `newFakeEnv`'s `env.f.handler` closure (defined
around line 20) to answer the two new `gh` subcommands and the code-review
Claude prompt. Replace the existing `gh` switch body:

```go
		case "gh":
			switch {
			case strings.HasPrefix(joined, "issue list"):
				return `[{"number": 7, "title": "Fix crash", "body": "boom", "labels": [{"name": "ai-agent"}]}]`, "", nil
			case strings.HasPrefix(joined, "issue view"):
				return `{"title": "Fix crash", "body": "boom", "comments": []}`, "", nil
			case strings.HasPrefix(joined, "pr create"):
				return "https://github.com/org/repo/pull/99\n", "", nil
			case strings.HasPrefix(joined, "pr view"):
				return `{"number": 99, "url": "https://github.com/org/repo/pull/99"}`, "", nil
			case strings.HasPrefix(joined, "pr review"):
				return "", "", nil
			}
			return "", "", nil
```

and the `claude` case's body (add the code-review branch before the
`failClaude`/default fallback):

```go
		case "claude":
			prompt := c.stdin
			if strings.Contains(prompt, "triage agent") {
				return claudeJSON(`{"issueNumber": 7, "kind": "bug", "reason": "small"}`, "t1"), "", nil
			}
			if strings.Contains(prompt, "/code-review") {
				return claudeJSON("Reviewing...\n"+codeReviewBeginSentinel+"\nSTATUS: clean\nNothing to fix.\n"+codeReviewEndSentinel, "cr1"), "", nil
			}
			if env.failClaude {
				return "", "boom", fmt.Errorf("exit 1")
			}
			return claudeJSON("Fixed and committed.", "d1"), "", nil
```

Then add, at the end of `loop_test.go`:

```go
// TestShipSkipsCodeReviewWhenNotConfigured verifies an absent
// Models.CodeReview block makes ship() behave exactly as before: no PR-number
// lookup, no review comment.
func TestShipSkipsCodeReviewWhenNotConfigured(t *testing.T) {
	env := newFakeEnv(t)
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "pr view")) != 0 || len(env.callsMatching("gh", "pr review")) != 0 {
		t.Error("code review must make no gh calls when Models.CodeReview is nil")
	}
	if env.readLocalState(7) != "ai-done" {
		t.Errorf("state = %q, want ai-done", env.readLocalState(7))
	}
}

// TestShipRunsCodeReviewWhenConfigured verifies ship() invokes the post-ship
// code review loop and still reaches ai-done.
func TestShipRunsCodeReviewWhenConfigured(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	o.cfg.Models.CodeReview = &CodeReviewConfig{ModelConfig: ModelConfig{Model: "sonnet"}, Rounds: 1}
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "pr review")) != 1 {
		t.Errorf("want exactly one pr review call, got %v", env.callsMatching("gh", "pr review"))
	}
	if env.readLocalState(7) != "ai-done" {
		t.Errorf("state = %q, want ai-done even with code review enabled", env.readLocalState(7))
	}
}

// TestShipReachesDoneWhenCodeReviewErrors verifies a code-review loop failure
// (here, the PR lookup fails) never blocks the ai-done transition — the spec
// treats code review as additive quality, never a shipping gate.
func TestShipReachesDoneWhenCodeReviewErrors(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	o.cfg.Models.CodeReview = &CodeReviewConfig{ModelConfig: ModelConfig{Model: "sonnet"}, Rounds: 1}
	orig := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.HasPrefix(joined, "pr view") {
			return "", "no pull requests found", fmt.Errorf("exit 1")
		}
		return orig(c)
	}
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if env.readLocalState(7) != "ai-done" {
		t.Errorf("state = %q, want ai-done even though code review errored", env.readLocalState(7))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestShip.*CodeReview' -v`
Expected: FAIL — `TestShipSkipsCodeReviewWhenNotConfigured` and friends should
currently pass trivially (ship() makes no such calls yet) except
`TestShipRunsCodeReviewWhenConfigured`, which FAILs because `ship()` doesn't
call `CodeReview.Run` yet, so `pr review` is never called. Confirm that
specific failure before proceeding.

- [ ] **Step 3: Wire `CodeReview` into `ship()`**

In `loop.go`, change `ship()`'s signature (line 482) from:

```go
func (o *Orchestrator) ship(ctx context.Context, issue Issue, wtPath, branch, base, kind string) error {
```

to:

```go
func (o *Orchestrator) ship(ctx context.Context, issue Issue, c *Claude, wtPath, branch, base, kind string) error {
```

and insert the review loop between `recordPR(...)` and the `SwapLabels(...)`
call (loop.go:501-503):

```go
	_ = o.gh.Comment(ctx, n, prComment(url))
	recordPR(o.issueLogDir(n), url)
	cr := &CodeReview{Target: o.gh, Push: o.wt.Push, Num: n}
	if err := cr.Run(ctx, c, o.cfg, wtPath, branch, base, o.issueLogDir(n)); err != nil {
		// The review loop's own errors never revert or re-park a successful
		// ship — it is a quality pass layered on top, not a gate on shipping.
		log.Printf("issue #%d: code review loop error: %v", n, err)
	}
	if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Done); err != nil {
```

Update the doc comment above `ship()` (loop.go:472-481) to mention the new
step, appending one sentence:

```go
// ship pushes the branch, opens (or recovers) the PR, comments the URL, runs
// the optional post-ship code review loop, and swaps WIP->Done. A
// deterministic tooling failure here (commit count, push, PR create) happens
// AFTER the pipeline has already produced commits, so it parks for rework —
// preserving the worktree, branch, and session, so a human who removes the
// label gets a run that builds on those commits instead of re-running the
// whole pipeline from zero. A pipeline that produced no commits also parks.
// The worktree and branch are never removed here either (spec Decision 3): a
// shipped issue's worktree sits on disk permanently, same as a parked or
// stopped one already does — an accepted, explicit trade-off with no cleanup
// mechanism in scope. The code review loop's own errors are logged, never
// propagated: it is a quality pass layered on top of a successful ship, not a
// gate on it. Returns nil only when fully shipped.
```

Finally, update `handleIssue`'s call site (loop.go:287) from:

```go
	return o.ship(ctx, issue, wtPath, branch, base, kind)
```

to:

```go
	return o.ship(ctx, issue, c, wtPath, branch, base, kind)
```

(`c` is already in scope in `handleIssue` — it's the `*Claude` constructed at
loop.go:250.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test ./... -v`
Expected: PASS across the whole package, including
`TestShipSkipsCodeReviewWhenNotConfigured`,
`TestShipRunsCodeReviewWhenConfigured`,
`TestShipReachesDoneWhenCodeReviewErrors`, and every pre-existing test in
`loop_test.go` (e.g. `TestParkWritesCauseAndShipClearsIt`,
`TestProcessOnceRecordsLocalStateDone`) unaffected by the new `ship()`
parameter.

- [ ] **Step 5: Commit**

```bash
git add loop.go loop_test.go
git commit -m "feat: run the post-ship code review loop before ai-done"
```

---

## Task 5: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Full build and test suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all pass, no vet warnings.

- [ ] **Step 2: gofmt check**

Run: `gofmt -l .`
Expected: empty output (no files need formatting). If any file is listed, run
`gofmt -w <file>` and re-run Step 1.

- [ ] **Step 3: Confirm no leftover TODO/stub markers**

Run: `grep -rn "TODO\|FIXME" codereview.go codereview_test.go github.go config.go loop.go ai/prompts/codereview.md.tmpl`
Expected: no output.

- [ ] **Step 4: Commit if Step 2 produced formatting fixes**

Only run this if Step 2 found something to fix:

```bash
git add -u
git commit -m "chore: gofmt"
```

If Step 2 found nothing, there is nothing to commit for this task.
