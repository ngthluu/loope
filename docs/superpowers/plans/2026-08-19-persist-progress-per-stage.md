# Push and open a PR at each feature-pipeline stage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Push the branch (and, at the spec stage, open the PR) right after each feature-pipeline stage commits — spec, plan, execute — instead of waiting for the whole pipeline to finish, while keeping every one of these steps best-effort so a push/PR/comment failure never aborts the pipeline.

**Architecture:** `RunFeaturePipeline`/`ResumeFeaturePipeline` (`pipeline_feature.go`) gain the `gh *GitHub`, `wt *Worktree`, `branch string` handles `ship` already has, plus `title string` and `n int` (the issue title and number `ship` also has via its `issue Issue` parameter — needed here too, for `prTitle`/`prBody`/`gh.Comment`). Three small best-effort helper calls are added inline at the existing stage boundaries: `pushSpecPR` right after `resolveSpec` succeeds, `pushPlanUpdate` right after `findPlanFile` succeeds, and a bare `wt.Push` right after `executePlan`/`resumeExecutePlan` return successfully. `ship` (`loop.go`) gains a `hasPR` check so it doesn't re-post the PR-link comment when the spec stage already posted it.

**Tech Stack:** Go, `gh`/`git` CLIs via the existing `Runner`/`GitHub`/`Worktree` seams, Go `text/template` prompts (`ai/prompts/*.md.tmpl`).

**Spec:** `docs/superpowers/specs/2026-08-19-persist-progress-per-stage-design.md`

## Global Constraints

- Feature pipeline only — `pipeline_bug.go`, `RunBugPipeline`, `ResumeBugPipeline` are untouched.
- The spec-stage PR is the only PR ever created for the branch; the plan and execute stages reuse it by pushing to the same branch, never calling `CreatePR` again.
- The plan-complete update is a `gh.Comment` with fixed text `Updated plan: `<path>`` — never a `gh pr edit --body` replace.
- Push/PR-create/comment failures at the spec or plan stage: `log.Printf` and swallow — the pipeline proceeds to the next stage exactly as if the step had succeeded.
- The execute-stage push point is a single push after the whole execute stage finishes (fresh or resumed) — no comment, no per-group/per-commit granularity.
- `Worktree.Push` and `GitHub.CreatePR` are already idempotent (`git push` is a no-op on no new commits; `CreatePR` recovers an existing PR's URL on "already exists") — reuse them as-is, add no new dedup logic beyond `hasPR`.
- No new state machine: the three push points are unconditional inline steps, not a persisted "which stage have I pushed" marker. `recordPR`'s existing `<logDir>/pr` file is the only new durable state, and `hasPR` only reads it.
- **Assumption (spec is imprecise on the exact parameter list):** the spec says `RunFeaturePipeline`/`ResumeFeaturePipeline` "gain three parameters" (`gh`, `wt`, `branch`) but its own design also requires `prTitle(title, n)`, `prBody(n, kind)`, and `gh.Comment(ctx, n, ...)` at the spec-stage push point — none of which are derivable from the parameters the functions have today. This plan adds two more threaded parameters, `title string` and `n int` (the same `issue.Title`/`issue.Number` `ship` already receives via its `issue Issue` parameter), alongside `gh`/`wt`/`branch`. `runPlanThenExecute`/`resumeExecutePlan` only need `n` (for `gh.Comment`), not `title` (the plan/execute push points never call `prTitle`).

---

### Task 1: `hasPR` in tracker.go

**Files:**
- Modify: `tracker.go` (add `hasPR` next to `recordPR`, around line 241)
- Test: `tracker_test.go`

**Interfaces:**
- Produces: `hasPR(logDir string) bool` — used by Task 5 (`ship`).

- [ ] **Step 1: Write the failing test**

Add to `tracker_test.go`, right after `TestRecordPRWritesFile`:

```go
func TestHasPRRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if hasPR(dir) {
		t.Error("no pr file recorded yet: hasPR should be false")
	}
	recordPR(dir, "https://github.com/o/r/pull/3")
	if !hasPR(dir) {
		t.Error("pr file recorded: hasPR should be true")
	}
	// Matches the other readers: an empty logDir is never mistaken for "has a PR".
	if hasPR("") {
		t.Error("empty logDir should never report hasPR")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestHasPRRoundTrip -v`
Expected: FAIL with `undefined: hasPR`

- [ ] **Step 3: Write minimal implementation**

In `tracker.go`, right after the `recordPR` function (after line 241):

```go
// hasPR reports whether recordPR has already written a PR URL for this issue,
// so ship (loop.go) can tell a PR the spec stage already opened apart from one
// it still needs to create itself, and skip re-posting the PR-link comment.
func hasPR(logDir string) bool {
	if logDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(logDir, "pr"))
	return err == nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestHasPRRoundTrip -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tracker.go tracker_test.go
git commit -m "feat: add hasPR to tell an already-recorded PR apart from a new one"
```

---

### Task 2: Thread gh/wt/branch/title/n through the feature pipeline; implement the spec-stage push+PR+comment

This is the largest task: it does the one-time plumbing every later push point depends on (new parameters on `RunFeaturePipeline`, `ResumeFeaturePipeline`, `brainstormLoop`, `runPlanThenExecute`; new call-site wiring in `loop.go`; migrating every existing test call site in `pipeline_feature_test.go` to the new signature) **and** lands the first real push point (spec-complete) in the same pass, because the plumbing has no independent test value on its own — Tasks 3 and 4 build on the parameters this task adds without touching the signatures again.

**Files:**
- Modify: `pipeline_feature.go` (signatures of `RunFeaturePipeline`, `ResumeFeaturePipeline`, `brainstormLoop`, `runPlanThenExecute`; new `pushSpecPR` helper; new `log` import)
- Modify: `loop.go` (the two `handleIssue` call sites, around lines 261–268)
- Modify: `pipeline_feature_test.go` (new `testGH`/`testWT` helpers; every existing `RunFeaturePipeline(...)`/`ResumeFeaturePipeline(...)` call site migrated to the new signature; two new tests)

**Interfaces:**
- Consumes: `hasPR`/`recordPR` (Task 1, tracker.go) — `recordPR` already existed; `pushSpecPR` calls it directly.
- Produces:
  - `RunFeaturePipeline(ctx, c, cfg, wtPath, issueContent, persona string, uat *UAT, gh *GitHub, wt *Worktree, branch, title string, n int) error`
  - `ResumeFeaturePipeline(ctx, c, cfg, wtPath, issueContent, persona string, uat *UAT, session SessionInfo, prompt string, gh *GitHub, wt *Worktree, branch, title string, n int) error`
  - `brainstormLoop(ctx, c, cfg, wtPath, issueContent, persona string, uat *UAT, sessionID, output string, start time.Time, gh *GitHub, wt *Worktree, branch, title string, n int) error`
  - `runPlanThenExecute(ctx, c, cfg, wtPath, prompt, resume string, start time.Time, gh *GitHub, wt *Worktree, branch string, n int) error`
  - `pushSpecPR(ctx, gh, wt, wtPath, branch, title string, n int, logDir string)` — used only inside `brainstormLoop`.
  - Test helpers `testGH() *GitHub` / `testWT() *Worktree` (pipeline_feature_test.go) — reused by Tasks 3 and 4's new tests.

- [ ] **Step 1: Write the two failing tests for the spec-stage push point**

Add to `pipeline_feature_test.go`, right after `TestFeaturePipelineQALoopThenExecute` (after line 173):

```go
// TestBrainstormLoopPushesAndCreatesPRAfterSpec locks in spec §1's ordering:
// the spec-stage push/PR/comment must complete BEFORE the plan session ever
// starts, using ship's own idempotent CreatePR/Push (so a later ship at the
// end of the run recovers the same PR instead of erroring).
func TestBrainstormLoopPushesAndCreatesPRAfterSpec(t *testing.T) {
	wt := t.TempDir()
	logDir := t.TempDir()

	var ghCalls []string
	gf := &fakeRunner{}
	gf.handler = func(c rcall) (string, string, error) {
		gf.mu.Lock()
		ghCalls = append(ghCalls, c.name+" "+strings.Join(c.args, " "))
		gf.mu.Unlock()
		if c.name == "gh" && strings.Contains(strings.Join(c.args, " "), "pr create") {
			return "https://github.com/org/repo/pull/42\n", "", nil
		}
		return "", "", nil
	}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	wtree := &Worktree{runner: gf}

	var prompts []string
	cf := &fakeRunner{}
	cf.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1: // architect: commits the spec straight away
			writeSpecFile(t, wt)
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 2: // fresh plan session: the spec-stage push/PR must already have run
			if len(ghCalls) == 0 {
				t.Fatal("plan session started before the spec-stage push/PR ran")
			}
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 3: // executor
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d: %v", len(prompts), c.args)
		return "", "", nil
	}
	c := &Claude{runner: cf, logDir: logDir}

	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE CONTENT", "PERSONA", nil,
		gh, wtree, "ai/issue-9", "Add export", 9); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 3 {
		t.Fatalf("got %d claude calls, want 3", len(prompts))
	}
	if len(ghCalls) < 3 {
		t.Fatalf("want at least push+create+comment on the gh/git runner, got %v", ghCalls)
	}
	if !strings.HasPrefix(ghCalls[0], "git push") {
		t.Errorf("first git/gh call should be the push, got %v", ghCalls)
	}
	var sawCreate, sawComment bool
	for _, call := range ghCalls {
		if strings.Contains(call, "pr create") {
			sawCreate = true
		}
		if strings.Contains(call, "issue comment") && strings.Contains(call, "pull/42") {
			sawComment = true
		}
	}
	if !sawCreate {
		t.Errorf("want a pr create call, got %v", ghCalls)
	}
	if !sawComment {
		t.Errorf("want the PR URL commented, got %v", ghCalls)
	}
	b, err := os.ReadFile(filepath.Join(logDir, "pr"))
	if err != nil || string(b) != "https://github.com/org/repo/pull/42" {
		t.Errorf("recordPR = %q, err=%v", b, err)
	}
}

// TestBrainstormLoopContinuesWhenSpecPushFails is decision 5: a push/PR/
// comment failure at the spec stage must not abort the pipeline — plan and
// execute still run.
func TestBrainstormLoopContinuesWhenSpecPushFails(t *testing.T) {
	wt := t.TempDir()
	gf := &fakeRunner{}
	gf.handler = func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "push") {
			return "", "connection refused", errors.New("git push: connection refused")
		}
		return "", "", nil
	}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	gh.retry = testRetry
	wtree := &Worktree{runner: gf, retry: testRetry}

	var prompts []string
	cf := &fakeRunner{}
	cf.handler = func(c rcall) (string, string, error) {
		prompts = append(prompts, c.stdin)
		switch len(prompts) {
		case 1:
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case 2:
			writePlanFile(t, wt)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case 3:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", len(prompts))
		return "", "", nil
	}
	c := &Claude{runner: cf}

	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE CONTENT", "PERSONA", nil,
		gh, wtree, "ai/issue-9", "Add export", 9); err != nil {
		t.Fatalf("a failed spec-stage push must not fail the pipeline, got %v", err)
	}
	if len(prompts) != 3 {
		t.Fatalf("pipeline must still run plan+execute despite the push failure, got %d claude calls", len(prompts))
	}
}
```

`fakeRunner.mu` is already exported within the package (lowercase but same package as the test file), used the same way `RunStream`/`Run` use it in `helpers_test.go`.

Also add, right after `writePlanFile` (after line 36), the two doubles every pre-existing test call site will be migrated to use:

```go
// testGH/testWT are no-op GitHub/Worktree doubles for the push/PR/comment
// steps the feature pipeline now runs mid-flight. They're backed by their OWN
// fakeRunner, deliberately separate from whatever runner a test's *Claude
// uses — so a push/PR/comment call never lands in a test's claude call count
// or prompt list.
func testGH() *GitHub {
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if strings.Contains(strings.Join(c.args, " "), "pr create") {
			return "https://github.com/org/repo/pull/1\n", "", nil
		}
		return "", "", nil
	}}
	return NewGitHub(f, &Config{RepoSlug: "org/repo"})
}

func testWT() *Worktree {
	return &Worktree{runner: &fakeRunner{}}
}
```

- [ ] **Step 2: Run the new tests to verify they fail to compile**

Run: `go vet ./... 2>&1 | head -30`
Expected: FAIL — `not enough arguments in call to RunFeaturePipeline` (the new tests already use the 12-argument signature; every pre-existing call site still uses the 7-argument one, so the package doesn't build yet). This is expected — Steps 3–5 fix it.

- [ ] **Step 3: Update the four function signatures in `pipeline_feature.go`**

Add `"log"` to the import block (after `"io/fs"`):

```go
import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)
```

Replace `RunFeaturePipeline` (lines 34–55):

```go
func RunFeaturePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT, gh *GitHub, wt *Worktree, branch, title string, n int) error {
	start := time.Now()
	res, err := architectCall(ctx, c, cfg, wtPath, "brainstorm-0", brainstormPrompt(issueContent, cfg.ConfidenceThreshold), "")
	// Record before the error check: an errored call (e.g. a 429 session limit)
	// still returns a session id, and the dashboard shows it on the parked ticket.
	if res != nil {
		c.RecordSession(res.SessionID, "feature", stageBrainstorm)
		c.RecordSnapshot(issueContent)
	}
	if err != nil {
		return err
	}

	// Upfront confidence gate: judged once, on the first brainstorm turn only.
	// A threshold <= 0 disables it. Fail open on an unparseable score.
	if cfg.ConfidenceThreshold > 0 {
		if score, ok := parseConfidence(res.Result); ok && score < cfg.ConfidenceThreshold {
			return &lowConfidenceError{score: score, feedback: sanitizeFeedback(res.Result)}
		}
	}
	return brainstormLoop(ctx, c, cfg, wtPath, issueContent, persona, uat, res.SessionID, res.Result, start, gh, wt, branch, title, n)
}
```

Replace `ResumeFeaturePipeline` (lines 62–82):

```go
func ResumeFeaturePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT, session SessionInfo, prompt string, gh *GitHub, wt *Worktree, branch, title string, n int) error {
	start := time.Now()
	switch session.Stage {
	case stagePlan:
		return runPlanThenExecute(ctx, c, cfg, wtPath, prompt, session.SessionID, start, gh, wt, branch, n)
	case stageExecute:
		if err := resumeExecutePlan(ctx, c, cfg, wtPath, prompt, session.SessionID); err != nil {
			return err
		}
		// Execute complete (spec §1): push once, best-effort — ship's own push at
		// the end of a successful run is the backstop, so a failure here is
		// logged and swallowed rather than failing an otherwise-successful resume.
		if perr := wt.Push(ctx, wtPath, branch); perr != nil {
			log.Printf("issue #%d: execute-stage push failed: %v", n, perr)
		}
		return nil
	case stageBrainstorm:
		res, err := architectCall(ctx, c, cfg, wtPath, "brainstorm-resume", prompt, session.SessionID)
		if res != nil {
			c.RecordSession(res.SessionID, "feature", stageBrainstorm)
			c.RecordSnapshot(issueContent)
		}
		if err != nil {
			return err
		}
		return brainstormLoop(ctx, c, cfg, wtPath, issueContent, persona, uat, res.SessionID, res.Result, start, gh, wt, branch, title, n)
	default:
		return RunFeaturePipeline(ctx, c, cfg, wtPath, issueContent, persona, uat, gh, wt, branch, title, n)
	}
}
```

Replace the `brainstormLoop` signature and the `SPEC_READY` branch (lines 99–115):

```go
func brainstormLoop(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT, sessionID, output string, start time.Time, gh *GitHub, wt *Worktree, branch, title string, n int) error {
	for round := 1; ; round++ {
		// The architect signals a committed spec: hand off to the fresh plan
		// session, then execute. If it claims a spec but none is on disk, fall
		// through and keep prodding (mirrors the plan-file behavior).
		if rel, ok := parseSpecReady(output); ok {
			if specPath, ok := resolveSpec(wtPath, rel, start); ok {
				// The UAT session runs alongside plan/execute — it only reads
				// the committed spec, so nothing downstream waits on it. The
				// wait runs on every exit path (including a failed plan): the
				// session must not outlive the pipeline, whose worktree and
				// context the caller tears down on return.
				wait := uat.StartFeature(ctx, c, cfg, wtPath, specPath)
				defer wait()
				// Spec complete (spec §1): push, open (or recover) the PR,
				// comment the URL, and record it — before plan/execute run at
				// all. Best-effort: pushSpecPR logs and swallows its own
				// failures rather than turning a completed spec into an error.
				pushSpecPR(ctx, gh, wt, wtPath, branch, title, n, c.logDir)
				return runPlanThenExecute(ctx, c, cfg, wtPath, planPrompt(specPath), "", start, gh, wt, branch, n)
			}
		}
```

(The rest of `brainstormLoop`'s body — the already-done branch, the Q&A round, and the closing `architectCall` — is unchanged; only the signature line and the block shown above change.)

Replace the `runPlanThenExecute` signature (line 173 only — body unchanged in this task):

```go
func runPlanThenExecute(ctx context.Context, c *Claude, cfg *Config, wtPath, prompt, resume string, start time.Time, gh *GitHub, wt *Worktree, branch string, n int) error {
```

Add the new helper, right after `runPlanThenExecute` (after its closing `}`, before the `maxExecuteGroups` const block):

```go
// pushSpecPR runs the spec-complete push point (spec §1): push the branch,
// open (or recover) its PR, comment the URL, and record it for the
// dashboard. Best-effort — decision 5: any failure here is logged and
// swallowed, never turning a completed spec stage into a pipeline error.
// Worktree.Push and GitHub.CreatePR are both idempotent (see worktree.go,
// github.go), so ship's own push/CreatePR at the very end of a successful
// run — and any later push/PR-create from the plan or execute stage — safely
// repeats whatever this call already did.
func pushSpecPR(ctx context.Context, gh *GitHub, wt *Worktree, wtPath, branch, title string, n int, logDir string) {
	if err := wt.Push(ctx, wtPath, branch); err != nil {
		log.Printf("issue #%d: spec-stage push failed: %v", n, err)
		return
	}
	url, err := gh.CreatePR(ctx, branch, prTitle(title, n), prBody(n, "feature"))
	if err != nil {
		log.Printf("issue #%d: spec-stage PR create failed: %v", n, err)
		return
	}
	if err := gh.Comment(ctx, n, prComment(url)); err != nil {
		log.Printf("issue #%d: spec-stage PR comment failed: %v", n, err)
	}
	recordPR(logDir, url)
}
```

- [ ] **Step 4: Update the two `handleIssue` call sites in `loop.go`**

`branch` and `n` are already in scope in `handleIssue` (`branch := branchName(n)` and `n := issue.Number`, lines 223–224). Replace lines 261–268:

```go
			if session.Kind == "bug" {
				perr = ResumeBugPipeline(ctx, c, o.cfg, wtPath, content, base, uat, session, prompt)
			} else {
				perr = ResumeFeaturePipeline(ctx, c, o.cfg, wtPath, content, persona, uat, session, prompt, o.gh, o.wt, branch, issue.Title, n)
			}
		} else if kind == "bug" {
			perr = RunBugPipeline(ctx, c, o.cfg, wtPath, content, base, uat)
		} else {
			perr = RunFeaturePipeline(ctx, c, o.cfg, wtPath, content, persona, uat, o.gh, o.wt, branch, issue.Title, n)
		}
```

- [ ] **Step 5: Migrate every pre-existing call site in `pipeline_feature_test.go`**

There are ~22 pre-existing `RunFeaturePipeline(...)`/`ResumeFeaturePipeline(...)` calls in this file (all with the old 7/9-argument signature) that don't care about push behavior — they only assert on the `*Claude`'s prompts/sessions. Rather than hand-edit each one, run this paren-balancing script from the repo root. It finds every call to either function and appends the five new arguments (`testGH()`, `testWT()`, a fixed branch/title/issue-number) right before that call's closing `)` — but only in the part of the file BEFORE `TestBrainstormLoopPushesAndCreatesPRAfterSpec` (the test added in Step 1, whose calls already pass real doubles and must not be touched again). It's safe here because none of the pre-existing calls' argument lists contain any unbalanced parens inside string literals (verified: no `(`/`)` appear in any of the string args passed today).

```bash
python3 - <<'PYEOF'
import pathlib

path = pathlib.Path("pipeline_feature_test.go")
src = path.read_text()
extra = ', testGH(), testWT(), "ai/issue-1", "Feature title", 1'

def insert_args(text, fname, extra):
    out, i = [], 0
    marker = fname + "("
    while True:
        j = text.find(marker, i)
        if j == -1:
            out.append(text[i:])
            break
        out.append(text[i:j + len(marker)])
        depth, k = 1, j + len(marker)
        while depth > 0:
            if text[k] == "(":
                depth += 1
            elif text[k] == ")":
                depth -= 1
            k += 1
        out.append(text[j + len(marker):k - 1] + extra)
        out.append(")")
        i = k
    return "".join(out)

marker = "func TestBrainstormLoopPushesAndCreatesPRAfterSpec"
cut = src.index(marker)
head, tail = src[:cut], src[cut:]
head = insert_args(head, "RunFeaturePipeline", extra)
head = insert_args(head, "ResumeFeaturePipeline", extra)
path.write_text(head + tail)
PYEOF
gofmt -w pipeline_feature_test.go
```

- [ ] **Step 6: Build and run the full test file**

Run: `go build ./... && go test ./... -run 'TestFeaturePipeline|TestResumeFeaturePipeline|TestBrainstormLoop|TestExecutePlan|TestRunGroupWithRetry' -v 2>&1 | tail -100`
Expected: PASS for every test — the 22 migrated call sites keep their original assertions untouched (they only ever inspected the `*Claude` fakeRunner, never `testGH()`/`testWT()`'s), and the two new tests from Step 1 pass now that the signatures compile.

- [ ] **Step 7: Run the full package test suite for a regression check**

Run: `go build ./... && go test ./... 2>&1 | tail -60`
Expected: PASS — `loop.go`'s two call sites (Step 4) now supply real `o.gh`/`o.wt`, so every existing `loop_test.go` test (which fakes `git`/`gh` through the SAME runner as `claude`, per `newFakeEnv`) now also sees `git push`/`gh pr create`/`gh issue comment` calls landing during the feature-pipeline's spec stage rather than only at `ship`. Since `newFakeEnv`'s default handler already answers those the same way regardless of when they arrive (empty success, or the `pull/99` URL for `pr create`), no existing `loop_test.go` assertion should break — but if `TestProcessOnceHappyPathBug` or a similar bug-only test starts failing, that's a real bug in this task's wiring (the bug pipeline must never route through `RunFeaturePipeline`), not a fixture problem.

- [ ] **Step 8: Commit**

```bash
git add pipeline_feature.go loop.go pipeline_feature_test.go
git commit -m "feat: push and open the PR right after the spec stage commits"
```

---

### Task 3: Plan-stage push + "Updated plan" comment

**Files:**
- Modify: `ai/prompts/comments.md.tmpl` (new `plan-comment` template)
- Modify: `loop.go` (new `planComment` builder, next to `prComment`/`prTitle`/`prBody`)
- Modify: `prompts_test.go` (new `promptTestData` entry)
- Modify: `prompts_golden_test.go` (new golden test)
- Modify: `pipeline_feature.go` (`runPlanThenExecute` gains the plan-stage push; new `pushPlanUpdate` helper)
- Test: `pipeline_feature_test.go`

**Interfaces:**
- Consumes: `runPlanThenExecute(ctx, c, cfg, wtPath, prompt, resume string, start time.Time, gh *GitHub, wt *Worktree, branch string, n int) error` (Task 2).
- Produces: `planComment(path string) string` (loop.go), `pushPlanUpdate(ctx, gh, wt, wtPath, branch string, n int, planPath string)` (pipeline_feature.go) — the latter is internal to `runPlanThenExecute`, not called anywhere else.

- [ ] **Step 1: Write the failing test**

Add to `pipeline_feature_test.go`, at the end of the file:

```go
// TestRunPlanThenExecutePushesAndCommentsPlanUpdate locks in spec §1's
// plan-stage push point: a push, then a fixed "Updated plan: ..." comment
// naming the plan file relative to the worktree root — BEFORE the execute
// session starts. No PR is created here (the spec stage already created it).
func TestRunPlanThenExecutePushesAndCommentsPlanUpdate(t *testing.T) {
	wt := t.TempDir()

	var ghCalls []string
	gf := &fakeRunner{}
	gf.handler = func(c rcall) (string, string, error) {
		ghCalls = append(ghCalls, c.name+" "+strings.Join(c.args, " "))
		return "", "", nil
	}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	wtree := &Worktree{runner: gf}

	var calls int
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		calls++
		switch calls {
		case 1: // fresh plan session: commits the plan
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 2: // executor: the plan-stage push/comment must already have run
			if len(ghCalls) == 0 {
				t.Fatal("execute session started before the plan-stage push/comment ran")
			}
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", calls)
		return "", "", nil
	}}
	c := &Claude{runner: cf}

	err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), gh, wtree, "ai/issue-9", 9)
	if err != nil {
		t.Fatal(err)
	}
	var sawComment, sawCreate bool
	pushes := 0
	for _, call := range ghCalls {
		if strings.HasPrefix(call, "git push") {
			pushes++
		}
		if strings.Contains(call, "issue comment") && strings.Contains(call, "Updated plan") &&
			strings.Contains(call, "docs/superpowers/plans/2026-07-06-thing.md") {
			sawComment = true
		}
		if strings.Contains(call, "pr create") {
			sawCreate = true
		}
	}
	if pushes != 1 {
		t.Errorf("want exactly 1 push at this stage (execute-stage push lands in a later task), got %d: %v", pushes, ghCalls)
	}
	if !sawComment {
		t.Errorf("want the 'Updated plan' comment naming the plan file, got %v", ghCalls)
	}
	if sawCreate {
		t.Error("the plan stage must never create a PR — the spec stage already did")
	}
}
```

Note this test relies on `writePlanFile` (helper already in this file) writing to `wt/docs/superpowers/plans/2026-07-06-thing.md`, so the "relative to worktree root" path the comment should carry is exactly `docs/superpowers/plans/2026-07-06-thing.md`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestRunPlanThenExecutePushesAndCommentsPlanUpdate -v`
Expected: FAIL — `runPlanThenExecute` doesn't push or comment yet, so `ghCalls` is empty and both `pushes != 1` and `!sawComment` fire.

- [ ] **Step 3: Add the `plan-comment` template**

In `ai/prompts/comments.md.tmpl`, right after the `pr-comment` block (after line 31):

```
{{define "plan-comment"}}🤖 Updated plan: `{{.Path}}`

{{.BotMarker}}{{end}}

```

- [ ] **Step 4: Add `planComment` in `loop.go`**

Right after `prComment` (after line 601):

```go
func planComment(path string) string {
	d := promptData()
	d["Path"] = path
	return mustRender("plan-comment", d)
}
```

- [ ] **Step 5: Wire the template into the two prompt test files**

In `prompts_test.go`, add to `promptTestData` (right after the `"pr-comment"` entry):

```go
	"plan-comment":              {"Path": "docs/superpowers/plans/2026-plan.md"},
```

In `prompts_golden_test.go`, add right after `TestGoldenPRComment`:

```go
func TestGoldenPlanComment(t *testing.T) {
	check(t, "planComment", planComment("docs/superpowers/plans/2026-plan.md"),
		"🤖 Updated plan: `docs/superpowers/plans/2026-plan.md`\n\n"+botMarker)
}
```

- [ ] **Step 6: Implement `pushPlanUpdate` and wire it into `runPlanThenExecute`**

In `pipeline_feature.go`, add right after `pushSpecPR`:

```go
// pushPlanUpdate runs the plan-complete push point (spec §1): push the
// branch, then post the fixed "Updated plan: ..." comment naming the plan
// file relative to the worktree root. Best-effort, same as pushSpecPR — no
// PR is created here, the spec stage already created (or ship's own backstop
// is about to create) the one PR this branch ever gets.
func pushPlanUpdate(ctx context.Context, gh *GitHub, wt *Worktree, wtPath, branch string, n int, planPath string) {
	if err := wt.Push(ctx, wtPath, branch); err != nil {
		log.Printf("issue #%d: plan-stage push failed: %v", n, err)
		return
	}
	rel, err := filepath.Rel(wtPath, planPath)
	if err != nil {
		rel = planPath
	}
	if err := gh.Comment(ctx, n, planComment(rel)); err != nil {
		log.Printf("issue #%d: plan-stage comment failed: %v", n, err)
	}
}
```

In `runPlanThenExecute`, replace the last two lines of the function:

```go
	plan, ok := findPlanFile(wtPath, start)
	if !ok {
		return fmt.Errorf("feature pipeline: plan session signaled %s but wrote no plan file", readySentinel)
	}
	return executePlan(ctx, c, cfg, wtPath, plan)
}
```

with:

```go
	plan, ok := findPlanFile(wtPath, start)
	if !ok {
		return fmt.Errorf("feature pipeline: plan session signaled %s but wrote no plan file", readySentinel)
	}
	// Plan complete (spec §1): push, then post the fixed "Updated plan: ..."
	// comment naming the plan file — before execute runs at all.
	pushPlanUpdate(ctx, gh, wt, wtPath, branch, n, plan)
	return executePlan(ctx, c, cfg, wtPath, plan)
}
```

- [ ] **Step 7: Run test to verify it passes**

Run: `go test ./... -run 'TestRunPlanThenExecutePushesAndCommentsPlanUpdate|TestGoldenPlanComment|TestEveryTemplateRenders' -v`
Expected: PASS

- [ ] **Step 8: Add the error-swallowing regression test**

Add to `pipeline_feature_test.go`:

```go
// TestRunPlanThenExecutePlanPushFailureDoesNotFailPipeline is decision 5 for
// the plan stage: a push/comment failure must not abort the pipeline.
func TestRunPlanThenExecutePlanPushFailureDoesNotFailPipeline(t *testing.T) {
	wt := t.TempDir()
	gf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "push") {
			return "", "timeout", errors.New("git push: timeout")
		}
		return "", "", nil
	}}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	gh.retry = testRetry
	wtree := &Worktree{runner: gf, retry: testRetry}

	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		writePlanFile(t, wt)
		return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
	}}
	c := &Claude{runner: f}
	err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), gh, wtree, "ai/issue-9", 9)
	if err != nil {
		t.Fatalf("a failed plan-stage push must not fail the pipeline, got %v", err)
	}
}
```

- [ ] **Step 9: Run test to verify it passes**

Run: `go test ./... -run TestRunPlanThenExecutePlanPushFailureDoesNotFailPipeline -v`
Expected: PASS

- [ ] **Step 10: Commit**

```bash
git add ai/prompts/comments.md.tmpl loop.go prompts_test.go prompts_golden_test.go pipeline_feature.go pipeline_feature_test.go
git commit -m "feat: push and comment an update right after the plan stage commits"
```

---

### Task 4: Execute-stage push (fresh and resumed paths)

**Files:**
- Modify: `pipeline_feature.go` (`runPlanThenExecute` gains the execute-stage push; `ResumeFeaturePipeline`'s `stageExecute` case already has it from Task 2 — this task only adds its test)
- Test: `pipeline_feature_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–3.
- Produces: nothing new — this task completes `runPlanThenExecute`'s body (no further signature changes).

- [ ] **Step 1: Write the failing test for the fresh path**

Add to `pipeline_feature_test.go`:

```go
// TestRunPlanThenExecutePushesAfterExecuteCompletes locks in spec §1's third
// push point: after executePlan succeeds, push once more — no comment.
func TestRunPlanThenExecutePushesAfterExecuteCompletes(t *testing.T) {
	wt := t.TempDir()
	var ghCalls []string
	gf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		ghCalls = append(ghCalls, c.name+" "+strings.Join(c.args, " "))
		return "", "", nil
	}}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	wtree := &Worktree{runner: gf}

	var calls int
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		calls++
		switch calls {
		case 1:
			writePlanFile(t, wt)
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case 2:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		t.Fatalf("unexpected call %d", calls)
		return "", "", nil
	}}
	c := &Claude{runner: cf}

	err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), gh, wtree, "ai/issue-9", 9)
	if err != nil {
		t.Fatal(err)
	}
	pushes := 0
	for _, call := range ghCalls {
		if strings.HasPrefix(call, "git push") {
			pushes++
		}
		if strings.Contains(call, "issue comment") && !strings.Contains(call, "Updated plan") {
			t.Errorf("the execute stage must not comment, got %v", call)
		}
	}
	if pushes != 2 {
		t.Errorf("want 2 pushes (plan-stage + execute-stage), got %d: %v", pushes, ghCalls)
	}
}

// TestExecuteStagePushFailureDoesNotFailPipeline is decision 5 for the
// execute stage: a push failure after executePlan succeeds must not fail an
// otherwise-successful pipeline run.
func TestExecuteStagePushFailureDoesNotFailPipeline(t *testing.T) {
	wt := t.TempDir()
	gf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), "push") {
			return "", "timeout", errors.New("git push: timeout")
		}
		return "", "", nil
	}}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	gh.retry = testRetry
	wtree := &Worktree{runner: gf, retry: testRetry}

	var calls int
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		calls++
		switch calls {
		case 1:
			writePlanFile(t, wt)
			return claudeJSON("PIPELINE_READY", "plan-1"), "", nil
		case 2:
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		return "", "", nil
	}}
	c := &Claude{runner: cf}
	if err := runPlanThenExecute(context.Background(), c, featureConfig(), wt,
		planPrompt("docs/superpowers/specs/2026-07-13-thing-design.md"), "",
		time.Now().Add(-time.Second), gh, wtree, "ai/issue-9", 9); err != nil {
		t.Fatalf("a failed execute-stage push must not fail the pipeline, got %v", err)
	}
}

// TestResumeFeaturePipelineExecuteStagePushesAfterSuccess covers the OTHER
// path to the execute-stage push: ResumeFeaturePipeline's stageExecute case
// (wired in Task 2, tested here).
func TestResumeFeaturePipelineExecuteStagePushesAfterSuccess(t *testing.T) {
	logDir := t.TempDir()
	var ghCalls []string
	gf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		ghCalls = append(ghCalls, c.name+" "+strings.Join(c.args, " "))
		return "", "", nil
	}}
	gh := NewGitHub(gf, &Config{RepoSlug: "org/repo"})
	wtree := &Worktree{runner: gf}
	cf := &fakeRunner{handler: func(c rcall) (string, string, error) {
		return claudeJSON("executed more", "exec-sess-2"), "", nil
	}}
	c := &Claude{runner: cf, logDir: logDir}
	cfg := featureConfig()
	session := SessionInfo{SessionID: "exec-sess", Kind: "feature", Stage: stageExecute}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, "/wt", "the issue", "", nil, session, "continue",
		gh, wtree, "ai/issue-9", "Add export", 9); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, call := range ghCalls {
		if strings.HasPrefix(call, "git push") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a push after the resumed execute session succeeds, got %v", ghCalls)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestRunPlanThenExecutePushesAfterExecuteCompletes|TestExecuteStagePushFailureDoesNotFailPipeline|TestResumeFeaturePipelineExecuteStagePushesAfterSuccess' -v`
Expected: `TestRunPlanThenExecutePushesAfterExecuteCompletes` and `TestExecuteStagePushFailureDoesNotFailPipeline` FAIL (`pushes != 2` / no push at all yet — `runPlanThenExecute` doesn't push after `executePlan` yet). `TestResumeFeaturePipelineExecuteStagePushesAfterSuccess` already PASSes (Task 2 already wired this path) — that's expected; it's included here for completeness of the execute-stage push point's coverage, not because it's new behavior.

- [ ] **Step 3: Implement the fresh-path push in `runPlanThenExecute`**

Replace the tail of `runPlanThenExecute` (as left by Task 3):

```go
	// Plan complete (spec §1): push, then post the fixed "Updated plan: ..."
	// comment naming the plan file — before execute runs at all.
	pushPlanUpdate(ctx, gh, wt, wtPath, branch, n, plan)
	return executePlan(ctx, c, cfg, wtPath, plan)
}
```

with:

```go
	// Plan complete (spec §1): push, then post the fixed "Updated plan: ..."
	// comment naming the plan file — before execute runs at all.
	pushPlanUpdate(ctx, gh, wt, wtPath, branch, n, plan)
	if err := executePlan(ctx, c, cfg, wtPath, plan); err != nil {
		return err
	}
	// Execute complete (spec §1): push once, best-effort — ship's own push at
	// the end of a successful run is the backstop.
	if perr := wt.Push(ctx, wtPath, branch); perr != nil {
		log.Printf("issue #%d: execute-stage push failed: %v", n, perr)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestRunPlanThenExecutePushesAfterExecuteCompletes|TestExecuteStagePushFailureDoesNotFailPipeline|TestResumeFeaturePipelineExecuteStagePushesAfterSuccess' -v`
Expected: PASS

- [ ] **Step 5: Run the full package suite**

Run: `go build ./... && go test ./... 2>&1 | tail -60`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add pipeline_feature.go pipeline_feature_test.go
git commit -m "feat: push once more after the execute stage completes"
```

---

### Task 5: `ship` skips the PR-link comment when the spec stage already posted it

**Files:**
- Modify: `loop.go` (`ship`, lines 482–512)
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `hasPR(logDir string) bool` (Task 1).

- [ ] **Step 1: Write the failing test**

Add to `loop_test.go`, right after `TestParkWritesCauseAndShipClearsIt`:

```go
// TestShipSkipsPRCommentWhenAlreadyRecorded is spec §3: when the spec stage
// already created the PR and posted the link comment (hasPR is true), ship
// must still run CommitCount/Push/CreatePR/the label swap, but skip
// re-posting the comment and re-writing the pr file.
func TestShipSkipsPRCommentWhenAlreadyRecorded(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	logDir := o.issueLogDir(7)
	recordPR(logDir, "https://github.com/org/repo/pull/99")
	issue := Issue{Number: 7, Title: "Fix crash"}
	if err := o.ship(context.Background(), issue, worktreePath(o.cfg.WorkDir, 7), branchName(7), "main", "feature"); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "pr create")) == 0 {
		t.Error("ship must still call CreatePR to resolve the canonical URL for the label swap")
	}
	prComments := 0
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "pull/99") {
			prComments++
		}
	}
	if prComments != 0 {
		t.Error("ship must not re-post the PR-link comment when hasPR is already true")
	}
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-done") {
		t.Errorf("want the wip->done swap to still run, got: %v", swap)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestShipSkipsPRCommentWhenAlreadyRecorded -v`
Expected: FAIL — `prComments != 0` (today's `ship` always comments).

- [ ] **Step 3: Update `ship`**

Replace lines 494–502:

```go
	if err := o.wt.Push(ctx, wtPath, branch); err != nil {
		return onInfra(err)
	}
	url, err := o.gh.CreatePR(ctx, branch, prTitle(issue.Title, n), prBody(n, kind))
	if err != nil {
		return onInfra(err)
	}
	_ = o.gh.Comment(ctx, n, prComment(url))
	recordPR(o.issueLogDir(n), url)
```

with:

```go
	if err := o.wt.Push(ctx, wtPath, branch); err != nil {
		return onInfra(err)
	}
	logDir := o.issueLogDir(n)
	// The spec stage may already have created this PR and posted the link
	// comment (spec §3) — check BEFORE CreatePR so a PR already recorded is
	// told apart from one ship still needs to announce for the first time.
	alreadyAnnounced := hasPR(logDir)
	url, err := o.gh.CreatePR(ctx, branch, prTitle(issue.Title, n), prBody(n, kind))
	if err != nil {
		return onInfra(err)
	}
	if !alreadyAnnounced {
		_ = o.gh.Comment(ctx, n, prComment(url))
		recordPR(logDir, url)
	}
```

And replace the two remaining `o.issueLogDir(n)` calls later in the function (the `recordState`/`clearParkCause` lines) with `logDir` (now already computed above) — the current tail of `ship`:

```go
	if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Done); err != nil {
		// PR is up but the Done swap failed. Surface it; leave ai-wip in place so
		// the issue isn't re-run just to retry a label swap (CreatePR is
		// idempotent).
		return fmt.Errorf("issue #%d: PR created (%s) but marking done failed: %w", n, url, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Done)
	clearParkCause(o.issueLogDir(n))
	return nil
}
```

becomes:

```go
	if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Done); err != nil {
		// PR is up but the Done swap failed. Surface it; leave ai-wip in place so
		// the issue isn't re-run just to retry a label swap (CreatePR is
		// idempotent).
		return fmt.Errorf("issue #%d: PR created (%s) but marking done failed: %w", n, url, err)
	}
	recordState(logDir, o.cfg.StateLabels.Done)
	clearParkCause(logDir)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestShipSkipsPRCommentWhenAlreadyRecorded -v`
Expected: PASS

- [ ] **Step 5: Run the existing `ship`-adjacent tests to confirm the "no PR yet" path is unchanged**

Run: `go test ./... -run 'TestProcessOnceHappyPathBug|TestParkWritesCauseAndShipClearsIt|TestDoneSwapFailureIsSurfaced|TestHandleIssueZeroCommitsParksForRework' -v`
Expected: PASS — these all exercise `ship` with no `pr` file pre-recorded (`hasPR` false), so `alreadyAnnounced` is false and the comment/record still happen exactly as before.

- [ ] **Step 6: Commit**

```bash
git add loop.go loop_test.go
git commit -m "fix: ship no longer re-posts the PR-link comment when the spec stage already did"
```

---

### Task 6: Integration test — PR exists right after the spec stage, exactly one PR comment for the whole run

**Files:**
- Test: `loop_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–5. This task adds no production code — it's the spec's required end-to-end check (Testing section, last bullet).

- [ ] **Step 1: Write the test**

Add to `loop_test.go`, at the end of the file:

```go
// TestProcessOnceFeatureOpensPRAfterSpecStage is the spec's required
// end-to-end check: a full feature-pipeline run (brainstorm -> spec -> plan
// -> execute -> ship) must have a PR open, and commented, right after the
// spec stage — before plan or execute run at all — and only ONE "🤖 PR:"
// comment must exist on the issue by the time the whole run finishes (ship
// must not re-announce the PR the spec stage already announced).
func TestProcessOnceFeatureOpensPRAfterSpecStage(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	var prCreatedBeforePlan bool
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" && strings.Contains(c.stdin, "triage agent") {
			return claudeJSON(`{"issueNumber": 7, "kind": "feature", "reason": "needs design"}`, "t1"), "", nil
		}
		if c.name == "claude" && strings.Contains(c.stdin, "brainstorming") {
			writeSpecFile(t, worktreePath(env.wtDir, 7))
			return claudeJSON("Spec written.\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		}
		if c.name == "claude" && strings.Contains(c.stdin, "writing-plans") {
			if len(env.callsMatching("gh", "pr create")) > 0 {
				prCreatedBeforePlan = true
			}
			writePlanFile(t, worktreePath(env.wtDir, 7))
			return claudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		}
		if c.name == "claude" && strings.Contains(c.stdin, "executing-plans") {
			return claudeJSON("Executed.", "exec-1"), "", nil
		}
		return base(c)
	}
	o := env.orchestrator()
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !prCreatedBeforePlan {
		t.Error("the PR was not created before the plan session ran")
	}
	prComments := 0
	for _, c := range env.callsMatching("gh", "issue comment") {
		if strings.Contains(c, "pull/99") {
			prComments++
		}
	}
	if prComments != 1 {
		t.Errorf("want exactly one PR-link comment across the whole run, got %d", prComments)
	}
	swap := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-done") {
		t.Errorf("want a single ai-wip->ai-done swap, got: %v", swap)
	}
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./... -run TestProcessOnceFeatureOpensPRAfterSpecStage -v`
Expected: PASS. If `prComments` comes back `2` instead of `1`, Task 5's `hasPR` check in `ship` isn't wired correctly — re-check Task 5. If `prCreatedBeforePlan` is `false`, Task 2's `pushSpecPR` call isn't reached before `runPlanThenExecute` — re-check the placement in `brainstormLoop`.

- [ ] **Step 3: Run the full package suite one more time**

Run: `go build ./... && go vet ./... && go test ./... 2>&1 | tail -60`
Expected: PASS, no vet warnings.

- [ ] **Step 4: Commit**

```bash
git add loop_test.go
git commit -m "test: cover the full feature pipeline opening its PR after the spec stage"
```

---

## Self-Review Notes

- **Spec coverage:** §1 (three push points) — Tasks 2–4. §2 (idempotent primitives, no new dedup) — reused `Worktree.Push`/`GitHub.CreatePR` as-is throughout; no new retry/dedup logic added. §3 (`ship` skips the comment) — Task 5. §4 ("what doesn't change" — bug pipeline untouched, no new state machine) — verified no `pipeline_bug.go` edits anywhere in this plan; `hasPR` reads the existing `recordPR` file, no new file introduced. Testing section's four bullets — Task 2 Step 1 (unit, push points + error swallowing), Task 1 (hasPR/recordPR round-trip), Task 5 (ship regression), Task 6 (integration).
- **Placeholder scan:** none — every step has literal code, exact file/line anchors, and runnable commands.
- **Type consistency:** `pushSpecPR`/`pushPlanUpdate` signatures match their call sites exactly across Tasks 2–4; `hasPR(logDir string) bool` (Task 1) matches its Task 5 call `hasPR(logDir)`; `planComment(path string) string` (Task 3) matches its Task 3 call `planComment(rel)`.
