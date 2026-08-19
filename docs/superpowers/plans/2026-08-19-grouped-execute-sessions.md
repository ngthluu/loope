# Grouped Execute Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator cap how many plan steps one execute session attempts (`stepsPerSession` in `Config`), so the feature pipeline's execute step runs across several fresh, bounded Claude sessions instead of one unbounded session, while leaving the default (unset/`0`) behavior byte-identical to today.

**Architecture:** `executePlan` in `pipeline_feature.go` dispatches on `cfg.StepsPerSession`. `0` (default) keeps the existing single-session call unchanged. A positive value routes to `executePlanGrouped`, which loops spawning fresh sessions (one per group, up to a safety cap), each told via prompt to implement only the next N steps and end with a `GROUP_DONE`/`PLAN_COMPLETE` sentinel; a mid-group failure or missing sentinel is retried on the *same* session via `--resume` with a short continuation prompt, up to a bounded retry count. No plan-parsing is added anywhere — every session figures out where to resume purely by reading the plan file and git log in its own worktree, exactly the "recover and continue from existing state" pattern already documented in `CLAUDE.md` for `Worktree.Create`.

**Tech Stack:** Go (stdlib only: `text/template`, `context`, `strings`, `fmt`), the existing `Claude`/`Runner`/`ClaudeCall` abstractions in `claude.go`/`runner.go`, and the project's embedded-template prompt system (`prompts.go`, `ai/prompts/*.md.tmpl`).

**Spec:** `docs/superpowers/specs/2026-08-19-grouped-execute-sessions-design.md`

## Global Constraints

- `StepsPerSession` is a new field on `Config` (`config.go`), JSON key `stepsPerSession`, type `int`. `0` (zero value / absent key) must preserve today's single-session execute behavior exactly — no sentinel requirement, no prompt change, no test regression.
- Every session in a group is a **brand-new** Claude session (no `--resume`) — only mid-group *retries* resume.
- Sentinel constants are never hardcoded into a `.tmpl` file — they flow through `promptData()` in `prompts.go`, per the existing pattern for every other sentinel.
- `subagent-driven-development` continues to run *inside* each execute session unchanged; this feature only adds a boundary *between* sessions.
- No deterministic parsing of the plan file's step/task structure anywhere.

---

## File Structure

- Modify `config.go` — add `Config.StepsPerSession`.
- Modify `pipeline_feature.go` — add `groupDoneSentinel`/`planCompleteSentinel` consts, `executeGroupPrompt`/`executeContinuePrompt` builders, rewrite `executePlan` to dispatch, add `executePlanGrouped`/`runGroupWithRetry`.
- Modify `prompts.go` — add `GroupDoneSentinel`/`PlanCompleteSentinel` to `promptData()`.
- Modify `ai/prompts/execute.md.tmpl` — add the conditional grouped-execution block.
- Create `ai/prompts/execute-continue.md.tmpl` — the mid-group continuation prompt.
- Modify `config_test.go` — round-trip test for `stepsPerSession`.
- Modify `prompts_golden_test.go` — golden tests for the new/changed prompt builders.
- Modify `pipeline_feature_test.go` — behavioral tests for `executePlan` dispatch, `executePlanGrouped`, `runGroupWithRetry`.
- Modify `loope.json.example` — document the new field.
- Modify `docs/configuration.md` — document the new field.

---

### Task 1: `Config.StepsPerSession`

**Files:**
- Modify: `config.go:95-110` (the `Config` struct)
- Test: `config_test.go`

**Interfaces:**
- Produces: `Config.StepsPerSession int` (JSON key `"stepsPerSession"`), read by `executePlan` in Task 4.

- [ ] **Step 1: Write the failing test**

Add to `config_test.go` (near the other `TestLoadConfig*` tests, e.g. after `TestLoadConfigTicketsPerCycleOverride`):

```go
func TestLoadConfigStepsPerSessionDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StepsPerSession != 0 {
		t.Errorf("StepsPerSession = %d, want 0 (absent key preserves single-session behavior)", cfg.StepsPerSession)
	}
}

func TestLoadConfigStepsPerSessionOverride(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w", "stepsPerSession": 5}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StepsPerSession != 5 {
		t.Errorf("StepsPerSession = %d, want 5", cfg.StepsPerSession)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestLoadConfigStepsPerSession -v`
Expected: FAIL — `cfg.StepsPerSession` does not compile (`Config` has no field `StepsPerSession`).

- [ ] **Step 3: Add the field**

In `config.go`, add `StepsPerSession` to the `Config` struct (sibling of `MaxQARounds`, since both are execute-pipeline bounds):

```go
type Config struct {
	RepoPath            string      `json:"repoPath"`
	RepoSlug            string      `json:"repoSlug"`
	EligibleLabel       string      `json:"eligibleLabel"`
	PollIntervalSec     int         `json:"pollIntervalSec"`
	TicketsPerCycle     int         `json:"ticketsPerCycle"`
	WorkDir             string      `json:"workDir"`
	Addr                string      `json:"addr"`
	PersonaPath         string      `json:"personaPath"`
	ClaudeConfigDir     string      `json:"claudeConfigDir"`
	MaxQARounds         int         `json:"maxQARounds"`
	ConfidenceThreshold int         `json:"confidenceThreshold"`
	// StepsPerSession caps how many plan steps one feature-pipeline execute
	// session attempts before handing off to a fresh session. 0 (the zero
	// value, and the default when the key is absent) means "unbounded": one
	// execute session implements the whole plan, exactly as before this field
	// existed. See executePlan in pipeline_feature.go.
	StepsPerSession int         `json:"stepsPerSession"`
	StateLabels     StateLabels `json:"stateLabels"`
	GitHubRetry     RetryConfig `json:"githubRetry"`
	Models          Models      `json:"models"`
}
```

Do not add a default for it in `LoadConfig`'s seed struct literal — the zero value (`0`) *is* the default, so an absent key already resolves correctly via `json.Unmarshal`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestLoadConfigStepsPerSession -v`
Expected: PASS

- [ ] **Step 5: Run the full config test file to check for regressions**

Run: `go test ./... -run TestLoadConfig -v`
Expected: PASS (all existing config tests unaffected)

- [ ] **Step 6: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add optional stepsPerSession config field"
```

---

### Task 2: Sentinels and `promptData()` wiring

**Files:**
- Modify: `pipeline_feature.go:13-15` (const block alongside `readySentinel`/`specReadySentinel`)
- Modify: `prompts.go:54-66` (`promptData()`)
- Test: `prompts_test.go` (verify no render breakage — existing `TestEveryPromptFileOnDiskIsParsed`-style tests already cover this; no new test file needed here, golden tests land in Task 3)

**Interfaces:**
- Produces: `groupDoneSentinel = "GROUP_DONE"`, `planCompleteSentinel = "PLAN_COMPLETE"` (Go consts in `pipeline_feature.go`); `promptData()` map keys `"GroupDoneSentinel"` and `"PlanCompleteSentinel"`, consumed by the templates in Task 3.

- [ ] **Step 1: Add the sentinel consts**

In `pipeline_feature.go`, extend the const block at the top of the file:

```go
const readySentinel = "PIPELINE_READY"

const specReadySentinel = "SPEC_READY:"

// groupDoneSentinel/planCompleteSentinel are emitted by a grouped execute
// session (see executePlanGrouped) to tell the daemon whether more of the
// plan remains for a later fresh session or the whole plan is now done.
// Both only ever appear when cfg.StepsPerSession > 0 — the ungrouped execute
// path never requires or checks either sentinel.
const groupDoneSentinel = "GROUP_DONE"
const planCompleteSentinel = "PLAN_COMPLETE"
```

- [ ] **Step 2: Add the two keys to `promptData()`**

In `prompts.go`, extend the map:

```go
func promptData() map[string]any {
	return map[string]any{
		"ConfidenceSentinel":   confidenceSentinel,
		"SpecReadySentinel":    specReadySentinel,
		"ReadySentinel":        readySentinel,
		"AlreadyDoneSentinel":  alreadyDoneSentinel,
		"DoneConfirmSentinel":  doneConfirmSentinel,
		"UATBeginSentinel":     uatBeginSentinel,
		"UATEndSentinel":       uatEndSentinel,
		"UATMarker":            uatMarker,
		"BotMarker":            botMarker,
		"GroupDoneSentinel":    groupDoneSentinel,
		"PlanCompleteSentinel": planCompleteSentinel,
	}
}
```

- [ ] **Step 3: Run the full test suite to confirm nothing broke**

Run: `go build ./... && go test ./... -run 'TestGolden|TestEveryPromptFileOnDiskIsParsed' -v`
Expected: PASS — adding unused map keys and unused consts changes no existing template output (`missingkey=error` only fires on a *missing* key, never on an unused extra one).

- [ ] **Step 4: Commit**

```bash
git add pipeline_feature.go prompts.go
git commit -m "feat: add GROUP_DONE/PLAN_COMPLETE sentinels for grouped execute sessions"
```

---

### Task 3: Prompt templates and builder functions

**Files:**
- Modify: `ai/prompts/execute.md.tmpl`
- Create: `ai/prompts/execute-continue.md.tmpl`
- Modify: `pipeline_feature.go` (bottom, alongside `executePrompt`)
- Test: `prompts_golden_test.go`

**Interfaces:**
- Consumes: `groupDoneSentinel`, `planCompleteSentinel` (Task 2), `promptData()`, `mustRender` (`prompts.go`).
- Produces: `executePrompt(planPath string) string` (unchanged signature, unchanged output for `StepsPerSession == 0`), `executeGroupPrompt(planPath string, stepsPerSession int) string`, `executeContinuePrompt() string` — all consumed by `executePlan`/`executePlanGrouped`/`runGroupWithRetry` in Task 4.

- [ ] **Step 1: Write the failing golden tests**

Add to `prompts_golden_test.go`, replacing nothing (keep the existing `TestGoldenExecutePrompt` as-is — it is the regression guard for the `StepsPerSession == 0` path) and appending these new tests after it:

```go
func TestGoldenExecuteGroupPrompt(t *testing.T) {
	want := `/superpowers:executing-plans Execute the plan at docs/plan.md.
Use the execution style the plan recommends (subagent-driven or inline).
Follow TDD per the plan. Commit as you complete tasks.
HEADLESS: do not ask questions; make reasonable calls and note them in commit messages.

This plan is being executed across multiple bounded sessions to avoid running
out of context. Implement only the next 5 steps of the
plan, even if more remain — do not go further. Before starting, check git log
and the plan file for what earlier sessions already completed, and continue
from there.
If those steps finish the entire plan, commit and end your final reply with
PLAN_COMPLETE. Otherwise, once you've implemented up to
5 steps, commit your progress and end your final reply
with GROUP_DONE.`
	check(t, "executeGroupPrompt", executeGroupPrompt("docs/plan.md", 5), want)
}

func TestGoldenExecuteContinuePrompt(t *testing.T) {
	want := `Continue. You were implementing a bounded group of plan steps and either did
not finish or did not end with the expected sentinel. Check git status and
the plan file for what remains in this group, finish it, commit, and end
your reply with PLAN_COMPLETE if the whole plan is now done, or
GROUP_DONE if more remains for a later session.`
	check(t, "executeContinuePrompt", executeContinuePrompt(), want)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestGoldenExecuteGroupPrompt|TestGoldenExecuteContinuePrompt' -v`
Expected: FAIL to compile — `executeGroupPrompt`/`executeContinuePrompt` don't exist yet.

- [ ] **Step 3: Update `execute.md.tmpl` with the conditional block**

Replace the full contents of `ai/prompts/execute.md.tmpl` with:

```
/superpowers:executing-plans Execute the plan at {{.PlanPath}}.
Use the execution style the plan recommends (subagent-driven or inline).
Follow TDD per the plan. Commit as you complete tasks.
HEADLESS: do not ask questions; make reasonable calls and note them in commit messages.
{{- if gt .StepsPerSession 0}}

This plan is being executed across multiple bounded sessions to avoid running
out of context. Implement only the next {{.StepsPerSession}} steps of the
plan, even if more remain — do not go further. Before starting, check git log
and the plan file for what earlier sessions already completed, and continue
from there.
If those steps finish the entire plan, commit and end your final reply with
{{.PlanCompleteSentinel}}. Otherwise, once you've implemented up to
{{.StepsPerSession}} steps, commit your progress and end your final reply
with {{.GroupDoneSentinel}}.
{{- end}}
```

The `{{- if ...}}` trim marker is load-bearing: it eats the newline that would otherwise separate line 4 from the `{{if}}` tag, so when `StepsPerSession == 0` the rendered output is byte-identical to the pre-existing template (verified by the untouched `TestGoldenExecutePrompt`). When the condition is true, that same trim plus the blank line at the top of the block produces exactly one blank line of separation before "This plan is being executed...".

- [ ] **Step 4: Create `execute-continue.md.tmpl`**

```
Continue. You were implementing a bounded group of plan steps and either did
not finish or did not end with the expected sentinel. Check git status and
the plan file for what remains in this group, finish it, commit, and end
your reply with {{.PlanCompleteSentinel}} if the whole plan is now done, or
{{.GroupDoneSentinel}} if more remains for a later session.
```

- [ ] **Step 5: Add the builder functions**

In `pipeline_feature.go`, replace the existing `executePrompt` function at the bottom of the file with:

```go
func executePrompt(planPath string) string {
	d := promptData()
	d["PlanPath"] = planPath
	d["StepsPerSession"] = 0
	return mustRender("execute.md.tmpl", d)
}

// executeGroupPrompt renders the same execute.md.tmpl template as
// executePrompt, but with StepsPerSession set so the conditional grouped-
// execution block (and its sentinel instructions) is included.
func executeGroupPrompt(planPath string, stepsPerSession int) string {
	d := promptData()
	d["PlanPath"] = planPath
	d["StepsPerSession"] = stepsPerSession
	return mustRender("execute.md.tmpl", d)
}

func executeContinuePrompt() string {
	return mustRender("execute-continue.md.tmpl", promptData())
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./... -run 'TestGolden' -v`
Expected: PASS for every golden test, including the untouched `TestGoldenExecutePrompt` (regression guard) and the two new tests.

- [ ] **Step 7: Run the full prompt-rendering test suite**

Run: `go test ./... -run 'TestEveryPromptFileOnDiskIsParsed|TestGolden' -v`
Expected: PASS — `execute-continue.md.tmpl` is picked up automatically by the `ai/prompts/*.md.tmpl` embed glob, so no wiring is needed beyond creating the file.

- [ ] **Step 8: Commit**

```bash
git add ai/prompts/execute.md.tmpl ai/prompts/execute-continue.md.tmpl pipeline_feature.go prompts_golden_test.go
git commit -m "feat: add grouped-execution prompt template and builders"
```

---

### Task 4: `executePlan` dispatch, `executePlanGrouped`, `runGroupWithRetry`

**Files:**
- Modify: `pipeline_feature.go` (the `executePlan` function, plus new functions)
- Test: `pipeline_feature_test.go`

**Interfaces:**
- Consumes: `Config.StepsPerSession` (Task 1), `executeGroupPrompt`/`executeContinuePrompt` (Task 3), `groupDoneSentinel`/`planCompleteSentinel` (Task 2), `Claude.Call`/`Claude.RecordSession`/`ClaudeCall{Dir, Label, Prompt, Resume, Model, SkipPermissions, DisallowedTools}` (`claude.go`, unchanged).
- Produces: `executePlan(ctx, c, cfg, wtPath, planPath) error` (unchanged signature, now dispatches), `executePlanGrouped(ctx context.Context, c *Claude, cfg *Config, wtPath, planPath string) error`, `runGroupWithRetry(ctx context.Context, c *Claude, cfg *Config, wtPath, label, prompt string) (string, error)`. Consumed by `runPlanThenExecute` (unchanged call site).

- [ ] **Step 1: Write the failing tests**

Add to `pipeline_feature_test.go` (after the existing `TestFeaturePipelineExecuteUsesExecuteConfig`, before `TestReadPersona`):

```go
// TestExecutePlanUngroupedUnchanged locks in that StepsPerSession == 0 keeps
// today's exact behavior: one call, label "execute", no sentinel required.
func TestExecutePlanUngroupedUnchanged(t *testing.T) {
	wt := t.TempDir()
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		return claudeJSON("Executed, no sentinel here.", "exec-1"), "", nil
	}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := executePlan(context.Background(), c, cfg, wt, "docs/plan.md"); err != nil {
		t.Fatalf("ungrouped execute must succeed without any sentinel, got %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(f.calls))
	}
	if got := argAfter(f.calls[0].args, "--resume"); got != "" {
		t.Error("ungrouped execute must be a fresh session")
	}
}

// TestExecutePlanGroupedRunsFreshSessionPerGroup verifies executePlanGrouped
// spawns one brand-new session per group (never resumed) and stops as soon as
// PLAN_COMPLETE arrives.
func TestExecutePlanGroupedRunsFreshSessionPerGroup(t *testing.T) {
	wt := t.TempDir()
	var prompts []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1, 2:
			return claudeJSON("Group done.\nGROUP_DONE", fmt.Sprintf("sess-%d", len(prompts))), "", nil
		case 3:
			return claudeJSON("All done.\nPLAN_COMPLETE", "sess-3"), "", nil
		}
		t.Fatalf("unexpected call %d", len(prompts))
		return "", "", nil
	}
	c := &Claude{runner: f}
	cfg := &Config{StepsPerSession: 5, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := executePlanGrouped(context.Background(), c, cfg, wt, "docs/plan.md"); err != nil {
		t.Fatalf("expected success once PLAN_COMPLETE arrives, got %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("got %d calls, want 3 (one fresh session per group)", len(f.calls))
	}
	for i, call := range f.calls {
		if got := argAfter(call.args, "--resume"); got != "" {
			t.Errorf("call %d: resume = %q, want fresh session (no --resume)", i, got)
		}
	}
	if !strings.Contains(prompts[0], "next 5 steps") {
		t.Errorf("group prompt should mention the steps-per-session cap: %s", prompts[0])
	}
}

// TestExecutePlanGroupedSafetyCap verifies a session that never signals
// PLAN_COMPLETE fails the pipeline after exactly maxExecuteGroups sessions,
// rather than looping forever.
func TestExecutePlanGroupedSafetyCap(t *testing.T) {
	wt := t.TempDir()
	calls := 0
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		calls++
		return claudeJSON("Still going.\nGROUP_DONE", fmt.Sprintf("sess-%d", calls)), "", nil
	}
	c := &Claude{runner: f}
	cfg := &Config{StepsPerSession: 5, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	err := executePlanGrouped(context.Background(), c, cfg, wt, "docs/plan.md")
	if err == nil {
		t.Fatal("want an error when PLAN_COMPLETE never arrives")
	}
	if calls != maxExecuteGroups {
		t.Errorf("got %d calls, want exactly maxExecuteGroups (%d)", calls, maxExecuteGroups)
	}
}

// TestRunGroupWithRetryRetriesAmbiguousResult verifies a response with
// neither sentinel triggers a --resume retry with the continuation prompt on
// the SAME session, and that a later retry succeeding returns its result.
func TestRunGroupWithRetryRetriesAmbiguousResult(t *testing.T) {
	wt := t.TempDir()
	var prompts []string
	var resumes []string
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		resumes = append(resumes, argAfter(c.args, "--resume"))
		if len(prompts) == 1 {
			return claudeJSON("No sentinel here, ran out of turns.", "sess-1"), "", nil
		}
		return claudeJSON("Finishing up.\nGROUP_DONE", "sess-1"), "", nil
	}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	result, err := runGroupWithRetry(context.Background(), c, cfg, wt, "execute-group-1", executeGroupPrompt("docs/plan.md", 5))
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if !strings.Contains(result, groupDoneSentinel) {
		t.Errorf("result = %q, want it to contain GROUP_DONE", result)
	}
	if len(prompts) != 2 {
		t.Fatalf("got %d calls, want 2 (initial + one retry)", len(prompts))
	}
	if resumes[0] != "" {
		t.Errorf("initial call resume = %q, want fresh session", resumes[0])
	}
	if resumes[1] != "sess-1" {
		t.Errorf("retry resume = %q, want sess-1 (same session)", resumes[1])
	}
	if prompts[1] != executeContinuePrompt() {
		t.Errorf("retry prompt = %q, want the continuation prompt", prompts[1])
	}
}

// TestRunGroupWithRetryExhaustsRetries verifies that never signaling a
// sentinel fails after maxGroupRetries retries rather than looping forever.
func TestRunGroupWithRetryExhaustsRetries(t *testing.T) {
	wt := t.TempDir()
	calls := 0
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		calls++
		return claudeJSON("Still no sentinel.", "sess-1"), "", nil
	}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	_, err := runGroupWithRetry(context.Background(), c, cfg, wt, "execute-group-1", executeGroupPrompt("docs/plan.md", 5))
	if err == nil {
		t.Fatal("want an error once retries are exhausted")
	}
	if calls != maxGroupRetries+1 {
		t.Errorf("got %d calls, want %d (initial + maxGroupRetries retries)", calls, maxGroupRetries+1)
	}
}

// TestRunGroupWithRetryFailsImmediatelyWithNoSessionID verifies an error
// result carrying no session id (nothing to resume) fails the pipeline right
// away instead of attempting a retry.
func TestRunGroupWithRetryFailsImmediatelyWithNoSessionID(t *testing.T) {
	wt := t.TempDir()
	calls := 0
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		calls++
		return "", "boom", fmt.Errorf("exit 1")
	}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	_, err := runGroupWithRetry(context.Background(), c, cfg, wt, "execute-group-1", executeGroupPrompt("docs/plan.md", 5))
	if err == nil {
		t.Fatal("want an error")
	}
	if calls != 1 {
		t.Errorf("got %d calls, want 1 (no retry when there is no session to resume)", calls)
	}
}

// TestExecutePlanGroupedRecordsSessionPerGroup verifies every group call
// (including retries) records its session, matching the existing
// overwrite-with-latest behavior used for the dashboard's current-session
// display.
func TestExecutePlanGroupedRecordsSessionPerGroup(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) {
		return claudeJSON("Done.\nPLAN_COMPLETE", "final-sess"), "", nil
	}
	c := &Claude{runner: f, logDir: logDir}
	cfg := &Config{StepsPerSession: 5, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := executePlanGrouped(context.Background(), c, cfg, wt, "docs/plan.md"); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "final-sess" || si.Kind != "feature" {
		t.Errorf("session = %+v, want final-sess/feature", si)
	}
}
```

Add `"fmt"` to the import block of `pipeline_feature_test.go` if not already present (it already is, per the existing `TestFeaturePipelineContinuesWhenUATFails` use of `fmt.Errorf`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestExecutePlanUngroupedUnchanged|TestExecutePlanGrouped|TestRunGroupWithRetry' -v`
Expected: FAIL to compile — `executePlanGrouped`/`runGroupWithRetry`/`maxExecuteGroups`/`maxGroupRetries` don't exist yet.

- [ ] **Step 3: Rewrite `executePlan` and add the grouped functions**

In `pipeline_feature.go`, replace the existing `executePlan` function with:

```go
// maxExecuteGroups bounds executePlanGrouped: no deterministic total-step
// count exists (the plan file is not machine-parseable), so this is a safety
// cap against a session that never signals planCompleteSentinel.
const maxExecuteGroups = 20

// maxGroupRetries bounds how many times a single group's session is retried
// (via --resume with a continuation prompt) before the pipeline fails.
const maxGroupRetries = 2

func executePlan(ctx context.Context, c *Claude, cfg *Config, wtPath, planPath string) error {
	if cfg.StepsPerSession <= 0 {
		// Unchanged: one fresh session implements the whole plan, no
		// sentinel required.
		res, err := c.Call(ctx, ClaudeCall{
			Dir: wtPath, Label: "execute", Prompt: executePrompt(planPath),
			Model:           cfg.Models.executeConfig(),
			SkipPermissions: true,
			DisallowedTools: []string{"AskUserQuestion"},
		})
		if res != nil {
			c.RecordSession(res.SessionID, "feature")
		}
		return err
	}
	return executePlanGrouped(ctx, c, cfg, wtPath, planPath)
}

// executePlanGrouped implements the plan across several bounded, brand-new
// Claude sessions (decision: never --resume between groups). Each group's
// session reads the plan file and git log itself to figure out where to pick
// up — the daemon tracks no per-group progress. It loops until a group
// signals planCompleteSentinel or maxExecuteGroups is reached.
func executePlanGrouped(ctx context.Context, c *Claude, cfg *Config, wtPath, planPath string) error {
	for i := 1; i <= maxExecuteGroups; i++ {
		label := fmt.Sprintf("execute-group-%d", i)
		result, err := runGroupWithRetry(ctx, c, cfg, wtPath, label, executeGroupPrompt(planPath, cfg.StepsPerSession))
		if err != nil {
			return err
		}
		if strings.Contains(result, planCompleteSentinel) {
			return nil
		}
		// strings.Contains(result, groupDoneSentinel): move on to a fresh
		// session for the next group.
	}
	return fmt.Errorf("feature pipeline: execute did not signal %s within %d grouped sessions", planCompleteSentinel, maxExecuteGroups)
}

// runGroupWithRetry runs one group's initial call as a fresh session, then —
// only when the call errors with a usable session id, or succeeds without
// either sentinel — retries up to maxGroupRetries times via --resume on that
// same session with the continuation prompt. An error with no session id
// (nothing to resume) fails immediately, matching how every other stage
// already propagates a hard Claude failure.
func runGroupWithRetry(ctx context.Context, c *Claude, cfg *Config, wtPath, label, prompt string) (string, error) {
	resume := ""
	for attempt := 0; attempt <= maxGroupRetries; attempt++ {
		callLabel := label
		if attempt > 0 {
			callLabel = fmt.Sprintf("%s-retry-%d", label, attempt)
			prompt = executeContinuePrompt()
		}
		res, err := c.Call(ctx, ClaudeCall{
			Dir: wtPath, Label: callLabel, Prompt: prompt, Resume: resume,
			Model:           cfg.Models.executeConfig(),
			SkipPermissions: true,
			DisallowedTools: []string{"AskUserQuestion"},
		})
		if res != nil {
			c.RecordSession(res.SessionID, "feature")
		}
		if err != nil {
			if res == nil || res.SessionID == "" {
				return "", err // nothing to resume; fail the pipeline now
			}
			resume = res.SessionID
			continue // retry via continuation prompt
		}
		if strings.Contains(res.Result, planCompleteSentinel) || strings.Contains(res.Result, groupDoneSentinel) {
			return res.Result, nil
		}
		resume = res.SessionID // neither sentinel: ambiguous, retry with "continue"
	}
	return "", fmt.Errorf("feature pipeline: %s did not signal completion after %d retries", label, maxGroupRetries)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestExecutePlanUngroupedUnchanged|TestExecutePlanGrouped|TestRunGroupWithRetry' -v`
Expected: PASS

- [ ] **Step 5: Run the full test suite**

Run: `go build ./... && go test ./...`
Expected: PASS — including every pre-existing `pipeline_feature_test.go` test (they all use `StepsPerSession == 0` implicitly via `featureConfig()`, so they exercise the unchanged path).

- [ ] **Step 6: Commit**

```bash
git add pipeline_feature.go pipeline_feature_test.go
git commit -m "feat: run the feature pipeline's execute step across bounded, fresh sessions"
```

---

### Task 5: Config docs and example

**Files:**
- Modify: `loope.json.example`
- Modify: `docs/configuration.md`

**Interfaces:**
- Consumes: `Config.StepsPerSession` (Task 1). No code interfaces produced — this task is documentation only.

- [ ] **Step 1: Add a commented `stepsPerSession` entry to `loope.json.example`**

`loope.json.example` is parsed as strict JSON by nothing in the test suite (it's a human-facing example, not loaded by `LoadConfig` in any test), but it must stay valid JSON. Since JSON has no comments, document it as a present-but-clearly-optional field with `0` as the value (matching the "unset/0 means unbounded" default), plus an inline note in `docs/configuration.md` (Step 2) rather than an in-file comment. Add it as a sibling of `maxQARounds`:

```json
{
  "repoPath": "/Users/you/src/your-repo",
  "repoSlug": "your-org/your-repo",
  "eligibleLabel": "ai-agent",
  "addr": "localhost:8080",
  "pollIntervalSec": 60,
  "ticketsPerCycle": 3,
  "workDir": "~/.loope/worktrees",
  "personaPath": "~/.loope/persona.md",
  "claudeConfigDir": "~/.claude-personal",
  "maxQARounds": 20,
  "confidenceThreshold": 70,
  "stepsPerSession": 0,
  "stateLabels": {"wip": "ai-wip", "done": "ai-done", "rework": "ai-rework", "needsInfo": "ai-needs-info", "stopped": "ai-stopped"},
  "githubRetry": {"maxAttempts": 0, "baseDelaySec": 2, "maxDelaySec": 60},
  "models": {
    "architect": {"model": "opus", "effort": "high", "maxBudgetUSD": 15, "maxTurns": 100},
    "answerer":  {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 2, "maxTurns": 5},
    "triage":    {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 1, "maxTurns": 5},
    "execute":   {"model": "opus", "effort": "high", "maxBudgetUSD": 40, "maxTurns": 400},
    "uat":       {"model": "sonnet", "effort": "medium", "maxBudgetUSD": 2, "maxTurns": 30}
  }
}
```

- [ ] **Step 2: Document the field in `docs/configuration.md`**

Add a row to the settings table (after the `confidenceThreshold` row):

```markdown
| `stepsPerSession`     | no       | `0` (unbounded)  | Caps how many plan steps one feature-pipeline execute session attempts before handing off to a fresh session ([details](#stepspersession)) |
```

Add a new section after the "## Confidence gate" section (before "## `githubRetry`"):

```markdown
## `stepsPerSession`

The feature pipeline's execute step normally implements the entire
implementation plan in one Claude session. A large plan can exhaust that
session's usable context before the plan is finished. Set `stepsPerSession`
to cap how much one session attempts:

```json
"stepsPerSession": 5
```

With this set, the execute step runs across multiple **brand-new** Claude
sessions instead of one — each session is told to implement only the next N
steps, then stop. Nothing about the plan file's structure is parsed by the
daemon: each fresh session reads the plan and the branch's git log itself to
figure out where the previous session left off, the same "recover and
continue from existing state" approach the loop already uses everywhere else.

A session ends a group by printing `GROUP_DONE` (more of the plan remains —
the daemon starts a fresh session for the next group) or `PLAN_COMPLETE` (the
whole plan is now implemented). If a session's call fails outright, or
succeeds without printing either sentinel, the loop retries that same session
(via `--resume`, not a fresh session) with a short "continue" prompt, up to a
few attempts, before giving up and parking the issue.

Leave `stepsPerSession` unset (or `0`, the default) to keep the original
behavior: one fresh execute session implements the whole plan, with no
sentinel requirement. This only affects the feature pipeline's execute step —
brainstorm, plan, and the bug pipeline are unaffected.
```

- [ ] **Step 3: Verify the example file still parses**

Run: `go run . -config loope.json.example 2>&1 | head -5` is not viable (it requires a real repo/workdir), so instead verify JSON validity directly:

Run: `python3 -c "import json; json.load(open('loope.json.example'))" && echo OK`
Expected: `OK`

- [ ] **Step 4: Commit**

```bash
git add loope.json.example docs/configuration.md
git commit -m "docs: document stepsPerSession"
```

---

## Final verification

- [ ] Run the full suite once more end-to-end: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, zero regressions, covering every task's tests plus the pre-existing suite.
