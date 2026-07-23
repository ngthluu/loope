# Confidence Gate for the Bug-Fix Route Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the bug pipeline escalate under-specified bug reports to `ai-needs-info` using the same confidence threshold, sentinel, parser, and error type the feature pipeline already uses.

**Architecture:** Two source changes plus docs. `ai/prompts/debug.md.tmpl` gains a `{{if gt .Threshold 0}}` block (rendering away entirely at threshold `0`, so the prompt stays byte-identical to today's), and `bugPrompt` changes signature to `bugPrompt(issue string, threshold int)` to feed it. `RunBugPipeline` gains the same four-line gate `RunFeaturePipeline` has, placed after the session-record/error checks and *before* the already-done check. The orchestrator needs no change: `loop.go` already matches `*lowConfidenceError` from any pipeline and routes it to `finishNeedsInfo`.

**Tech Stack:** Go (standard library only), `text/template` with `missingkey=error`, `go test` with the repo's `fakeRunner` test doubles.

## Global Constraints

- One setting governs both routes: `cfg.ConfidenceThreshold`. Do **not** add a bug-specific threshold field, config key, or constant.
- Reuse the existing mechanism only: `confidenceSentinel`, `parseConfidence`, `stripConfidenceLine`, `lowConfidenceError` (all in `confidence.go`). No parallel sentinel, parser, or error type.
- Sentinels are never written as literal text in a `.tmpl` file. Always inject via `{{.ConfidenceSentinel}}` from `promptData()`.
- `confidenceThreshold: 0` must disable the gate on both routes, and at `0` the rendered debug prompt must be **byte-identical** to today's.
- Fail open: an absent or unparseable score proceeds to the normal path.
- Confidence outranks already-done: the gate runs before the `parseAlreadyDone` check.
- Do not change the feature route's behavior, `loop.go`, `finishNeedsInfo`, or the `needs-info` comment template.
- Existing bug-pipeline tests build `&Config{…}` with no threshold and must pass **unmodified** — that is the default-off regression check.
- Every task ends green: `go build ./... && go test ./...` from the repo root.

---

### Task 1: Threshold block in the debug prompt

`bugPrompt` currently takes only the issue text. Give it a threshold parameter (mirroring `brainstormPrompt(issue string, threshold int)` in `pipeline_feature.go:258`) and add the conditional block to the template. The block is placed immediately after the `/superpowers:systematic-debugging {{.Issue}}` line — the same position the block occupies in `brainstorm.md.tmpl`.

Unlike the feature route, the bug prompt permits read-only investigation *before* scoring: bug reports are terse by nature, and confidence in a fix is a function of the code, not the prose.

**Files:**
- Modify: `ai/prompts/debug.md.tmpl` (whole file)
- Modify: `pipeline_bug.go:28-32` (`bugPrompt`) and `pipeline_bug.go:9` (its one call site)
- Test: `prompts_golden_test.go:112-123` (rename + add a second golden)

**Interfaces:**
- Consumes: `promptData()` from `prompts.go:54`, `mustRender(name string, data map[string]any) string` from `prompts.go:42`.
- Produces: `bugPrompt(issue string, threshold int) string`. Task 2 calls it as `bugPrompt(issueContent, cfg.ConfidenceThreshold)`.

- [ ] **Step 1: Write the failing goldens**

Replace the existing `TestGoldenBugPrompt` in `prompts_golden_test.go` (currently at line 112) with these two tests. The `threshold=0` golden is character-for-character the old golden — that is what pins the no-regression claim.

```go
func TestGoldenBugPromptWithThreshold(t *testing.T) {
	want := `/superpowers:systematic-debugging ISSUE BODY

You may read the codebase first to investigate — but do NOT write code, tests,
or commits yet. Once you understand the failure, assess how confidently this bug
can be fixed as reported and print CONFIDENCE: <0-100> as the FIRST line of your reply.
If that score is below 70, the report is too vague or ambiguous to fix
responsibly: change no file. Instead, list what is missing and the specific
questions the author must answer, then stop.

Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
PIPELINE_ALREADY_DONE: <one-sentence reason> on its own line and stop.`
	check(t, "bugPrompt(threshold=70)", bugPrompt("ISSUE BODY", 70), want)
}

func TestGoldenBugPromptWithoutThreshold(t *testing.T) {
	want := `/superpowers:systematic-debugging ISSUE BODY

Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
PIPELINE_ALREADY_DONE: <one-sentence reason> on its own line and stop.`
	check(t, "bugPrompt(threshold=0)", bugPrompt("ISSUE BODY", 0), want)
}
```

Note the em dash in "investigate — but"; the repo's prompts already use em dashes, and the golden must match the template byte for byte.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run TestGoldenBugPrompt`
Expected: FAIL — compile error, `not enough arguments in call to bugPrompt` (the function still takes one argument).

- [ ] **Step 3: Add the threshold block to the template**

Rewrite `ai/prompts/debug.md.tmpl` to exactly this. The blank-line placement matters: with `Threshold` at `0`, template execution drops everything between `{{if}}` and `{{end}}` including the newline that follows `{{if …}}`, leaving `line 1` + blank line + `Reproduce…` — byte-identical to today.

```
/superpowers:systematic-debugging {{.Issue}}
{{if gt .Threshold 0}}
You may read the codebase first to investigate — but do NOT write code, tests,
or commits yet. Once you understand the failure, assess how confidently this bug
can be fixed as reported and print {{.ConfidenceSentinel}} <0-100> as the FIRST line of your reply.
If that score is below {{.Threshold}}, the report is too vague or ambiguous to fix
responsibly: change no file. Instead, list what is missing and the specific
questions the author must answer, then stop.
{{end}}
Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
{{.AlreadyDoneSentinel}} <one-sentence reason> on its own line and stop.
```

- [ ] **Step 4: Change the `bugPrompt` signature and its call site**

In `pipeline_bug.go`, replace the function:

```go
func bugPrompt(issue string, threshold int) string {
	d := promptData()
	d["Issue"] = issue
	d["Threshold"] = threshold
	return mustRender("debug.md.tmpl", d)
}
```

And update the one call site inside `RunBugPipeline`:

```go
		Dir: wtPath, Label: "debug", Prompt: bugPrompt(issueContent, cfg.ConfidenceThreshold),
```

`missingkey=error` means forgetting `d["Threshold"]` is a render panic, not a silent `<no value>` — the goldens catch it either way.

- [ ] **Step 5: Run the goldens to verify they pass**

Run: `go test ./... -run TestGoldenBugPrompt -v`
Expected: PASS for both `TestGoldenBugPromptWithThreshold` and `TestGoldenBugPromptWithoutThreshold`.

If the `threshold=70` golden fails on whitespace, fix the **golden** to match the template only after confirming the template's own line breaks are what you intended; if the `threshold=0` golden fails, the template is wrong — that golden is the contract.

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: PASS. In particular `TestBugPipelineSingleDebugSession` and `TestBugPipelinePromptMentionsAlreadyDoneSentinel` (which build `&Config{}` with no threshold) must still pass untouched.

- [ ] **Step 7: Commit**

```bash
git add ai/prompts/debug.md.tmpl pipeline_bug.go prompts_golden_test.go
git commit -m "feat: add a confidence-threshold block to the debug prompt"
```

---

### Task 2: The gate in `RunBugPipeline`

Add the score check to the pipeline. Placement is the whole point: **after** the record-session and error checks (so `loop -rework` still works after an errored call), and **before** the `parseAlreadyDone` check (so a session that is not confident enough to fix the bug cannot also close the issue as already implemented).

**Files:**
- Modify: `pipeline_bug.go:19-25`
- Test: `pipeline_bug_test.go` (append new cases)

**Interfaces:**
- Consumes: `bugPrompt(issue string, threshold int) string` from Task 1; `parseConfidence(s string) (int, bool)`, `stripConfidenceLine(s string) string`, `lowConfidenceError{score int; feedback string}`, `confidenceSentinel` — all from `confidence.go`; `Config.ConfidenceThreshold int` from `config.go`.
- Produces: `RunBugPipeline` returning `*lowConfidenceError` on a low score. Task 3 relies on that reaching `loop.go`'s existing `errors.As(err, &lc)` branch.

- [ ] **Step 1: Write the failing tests**

Append to `pipeline_bug_test.go`. These mirror `TestFeaturePipelineLowConfidenceEscalates` / `…HighConfidenceProceeds` in `pipeline_feature_test.go:370-400`.

```go
func TestBugPipelineLowConfidenceEscalates(t *testing.T) {
	count := 0
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		count++
		return claudeJSON("CONFIDENCE: 40\nNo stack trace and no repro steps.\nWhich command triggers the crash?", "s1"), "", nil
	}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "crashes sometimes on startup")
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
	if lc.score != 40 {
		t.Errorf("score = %d, want 40", lc.score)
	}
	if !strings.Contains(lc.feedback, "repro steps") || strings.Contains(lc.feedback, confidenceSentinel) {
		t.Errorf("feedback should carry the reasons without the CONFIDENCE line: %q", lc.feedback)
	}
	if count != 1 {
		t.Errorf("low confidence must stop after the debug turn, got %d calls", count)
	}
}

func TestBugPipelineHighConfidenceProceeds(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("CONFIDENCE: 85\nFixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatalf("a score at or above the threshold must proceed: %v", err)
	}
}

// A score exactly at the threshold is not below it, so it proceeds.
func TestBugPipelineConfidenceAtThresholdProceeds(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("CONFIDENCE: 70\nFixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatalf("score == threshold must proceed: %v", err)
	}
}

// confidenceThreshold: 0 disables the gate entirely — even an explicit low score
// in the output is ignored.
func TestBugPipelineZeroThresholdIgnoresScore(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("CONFIDENCE: 5\nFixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatalf("threshold 0 disables the gate: %v", err)
	}
}

// Fail open: a session that forgot the sentinel but fixed the bug still ships.
func TestBugPipelineMissingSentinelFailsOpen(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("Fixed and committed.", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	if err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE"); err != nil {
		t.Fatalf("an absent score must fail open, got %v", err)
	}
}

// Confidence outranks already-done: a session too unsure to fix the bug must not
// be able to close the issue as already implemented either.
func TestBugPipelineLowConfidenceBeatsAlreadyDone(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		"CONFIDENCE: 20\nI cannot tell what behavior is wrong.\nPIPELINE_ALREADY_DONE: looks fine to me", "s1")}}}
	c := &Claude{runner: f}
	cfg := &Config{ConfidenceThreshold: 70, Models: Models{Architect: ModelConfig{Model: "opus"}}}
	err := RunBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE")
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %T (%v)", err, err)
	}
	var done *alreadyDoneError
	if errors.As(err, &done) {
		t.Error("a low-confidence session must not close the issue as already done")
	}
}
```

`errors`, `strings`, `context`, `fmt` and `testing` are already imported at the top of `pipeline_bug_test.go` — no import changes needed.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestBugPipeline(LowConfidence|HighConfidence|ConfidenceAtThreshold|ZeroThreshold|MissingSentinel)' -v`
Expected: FAIL. `TestBugPipelineLowConfidenceEscalates` fails with `want *lowConfidenceError, got <nil>`; `TestBugPipelineLowConfidenceBeatsAlreadyDone` fails with `got *main.alreadyDoneError`. The other three already pass (nothing gates yet) — that is fine.

- [ ] **Step 3: Add the gate**

In `pipeline_bug.go`, insert between the `if err != nil` block and the `parseAlreadyDone` check so the body reads:

```go
	if err != nil {
		return err
	}
	// Confidence gate, shared with the feature route: same threshold, sentinel,
	// parser and terminal outcome. It runs before the already-done check on
	// purpose — a session too unsure to fix the bug must not get to close the
	// issue as already implemented instead. A threshold <= 0 disables it, and an
	// unparseable score fails open so a session that forgot the sentinel but
	// fixed the bug still ships.
	if cfg.ConfidenceThreshold > 0 {
		if score, ok := parseConfidence(res.Result); ok && score < cfg.ConfidenceThreshold {
			return &lowConfidenceError{score: score, feedback: stripConfidenceLine(res.Result)}
		}
	}
	if reason, ok := parseAlreadyDone(res.Result); ok {
		return &alreadyDoneError{reason: reason}
	}
	return nil
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run TestBugPipeline -v`
Expected: PASS for all `TestBugPipeline*` cases, including the pre-existing `TestBugPipelineReturnsAlreadyDone`, `TestBugPipelineRecordsSession`, and `TestBugPipelineRecordsSessionOnError` — which must not have been edited.

- [ ] **Step 5: Run the full suite**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pipeline_bug.go pipeline_bug_test.go
git commit -m "feat: gate the bug route on the architect's confidence score"
```

---

### Task 3: End-to-end orchestrator case and documentation

Prove the wiring end to end — that a `kind == "bug"` issue with a low-confidence debug session reaches `finishNeedsInfo` without any change to `loop.go` — and update the README, which currently describes the gate as feature-only.

**Files:**
- Test: `loop_test.go` (append one case, after `TestProcessOnceLowConfidenceEscalatesToNeedsInfo` at line 91-138)
- Modify: `README.md:210` (config table row), `README.md:228-238` ("Confidence gate" section), `README.md:42-44` (the `bug:` bullet in the loop overview)

**Interfaces:**
- Consumes: `RunBugPipeline` returning `*lowConfidenceError` (Task 2); the existing `fakeEnv` helpers `newFakeEnv(t)`, `env.orchestrator()`, `env.callsMatching(name, substr)`, `runCycle(o)`, and `claudeJSON(result, sessionID)` from `loop_test.go` / `helpers_test.go`.
- Produces: nothing consumed by later tasks — this is the last task.

- [ ] **Step 1: Write the failing end-to-end test**

Append to `loop_test.go`. The handler makes triage classify the issue as a bug, then makes the debug session return a low score; everything else falls through to the fake env's base handler.

```go
// The gate is route-agnostic: a bug whose debug session scores low must reach the
// same ai-needs-info outcome as an under-specified feature, with no PR and the
// issue left open.
func TestProcessOnceBugLowConfidenceEscalatesToNeedsInfo(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" && strings.Contains(c.stdin, "triage agent") {
			return claudeJSON(`{"issueNumber": 7, "kind": "bug", "reason": "small defect"}`, "t1"), "", nil
		}
		if c.name == "claude" && strings.Contains(c.stdin, "systematic-debugging") {
			return claudeJSON("CONFIDENCE: 25\nNo stack trace — which command reproduces the crash?", "dbg-1"), "", nil
		}
		return base(c)
	}
	o := env.orchestrator()
	o.cfg.ConfidenceThreshold = 70
	if err := runCycle(o); err != nil {
		t.Fatalf("needs-info is a clean outcome, want nil error, got %v", err)
	}
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-needs-info") {
		t.Errorf("want single ai-wip->ai-needs-info swap, got: %v", swap)
	}
	var commented bool
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "stack trace") {
			commented = true
		}
	}
	if !commented {
		t.Error("needs-info path should comment the debug session's questions on the issue")
	}
	if len(env.callsMatching("gh", "pr create")) != 0 {
		t.Error("needs-info must not create a PR")
	}
	if len(env.callsMatching("gh", "issue close")) != 0 {
		t.Error("needs-info must not close the issue")
	}
}
```

- [ ] **Step 2: Run it to verify it passes**

Run: `go test ./... -run TestProcessOnceBugLowConfidenceEscalatesToNeedsInfo -v`
Expected: PASS — Task 2 supplied the behavior, so this test confirms the orchestrator half needs no change. If it FAILS, the failure is real and `loop.go` is doing something kind-specific; fix the pipeline side, not `loop.go`'s `*lowConfidenceError` branch.

If the comment assertion fails because the fake env's triage handler is matched by a different substring than `"triage agent"`, check the actual triage prompt with `grep -n "triage agent" ai/prompts/triage.md.tmpl` and match the same phrase the existing `TestProcessOnceLowConfidenceEscalatesToNeedsInfo` uses at `loop_test.go:96`.

- [ ] **Step 3: Update the README config table**

In `README.md:210`, replace the `confidenceThreshold` row with:

```
| `confidenceThreshold` | no       | `70`       | Confidence score (0–100) below which an issue is escalated to `needsInfo` instead of implemented, on both the bug and feature routes; `0` disables the gate |
```

- [ ] **Step 4: Update the "Confidence gate" section**

In `README.md`, replace the body of the `### Confidence gate` section (currently lines 230-238) with:

```markdown
Both routes score how confidently the issue can be implemented as written
(0–100) before committing to an implementation. The feature pipeline's
brainstorm session scores from the issue text, before designing anything. The
bug pipeline's debug session may read the codebase first — a terse bug report
can still be trivially fixable once you open the file — but writes nothing
until after it has scored.

When that score is below `confidenceThreshold` (default `70`), the loop does
**not** guess: it comments the score and the session's specific questions on the
issue, applies the `ai-needs-info` label, removes the worktree, and stops. The
issue leaves the queue and is **not** auto-resumed — a human answers the
questions and removes the `ai-needs-info` label, which re-queues the issue from
scratch. Set `confidenceThreshold` to `0` to disable the gate on both routes and
always attempt an implementation.
```

- [ ] **Step 5: Update the loop-overview `bug:` bullet**

In `README.md:42-44`, replace the `bug:` bullet so it mentions the gate the way the `feature:` bullet does:

```markdown
   - `bug`: small, well-scoped defect → one systematic-debugging session that
     investigates, scores how confidently the bug can be fixed as reported and,
     below `confidenceThreshold`, escalates it to `ai-needs-info` instead of
     guessing (see below); otherwise it reproduces with a failing test, fixes,
     and commits.
```

- [ ] **Step 6: Run the full suite**

Run: `go build ./... && go test ./...`
Expected: PASS, no failures.

- [ ] **Step 7: Commit**

```bash
git add loop_test.go README.md
git commit -m "test: cover the bug route's needs-info escalation end to end"
```

---

## Assumptions

Recorded because this plan was written headless against an approved spec.

1. **Exact block wording.** The spec quotes one sentence verbatim ("You may read the codebase first to investigate — but do NOT write code, tests, or commits yet") and describes the rest as bullets. The full block text in Task 1 is this plan's rendering of those bullets, phrased to parallel `brainstorm.md.tmpl`. If an implementer improves the wording, the `threshold=70` golden must be updated to match; the `threshold=0` golden must not change.
2. **Golden test naming.** The spec says the goldens "match the existing brainstorm pair", so the existing `TestGoldenBugPrompt` is renamed `TestGoldenBugPromptWithoutThreshold` and a `…WithThreshold` sibling added, mirroring `TestGoldenBrainstormPromptWith/WithoutThreshold`.
3. **Boundary semantics.** `score < threshold` escalates, so a score exactly equal to the threshold proceeds — inherited from `pipeline_feature.go:50`. Task 2 adds an explicit test to pin it.
4. **No `rework.go` change.** `bugPrompt` has exactly one non-test call site (`pipeline_bug.go:9`); resumed sessions use `reworkPrompt` and are deliberately not re-gated, per the spec's non-goals.
