# Persist Worktrees and Resume Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every re-entry into a ticket's pipeline (API-error park, `ai-rework` removed, `ai-needs-info` answered, dashboard Continue, daemon restart) resume the same Claude session with `--resume` instead of restarting `brainstorm-0`/`debug` from scratch, and stop the daemon from ever deleting a worktree or branch.

**Architecture:** `SessionInfo` gains a `Stage` field so `handleIssue` knows which pipeline entry point a persisted session belongs to. `handleIssue` becomes a single decision point: no session file (or an unreadable one) on disk → today's fresh `RunFeaturePipeline`/`RunBugPipeline` path, unchanged; a session file present → a new `Resume*Pipeline` entry point that re-enters at the recorded stage with `--resume <id>` and a trigger-appropriate prompt (`"continue"`, or an added-lines diff against a persisted issue snapshot for an `ai-needs-info` re-entry). `finishDone`, `finishNeedsInfo`, and `ship` stop calling `Worktree.Remove`/`DeleteBranch` entirely.

**Tech Stack:** Go 1.x, existing `Claude`/`Worktree`/`GitHub`/`Orchestrator` types, table-driven `testing` with the repo's `fakeRunner`/`fakeEnv` harness.

**Spec:** `docs/superpowers/specs/2026-08-19-persist-worktree-and-resume-session-design.md`

## Global Constraints

- `SessionInfo.Stage` values are exactly `"brainstorm"`, `"plan"`, `"execute"`, `"debug"` (spec §2).
- No stuck-process detection and no automatic retry loop — only *what happens after* an existing human/label trigger changes (spec Decisions 1–2).
- `finishDone`, `finishNeedsInfo`, and `ship` never delete the worktree or branch, with no exceptions (spec Decision 3, §3).
- The `ai-needs-info` resume prompt is an added-lines diff of new issue content against the last-seen snapshot, not a bare `"continue"` (spec Decision 4, §4).
- A fresh daemon start auto-resumes any ticket with a persisted session via the existing `SweepOrphans` re-queue — no new startup pass (spec Decision 5, §5).
- `readSession` failure (missing/corrupt file) is always treated as "no persisted session" and falls through to the fresh path — never a hard error (spec, Error handling).
- A `--resume` call that itself fails surfaces exactly like any other pipeline error today (`park`, comment, wait for a human) — no fallback to a fresh brainstorm (spec, Error handling).

## Assumptions (not spelled out verbatim in the spec — flagged for review)

1. **Which trigger fired is inferred from the local state marker, not passed explicitly.** All four re-entry triggers (rework label removed, needs-info label removed, dashboard Continue, orphan sweep) converge on the exact same path: the issue loses its state label, `ListEligibleIssues` picks it up as eligible again, and `handleIssue` runs with no extra signal about *why*. To build the needs-info-specific diff prompt (spec §4) `handleIssue` must peek at the issue's local `state` marker (written by `finishNeedsInfo`/`park`/`pause`) **before** overwriting it with `ai-wip`, and compare it against `cfg.StateLabels.NeedsInfo`. This needs a new `readState(logDir string) string` helper (mirroring the existing `recordState`/`clearState` in `tracker.go`) — the plan below adds it in Task 5.
2. **The diff algorithm is a multiset line-diff**, matching the spec's own description ("an edited issue body is rare enough that the whole new body is one 'added' block under a simple diff, which is an acceptable approximation"): lines in the new snapshot not present in the old one (by count, so a removed-then-re-added identical line isn't double counted) are the diff, in original order.
3. **`RecordSnapshot` writes the full issue content at every existing `RecordSession` call site**, exactly mirroring the spec's "written alongside `RecordSession` at the same call sites" (spec §4). Since `issueContent` is constant for the lifetime of one `handleIssue` call, this means the snapshot file gets rewritten with the same bytes multiple times per run — harmless (it's a full overwrite, not an append) and it means the snapshot always reflects exactly what the paused session last saw, with no extra plumbing.
4. **Resuming the `"plan"` and `"execute"` stages needs neither the original spec path nor plan path.** Tracing the current code: `specPath` is only ever used to build the fresh `planPrompt(specPath)`, and `planPath` only to build the fresh `executePrompt(planPath)`. On resume, the prompt is replaced by the trigger prompt (`"continue"`/diff) instead, so neither path needs to be rediscovered. This lets the resume dispatcher for `"plan"`/`"execute"` skip straight to a `--resume` call with no new file-search logic.
5. **An unrecognized `Stage` value (corrupt/old data) falls back to the fresh pipeline entry point from *inside* `ResumeFeaturePipeline`/`ResumeBugPipeline`**, not by having `handleIssue` pre-inspect the stage. This keeps `handleIssue`'s decision genuinely binary ("does a session exist") per spec §1, and satisfies the spec's "safety net" fallback (§2) with one `switch`'s `default` case per pipeline.
6. **`finishDone`/`finishNeedsInfo` drop their now-dead `wtPath`/`branch` parameters** once their only uses (`wt.Remove`/`wt.DeleteBranch`) are deleted, rather than keeping unused parameters around. `ship` keeps both — it still uses them for `CommitCount`/`Push`.

---

## File Structure

| File | Change |
|---|---|
| `claude.go` | `SessionInfo.Stage` field; `RecordSession` gains a `stage` parameter; new `RecordSnapshot`/`readSnapshot` (mirroring `RecordSession`/`readSession`) for the issue-content snapshot. |
| `claude_test.go` | Update `RecordSession` call sites to the new 3-arg signature; add snapshot round-trip tests. |
| `resume.go` *(new)* | `loadResumableSession`, `resumePrompt`, `diffAddedLines` — the trigger-prompt logic, independent of `Orchestrator`. |
| `resume_test.go` *(new)* | Unit tests for `diffAddedLines` fixtures and `resumePrompt` trigger selection. |
| `tracker.go` | New `readState(logDir string) string` helper alongside the existing `recordState`/`clearState`. |
| `tracker_test.go` | Round-trip test for `readState`. |
| `pipeline_feature.go` | Extract `architectCall`, `brainstormLoop`; change `runPlanThenExecute`/`executePlan` to take a prompt+resume pair instead of a spec/plan path; add `ResumeFeaturePipeline`. Every `RecordSession` call site gets the new `stage` argument and an adjacent `RecordSnapshot` call. |
| `pipeline_feature_test.go` | Update `RecordSession`-derived assertions to check `Stage`; add `ResumeFeaturePipeline` tests per stage. |
| `pipeline_bug.go` | Extract `afterDebug`; add `ResumeBugPipeline`. |
| `pipeline_bug_test.go` | Same `Stage` assertion updates; add `ResumeBugPipeline` tests. |
| `loop.go` | `handleIssue` reads prior local state before overwriting it, dispatches to `Resume*Pipeline` when a session exists; `finishDone`/`finishNeedsInfo`/`ship` stop deleting worktree/branch. |
| `loop_test.go` | Update `finishDone`/`finishNeedsInfo`/`ship` regression tests to assert the worktree/branch survive; add resume-dispatch integration tests. |

---

### Task 1: `SessionInfo.Stage` and the new `RecordSession` signature

**Files:**
- Modify: `claude.go:238-262`
- Test: `claude_test.go:302-330`

**Interfaces:**
- Produces: `SessionInfo{SessionID, Kind, Stage string}`; `func (c *Claude) RecordSession(id, kind, stage string)`. Every later task's `RecordSession` calls use this 3-arg form.

- [ ] **Step 1: Write the failing test**

Update the two existing tests in `claude_test.go` to the 3-arg call and assert `Stage` round-trips:

```go
func TestRecordAndReadSession(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordSession("sess-123", "feature", "brainstorm")

	si, err := readSession(dir)
	if err != nil {
		t.Fatalf("readSession: %v", err)
	}
	if si.SessionID != "sess-123" || si.Kind != "feature" || si.Stage != "brainstorm" {
		t.Errorf("session = %+v, want sess-123/feature/brainstorm", si)
	}
}

func TestRecordSessionOverwritesAndSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordSession("first", "bug", "debug")
	c.RecordSession("", "bug", "debug") // empty id must not overwrite
	c.RecordSession("second", "bug", "debug")

	si, err := readSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	if si.SessionID != "second" {
		t.Errorf("session id = %q, want second (latest non-empty)", si.SessionID)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail to compile**

Run: `go test ./... -run TestRecordAndReadSession -v`
Expected: FAIL — `too many arguments in call to c.RecordSession`

- [ ] **Step 3: Update `SessionInfo` and `RecordSession`**

In `claude.go`, replace lines 238–262:

```go
// SessionInfo is persisted to <logDir>/session so the dashboard can show which
// Claude session did the work, and so a re-entry into the pipeline can resume
// it. It holds the latest primary session for the issue and the pipeline stage
// that session belongs to.
type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
	Stage     string `json:"stage"`
}

// Recognized SessionInfo.Stage values — the pipeline entry point a persisted
// session resumes into. Every recorded stage is a real Claude.Call site with a
// natural resume point (see Resume*Pipeline); there is no stage with none.
const (
	stageBrainstorm = "brainstorm"
	stagePlan       = "plan"
	stageExecute    = "execute"
	stageDebug      = "debug"
)

// RecordSession writes the latest primary working session id, pipeline kind,
// and pipeline stage for this issue to <logDir>/session. Best-effort, like the
// other log-writers: a no-op when logDir or id is empty, so an ephemeral
// answerer call (empty here because callers only invoke it for
// architect/debug/execute sessions) or a logless Claude never clobbers a
// recorded session.
func (c *Claude) RecordSession(id, kind, stage string) {
	if c.logDir == "" || id == "" {
		return
	}
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(SessionInfo{SessionID: id, Kind: kind, Stage: stage})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.logDir, sessionFile), b, 0o644)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestRecordAndReadSession|TestRecordSessionOverwritesAndSkipsEmpty|TestReadSessionMissing' -v`
Expected: PASS. (The package won't fully compile yet — other files still call the 2-arg `RecordSession`; that's fixed in Tasks 3–4. Use `go vet claude.go claude_test.go` if the full package build blocks this step, or proceed straight to Task 3 without an intermediate full-package test run.)

- [ ] **Step 5: Commit**

```bash
git add claude.go claude_test.go
git commit -m "feat: add Stage to SessionInfo and RecordSession"
```

---

### Task 2: Issue-content snapshot and the added-lines diff helper

**Files:**
- Modify: `claude.go` (add `RecordSnapshot`/`readSnapshot` near `RecordSession`/`readSession`)
- Create: `resume.go`
- Test: `claude_test.go` (snapshot round-trip), `resume_test.go` (new)

**Interfaces:**
- Consumes: nothing new.
- Produces: `func (c *Claude) RecordSnapshot(content string)`; `func readSnapshot(logDir string) (string, error)`; `func diffAddedLines(oldText, newText string) string`; `func resumePrompt(logDir, priorState, needsInfoLabel, content string) string`. Task 5/6 use `resumePrompt` and Tasks 3–4 use `RecordSnapshot`.

- [ ] **Step 1: Write the failing tests**

Append to `claude_test.go`:

```go
func TestRecordAndReadSnapshot(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordSnapshot("# Title (#7)\n\nbody text\n")

	got, err := readSnapshot(dir)
	if err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	if got != "# Title (#7)\n\nbody text\n" {
		t.Errorf("snapshot = %q", got)
	}
}

func TestRecordSnapshotSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordSnapshot("first")
	c.RecordSnapshot("") // empty content must not overwrite
	if got, _ := readSnapshot(dir); got != "first" {
		t.Errorf("snapshot = %q, want first", got)
	}
}

func TestReadSnapshotMissing(t *testing.T) {
	if _, err := readSnapshot(t.TempDir()); err == nil {
		t.Error("want error reading a missing snapshot file")
	}
}
```

Create `resume_test.go`:

```go
package main

import "testing"

func TestDiffAddedLinesNewCommentAppended(t *testing.T) {
	old := "# Title (#7)\n\nbody\n\n## Comments\n\n@alice: first comment\n"
	new_ := old + "\n@bob: second comment\n"
	got := diffAddedLines(old, new_)
	if got != "\n@bob: second comment\n" && got != "@bob: second comment\n" {
		t.Errorf("diff = %q, want just the new comment", got)
	}
}

func TestDiffAddedLinesBodyEdited(t *testing.T) {
	old := "# Title (#7)\n\noriginal body\n"
	new_ := "# Title (#7)\n\nedited body with more detail\n"
	got := diffAddedLines(old, new_)
	if got == "" {
		t.Fatal("an edited body must produce a non-empty diff")
	}
	if !contains(got, "edited body with more detail") {
		t.Errorf("diff = %q, want it to contain the edited line", got)
	}
}

func TestDiffAddedLinesNothingChanged(t *testing.T) {
	text := "# Title (#7)\n\nbody\n"
	if got := diffAddedLines(text, text); got != "" {
		t.Errorf("diff = %q, want empty for identical content", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i+len(substr) <= len(s); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}

func TestResumePromptDefaultsToContinue(t *testing.T) {
	logDir := t.TempDir()
	got := resumePrompt(logDir, "ai-rework", "ai-needs-info", "new content")
	if got != "continue" {
		t.Errorf("prompt = %q, want continue for a non-needs-info prior state", got)
	}
}

func TestResumePromptDiffsOnNeedsInfoReentry(t *testing.T) {
	logDir := t.TempDir()
	c := &Claude{logDir: logDir}
	c.RecordSnapshot("# Title (#7)\n\nbody\n")
	newContent := "# Title (#7)\n\nbody\n\n## Comments\n\n@alice: an answer\n"
	got := resumePrompt(logDir, "ai-needs-info", "ai-needs-info", newContent)
	if got == "continue" || !contains(got, "an answer") {
		t.Errorf("prompt = %q, want the diffed new comment", got)
	}
}

func TestResumePromptFallsBackToContinueOnEmptyDiff(t *testing.T) {
	logDir := t.TempDir()
	c := &Claude{logDir: logDir}
	c.RecordSnapshot("# Title (#7)\n\nbody\n")
	got := resumePrompt(logDir, "ai-needs-info", "ai-needs-info", "# Title (#7)\n\nbody\n")
	if got != "continue" {
		t.Errorf("prompt = %q, want continue when the label was removed with no new content", got)
	}
}

func TestResumePromptFallsBackToContinueOnMissingSnapshot(t *testing.T) {
	logDir := t.TempDir() // no snapshot recorded
	got := resumePrompt(logDir, "ai-needs-info", "ai-needs-info", "new content")
	if got != "continue" {
		t.Errorf("prompt = %q, want continue when there is no snapshot to diff against", got)
	}
}
```

(`contains` is a tiny local test helper — `strings.Contains` would normally be used directly in the assertions; it's spelled out here only because this step must be self-contained. When implementing, just use `strings.Contains` from the `strings` package and drop the local `contains` helper.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestRecordAndReadSnapshot|TestDiffAddedLines|TestResumePrompt' -v`
Expected: FAIL — `undefined: readSnapshot` / `undefined: diffAddedLines` / `undefined: resumePrompt`

- [ ] **Step 3: Implement `RecordSnapshot`/`readSnapshot` in `claude.go`**

Add directly below the existing `readSession` function:

```go
// snapshotFile holds the exact issue content (title + body + non-bot comments,
// as FetchIssueContent produces it) the pipeline last read. It lets a resumed
// session's prompt be built from what's NEW since the paused session saw the
// issue, rather than a bare "continue" — see resumePrompt in resume.go.
const snapshotFile = "issue-snapshot"

// RecordSnapshot writes the issue content this call site read to
// <logDir>/issue-snapshot, overwriting whatever was there. Best-effort, like
// RecordSession: a no-op on an empty logDir or content.
func (c *Claude) RecordSnapshot(content string) {
	if c.logDir == "" || content == "" {
		return
	}
	if err := os.MkdirAll(c.logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(c.logDir, snapshotFile), []byte(content), 0o644)
}

// readSnapshot reads the issue content written by RecordSnapshot from logDir.
func readSnapshot(logDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(logDir, snapshotFile))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
```

- [ ] **Step 4: Implement `resume.go`**

```go
package main

import "strings"

// loadResumableSession reports whether logDir holds a resumable session: a
// readable, non-empty SessionInfo. A missing or corrupt session file is never
// a hard error — it just means this is a first attempt, so handleIssue falls
// through to the fresh pipeline.
func loadResumableSession(logDir string) (SessionInfo, bool) {
	si, err := readSession(logDir)
	if err != nil || si.SessionID == "" {
		return SessionInfo{}, false
	}
	return si, true
}

// diffAddedLines returns the lines in newText that aren't in oldText, in
// newText's order, joined by newlines. It's a multiset line diff, not a real
// text diff: comments are append-only from the daemon's perspective, so a new
// comment shows up as its added lines; an edited issue body is rare enough
// that treating the whole new body as "added" (because none of its lines
// matched the old body byte-for-byte) is an acceptable approximation, per the
// design doc.
func diffAddedLines(oldText, newText string) string {
	remaining := map[string]int{}
	for _, l := range strings.Split(oldText, "\n") {
		remaining[l]++
	}
	var added []string
	for _, l := range strings.Split(newText, "\n") {
		if remaining[l] > 0 {
			remaining[l]--
			continue
		}
		added = append(added, l)
	}
	return strings.Join(added, "\n")
}

// resumePrompt builds the prompt for a resumed pipeline call (spec §4). An
// issue whose local state marker was ai-needs-info immediately before this
// re-entry gets the added-lines diff of the freshly fetched content against
// the snapshot the paused session last saw; every other re-entry (rework
// removed, dashboard Continue, orphan sweep) — and a needs-info re-entry with
// nothing new to show — gets the literal "continue".
func resumePrompt(logDir, priorState, needsInfoLabel, content string) string {
	if priorState != needsInfoLabel {
		return "continue"
	}
	old, err := readSnapshot(logDir)
	if err != nil {
		return "continue"
	}
	if diff := strings.TrimSpace(diffAddedLines(old, content)); diff != "" {
		return diffAddedLines(old, content)
	}
	return "continue"
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test . -run 'TestRecordAndReadSnapshot|TestRecordSnapshotSkipsEmpty|TestReadSnapshotMissing|TestDiffAddedLines|TestResumePrompt' -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add claude.go claude_test.go resume.go resume_test.go
git commit -m "feat: persist issue snapshots and diff them for needs-info resume prompts"
```

---

### Task 3: Refactor `pipeline_feature.go` for resume, add `ResumeFeaturePipeline`

**Files:**
- Modify: `pipeline_feature.go:26-165`
- Test: `pipeline_feature_test.go`

**Interfaces:**
- Consumes: `SessionInfo{SessionID, Kind, Stage}` (Task 1), `c.RecordSnapshot` (Task 2), `stageBrainstorm`/`stagePlan`/`stageExecute` constants (Task 1).
- Produces: `func ResumeFeaturePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT, session SessionInfo, prompt string) error`. Task 6 calls this from `handleIssue`.

- [ ] **Step 1: Write the failing tests**

Append to `pipeline_feature_test.go`:

```go
// TestResumeFeaturePipelineBrainstormStage resumes an architect session with
// --resume and the trigger prompt instead of calling brainstorm-0, then
// continues through the ordinary round loop to a fresh plan+execute.
func TestResumeFeaturePipelineBrainstormStage(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case c.stdin == "continue":
			if c.args[len(c.args)-1] != "arch-sess" {
				// --resume <id> is always the last two args; spot-check via ClaudeCall instead below.
			}
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-sess"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess"), "", nil
		default: // execute
			return claudeJSON("executed", "execute-sess"), "", nil
		}
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := featureConfig()
	session := SessionInfo{SessionID: "arch-sess", Kind: "feature", Stage: stageBrainstorm}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, wt, "the issue", "", nil, session, "continue"); err != nil {
		t.Fatal(err)
	}
	// The resumed architect call must carry --resume arch-sess and prompt "continue".
	found := false
	for _, call := range f.calls {
		if call.name == "claude" && call.stdin == "continue" && argAfter(call.args, "--resume") == "arch-sess" {
			found = true
		}
	}
	if !found {
		t.Error("want a claude call with --resume arch-sess and prompt \"continue\"")
	}
	si, err := readSession(logDir)
	if err != nil || si.SessionID != "execute-sess" || si.Stage != stageExecute {
		t.Errorf("session = %+v, err = %v, want execute-sess/execute after resuming through to execute", si, err)
	}
}

// TestResumeFeaturePipelinePlanStage resumes the plan session directly,
// skipping brainstorm entirely, then runs execute fresh.
func TestResumeFeaturePipelinePlanStage(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if argAfter(c.args, "--resume") == "plan-sess" {
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess-2"), "", nil
		}
		return claudeJSON("executed", "execute-sess"), "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := featureConfig()
	session := SessionInfo{SessionID: "plan-sess", Kind: "feature", Stage: stagePlan}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, wt, "the issue", "", nil, session, "continue"); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil || si.Stage != stageExecute {
		t.Errorf("session = %+v, err = %v, want stage execute", si, err)
	}
}

// TestResumeFeaturePipelineExecuteStage resumes the execute session directly
// with the trigger prompt.
func TestResumeFeaturePipelineExecuteStage(t *testing.T) {
	logDir := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if argAfter(c.args, "--resume") == "exec-sess" && c.stdin == "continue" {
			return claudeJSON("executed more", "exec-sess-2"), "", nil
		}
		return "", "unexpected call", fmt.Errorf("unexpected call: %+v", c)
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := featureConfig()
	session := SessionInfo{SessionID: "exec-sess", Kind: "feature", Stage: stageExecute}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, "/wt", "the issue", "", nil, session, "continue"); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil || si.SessionID != "exec-sess-2" || si.Stage != stageExecute {
		t.Errorf("session = %+v, err = %v, want exec-sess-2/execute", si, err)
	}
}

// TestResumeFeaturePipelineUnknownStageFallsBackToFresh is the safety net: a
// stage value that isn't one of the three known ones re-enters at brainstorm-0
// exactly like a fresh pipeline, rather than erroring.
func TestResumeFeaturePipelineUnknownStageFallsBackToFresh(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "brainstorming"):
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "fresh-arch"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess"), "", nil
		default:
			return claudeJSON("executed", "execute-sess"), "", nil
		}
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := featureConfig()
	session := SessionInfo{SessionID: "stale-sess", Kind: "feature", Stage: "bogus"}
	if err := ResumeFeaturePipeline(context.Background(), c, cfg, wt, "the issue", "", nil, session, "continue"); err != nil {
		t.Fatal(err)
	}
	// Fresh brainstorm-0 call must have fired with no --resume.
	for _, call := range f.calls {
		if call.name == "claude" && strings.Contains(call.stdin, "brainstorming") && argAfter(call.args, "--resume") != "" {
			t.Error("unknown stage must fall back to a FRESH brainstorm-0 call, not resume the stale session")
		}
	}
}
```

Check whether `featureConfig()` already exists in `pipeline_feature_test.go` (it's referenced at `pipeline_feature_test.go:480` in the current file) — reuse it; do not redefine.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run TestResumeFeaturePipeline -v`
Expected: FAIL — `undefined: ResumeFeaturePipeline`, `undefined: stageBrainstorm` (already defined by Task 1 — if Task 1 is committed first this part passes)

- [ ] **Step 3: Refactor `pipeline_feature.go`**

Replace lines 26–165 with:

```go
// RunFeaturePipeline drives three sessions: an architect brainstorm session
// (session A) that scores its confidence up front and, above the threshold,
// works with a sonnet product-owner proxy to a committed spec (SPEC_READY); a
// fresh plan session (session B) that turns the spec into a committed plan
// (PIPELINE_READY); and a fresh execute session (session C) that implements it.
// Below the confidence threshold it returns *lowConfidenceError without
// designing anything.
// Immediately after the spec is committed — before plan and execute — the
// non-blocking UAT step publishes a human-verifiable checklist onto the issue.
func RunFeaturePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT) error {
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
	return brainstormLoop(ctx, c, cfg, wtPath, issueContent, persona, uat, res.SessionID, res.Result, start)
}

// ResumeFeaturePipeline re-enters a persisted feature-pipeline session at its
// recorded stage, with --resume and the trigger prompt (spec §2), instead of
// starting a fresh brainstorm-0. An unrecognized stage — the only case with no
// natural resume point, expected to never happen in practice — falls back to a
// fully fresh RunFeaturePipeline as a safety net.
func ResumeFeaturePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT, session SessionInfo, prompt string) error {
	start := time.Now()
	switch session.Stage {
	case stagePlan:
		return runPlanThenExecute(ctx, c, cfg, wtPath, prompt, session.SessionID, start)
	case stageExecute:
		return executePlan(ctx, c, cfg, wtPath, prompt, session.SessionID)
	case stageBrainstorm:
		res, err := architectCall(ctx, c, cfg, wtPath, "brainstorm-resume", prompt, session.SessionID)
		if res != nil {
			c.RecordSession(res.SessionID, "feature", stageBrainstorm)
			c.RecordSnapshot(issueContent)
		}
		if err != nil {
			return err
		}
		return brainstormLoop(ctx, c, cfg, wtPath, issueContent, persona, uat, res.SessionID, res.Result, start)
	default:
		return RunFeaturePipeline(ctx, c, cfg, wtPath, issueContent, persona, uat)
	}
}

// architectCall runs one architect-model turn: the brainstorm-0/brainstorm-N
// call when resume is "", or a --resume turn (round call or resumed re-entry)
// when it isn't.
func architectCall(ctx context.Context, c *Claude, cfg *Config, wtPath, label, prompt, resume string) (*ClaudeResult, error) {
	return c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: label, Prompt: prompt, Resume: resume,
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
}

// brainstormLoop is the architect Q&A round loop shared by a fresh brainstorm-0
// call and a resumed brainstorm session: sessionID/output are the id and result
// text of whichever call preceded it (brainstorm-0, or the resume turn).
func brainstormLoop(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string, uat *UAT, sessionID, output string, start time.Time) error {
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
				return runPlanThenExecute(ctx, c, cfg, wtPath, planPrompt(specPath), "", start)
			}
		}

		var reply string
		donePushback := false
		if reason, ok := parseAlreadyDone(output); ok {
			// Architect claims already implemented — the answerer (PO proxy)
			// must confirm before we close. This confirmation is terminal, not a
			// bounded round.
			confirm, err := c.Call(ctx, ClaudeCall{
				Dir: wtPath, Label: fmt.Sprintf("done-confirm-%d", round),
				Prompt:          doneConfirmPrompt(issueContent, persona, reason),
				Model:           cfg.Models.Answerer,
				SkipPermissions: true,
			})
			if err != nil {
				return err
			}
			if strings.Contains(confirm.Result, doneConfirmSentinel) {
				return &alreadyDoneError{reason: reason}
			}
			reply = confirm.Result // objection; hand it back to the architect
			donePushback = true
		}

		// Sending a reply to the architect is a bounded Q&A round.
		if round > cfg.MaxQARounds {
			return fmt.Errorf("feature pipeline: exceeded %d Q&A rounds without a completed spec", cfg.MaxQARounds)
		}
		if !donePushback {
			ans, err := c.Call(ctx, ClaudeCall{
				Dir: wtPath, Label: fmt.Sprintf("answer-%d", round),
				Prompt:          answererPrompt(issueContent, persona, output),
				Model:           cfg.Models.Answerer,
				SkipPermissions: true,
			})
			if err != nil {
				return err
			}
			reply = ans.Result
		}

		res, err := architectCall(ctx, c, cfg, wtPath, fmt.Sprintf("brainstorm-%d", round), reply, sessionID)
		if res != nil {
			c.RecordSession(res.SessionID, "feature", stageBrainstorm)
		}
		if err != nil {
			return err
		}
		output = res.Result
	}
}

// runPlanThenExecute runs the plan session (session B) that turns the approved
// spec into a committed plan, then executes it (session C). Both are fresh
// sessions on a first pass — the plan session must not carry brainstorm
// context — but the SAME entry point serves a resumed plan session too: resume
// is "" on a fresh call (prompt is planPrompt(specPath)) and the persisted plan
// session's id when re-entering (prompt is the trigger prompt instead).
func runPlanThenExecute(ctx context.Context, c *Claude, cfg *Config, wtPath, prompt, resume string, start time.Time) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "plan", Prompt: prompt, Resume: resume,
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if res != nil {
		c.RecordSession(res.SessionID, "feature", stagePlan)
	}
	if err != nil {
		return err
	}
	if !strings.Contains(res.Result, readySentinel) {
		return fmt.Errorf("feature pipeline: plan session did not signal %s", readySentinel)
	}
	plan, ok := findPlanFile(wtPath, start)
	if !ok {
		return fmt.Errorf("feature pipeline: plan session signaled %s but wrote no plan file", readySentinel)
	}
	return executePlan(ctx, c, cfg, wtPath, executePrompt(plan), "")
}

// executePlan runs the execute session (session C), fresh (resume == "", prompt
// built from the just-written plan file) or resumed (resume is the persisted
// execute session's id, prompt is the trigger prompt).
func executePlan(ctx context.Context, c *Claude, cfg *Config, wtPath, prompt, resume string) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "execute", Prompt: prompt, Resume: resume,
		Model:           cfg.Models.executeConfig(),
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if res != nil {
		c.RecordSession(res.SessionID, "feature", stageExecute)
	}
	if err != nil {
		return err
	}
	return nil
}
```

Leave `parseSpecReady`, `findSpecFile`, `resolveSpec`, `findPlanFile`, `readPersona`, and the `*Prompt` builder functions (lines 166–303 of the original file) untouched.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestResumeFeaturePipeline|TestFeaturePipeline|TestRunFeaturePipeline' -v`
Expected: PASS — including every pre-existing `RunFeaturePipeline` test (the refactor must not change its externally observable behavior).

Then run the full package to catch any other breakage from the refactor:

Run: `go build ./...`
Expected: fails only on the two remaining 2-arg `c.RecordSession(...)` calls in `pipeline_bug.go` (fixed in Task 4).

- [ ] **Step 5: Commit**

```bash
git add pipeline_feature.go pipeline_feature_test.go
git commit -m "feat: add ResumeFeaturePipeline, resuming brainstorm/plan/execute in place"
```

---

### Task 4: Refactor `pipeline_bug.go` for resume, add `ResumeBugPipeline`

**Files:**
- Modify: `pipeline_bug.go`
- Test: `pipeline_bug_test.go`

**Interfaces:**
- Consumes: `stageDebug` constant (Task 1), `c.RecordSnapshot` (Task 2).
- Produces: `func ResumeBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT, session SessionInfo, prompt string) error`. Task 6 calls this from `handleIssue`.

- [ ] **Step 1: Write the failing tests**

Update the two existing `RecordSession` assertions in `pipeline_bug_test.go` (`TestBugPipelineRecordsSession`, `TestBugPipelineRecordsSessionOnError`) to also check `Stage == stageDebug`:

```go
	if si.SessionID != "debug-sess" || si.Kind != "bug" || si.Stage != stageDebug {
		t.Errorf("session = %+v, want debug-sess/bug/debug", si)
	}
```

(and similarly for the `debug-429` test). Append new tests:

```go
// TestResumeBugPipelineReentersWithResumeAndPrompt verifies the resumed call
// carries --resume <id> and the trigger prompt instead of bugPrompt, and still
// runs the confidence gate / already-done check / UAT on the resumed result.
func TestResumeBugPipelineReentersWithResumeAndPrompt(t *testing.T) {
	logDir := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if argAfter(c.args, "--resume") == "debug-sess" && c.stdin == "continue" {
			return claudeJSON("Fixed and committed.", "debug-sess-2"), "", nil
		}
		return "", "unexpected call", fmt.Errorf("unexpected call: %+v", c)
	}}
	c := &Claude{runner: f, logDir: logDir}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}}
	session := SessionInfo{SessionID: "debug-sess", Kind: "bug", Stage: stageDebug}
	if err := ResumeBugPipeline(context.Background(), c, cfg, "/wt", "the issue", "main", nil, session, "continue"); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil || si.SessionID != "debug-sess-2" || si.Stage != stageDebug {
		t.Errorf("session = %+v, err = %v, want debug-sess-2/debug", si, err)
	}
}

// TestResumeBugPipelineLowConfidenceEscalates verifies the confidence gate still
// runs against the resumed session's output.
func TestResumeBugPipelineLowConfidenceEscalates(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("CONFIDENCE: 20\nstill unclear", "debug-sess-2")}}}
	c := &Claude{runner: f}
	cfg := &Config{Models: Models{Architect: ModelConfig{Model: "opus"}}, ConfidenceThreshold: 70}
	session := SessionInfo{SessionID: "debug-sess", Kind: "bug", Stage: stageDebug}
	err := ResumeBugPipeline(context.Background(), c, cfg, "/wt", "ISSUE", "main", nil, session, "continue")
	var lc *lowConfidenceError
	if !errors.As(err, &lc) {
		t.Fatalf("want *lowConfidenceError, got %v", err)
	}
}
```

Add `"errors"` to the test file's imports if not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestResumeBugPipeline|TestBugPipelineRecordsSession' -v`
Expected: FAIL — `undefined: ResumeBugPipeline`

- [ ] **Step 3: Refactor `pipeline_bug.go`**

Replace the whole file body (keep the package/import lines) with:

```go
// RunBugPipeline drives one systematic-debugging session, gated on confidence
// and the already-done claim. base is the base branch: on the outcome where a
// fix was actually produced, the non-blocking UAT step diffs against
// origin/<base> to build a human-verifiable checklist for the issue body.
func RunBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "debug", Prompt: bugPrompt(issueContent, cfg.ConfidenceThreshold),
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	// Record before the error check: an errored call (e.g. a 429 session limit)
	// still returns a session id, and the dashboard shows it on the parked ticket.
	if res != nil {
		c.RecordSession(res.SessionID, "bug", stageDebug)
		c.RecordSnapshot(issueContent)
	}
	if err != nil {
		return err
	}
	return afterDebug(ctx, c, cfg, wtPath, issueContent, base, uat, res.Result)
}

// ResumeBugPipeline re-enters a persisted debug session with --resume and the
// trigger prompt instead of the fresh bugPrompt (spec §2). "debug" is the only
// stage a bug pipeline ever records, so there's no stage switch here — an
// unrecognized SessionInfo.Stage can't reach this function (handleIssue only
// calls it for session.Kind == "bug", and every bug-pipeline RecordSession call
// uses stageDebug).
func ResumeBugPipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT, session SessionInfo, prompt string) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "debug-resume", Prompt: prompt, Resume: session.SessionID,
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if res != nil {
		c.RecordSession(res.SessionID, "bug", stageDebug)
		c.RecordSnapshot(issueContent)
	}
	if err != nil {
		return err
	}
	return afterDebug(ctx, c, cfg, wtPath, issueContent, base, uat, res.Result)
}

// afterDebug runs the confidence gate, already-done check, and (if a fix was
// actually produced) the UAT step against a debug session's output — shared by
// the fresh and resumed entry points.
func afterDebug(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, base string, uat *UAT, output string) error {
	// Confidence gate, shared with the feature route: same threshold, sentinel,
	// parser and terminal outcome. It runs before the already-done check on
	// purpose — a session too unsure to fix the bug must not get to close the
	// issue as already implemented instead. A threshold <= 0 disables it, and an
	// unparseable score fails open so a session that forgot the sentinel but
	// fixed the bug still ships.
	if cfg.ConfidenceThreshold > 0 {
		if score, ok := parseConfidence(output); ok && score < cfg.ConfidenceThreshold {
			return &lowConfidenceError{score: score, feedback: sanitizeFeedback(output)}
		}
	}
	if reason, ok := parseAlreadyDone(output); ok {
		return &alreadyDoneError{reason: reason}
	}
	// Only this outcome produced a fix — neither the needs-info nor the
	// already-done return above reaches here, so neither publishes a checklist.
	uat.RunBug(ctx, c, cfg, wtPath, issueContent, base)
	return nil
}

func bugPrompt(issue string, threshold int) string {
	d := promptData()
	d["Issue"] = issue
	d["Threshold"] = threshold
	return mustRender("debug.md.tmpl", d)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestResumeBugPipeline|TestBugPipeline' -v`
Expected: PASS

Then confirm the whole package builds:

Run: `go build ./...`
Expected: succeeds (all `RecordSession` call sites now use the 3-arg form).

Run: `go test ./...`
Expected: PASS for everything except the tests Tasks 6–7 haven't updated yet (`loop.go`'s `handleIssue`/`finishDone`/`finishNeedsInfo`/`ship` are unmodified so far, so this should already be green).

- [ ] **Step 5: Commit**

```bash
git add pipeline_bug.go pipeline_bug_test.go
git commit -m "feat: add ResumeBugPipeline, resuming the debug session in place"
```

---

### Task 5: `readState` helper

**Files:**
- Modify: `tracker.go:189-197` (add helper immediately after `recordState`)
- Test: `tracker_test.go`

**Interfaces:**
- Produces: `func readState(logDir string) string`. Task 6 uses it to peek at an issue's local state marker before `handleIssue` overwrites it with `ai-wip`.

- [ ] **Step 1: Write the failing test**

Add to `tracker_test.go`:

```go
func TestReadState(t *testing.T) {
	dir := t.TempDir()
	if got := readState(dir); got != "" {
		t.Errorf("readState on an empty dir = %q, want empty", got)
	}
	recordState(dir, "ai-needs-info")
	if got := readState(dir); got != "ai-needs-info" {
		t.Errorf("readState = %q, want ai-needs-info", got)
	}
	clearState(dir)
	if got := readState(dir); got != "" {
		t.Errorf("readState after clearState = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestReadState -v`
Expected: FAIL — `undefined: readState`

- [ ] **Step 3: Implement `readState`**

In `tracker.go`, immediately after `recordState` (after line 197):

```go
// readState returns the issue's local state marker (e.g. "ai-needs-info"), or
// "" if none is recorded. Used by handleIssue to learn what state an issue was
// last in — and so which resume-prompt strategy applies (spec §4) — BEFORE it
// overwrites the marker with ai-wip for the new attempt.
func readState(logDir string) string {
	b, err := os.ReadFile(filepath.Join(logDir, stateFile))
	if err != nil {
		return ""
	}
	return string(b)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestReadState -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add tracker.go tracker_test.go
git commit -m "feat: add readState to peek at an issue's local state marker"
```

---

### Task 6: Wire resume dispatch into `handleIssue`

**Files:**
- Modify: `loop.go:222-270`
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `loadResumableSession` (Task 2/resume.go), `resumePrompt` (Task 2), `readState` (Task 5), `ResumeFeaturePipeline`/`ResumeBugPipeline` (Tasks 3–4).

- [ ] **Step 1: Write the failing tests**

Append to `loop_test.go`:

```go
// TestHandleIssueResumesPersistedFeatureSession simulates a rework-then-removed
// re-entry: a first cycle parks the issue with a recorded brainstorm session,
// then a second cycle (the label removed, same worktree/logs preserved) must
// call the architect with --resume and prompt "continue" instead of
// brainstorm-0.
func TestHandleIssueResumesPersistedFeatureSession(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" && strings.Contains(c.stdin, "triage agent") {
			return claudeJSON(`{"issueNumber": 7, "kind": "feature", "reason": "needs design"}`, "t1"), "", nil
		}
		if c.name == "claude" && strings.Contains(c.stdin, "brainstorming") {
			// First (fresh) attempt fails outright so the issue parks with a
			// recorded session and the worktree preserved.
			return "", "boom", fmt.Errorf("exit 1")
		}
		return base(c)
	}
	o := env.orchestrator()
	if err := runCycle(o); err != nil {
		t.Fatalf("park is a clean cycle outcome, got %v", err)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) == 0 {
		t.Fatal("setup: first attempt must park as ai-rework")
	}

	// Second cycle: the architect now resumes instead of failing, and the fake
	// triage still returns the same issue as eligible (no state-label filtering
	// in this harness, mirroring "label removed -> eligible again").
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" && strings.Contains(c.stdin, "triage agent") {
			return claudeJSON(`{"issueNumber": 7, "kind": "feature", "reason": "resumed"}`, "t1"), "", nil
		}
		return base(c)
	}
	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	var resumed bool
	for _, call := range env.f.calls {
		if call.name == "claude" && call.stdin == "continue" && argAfter(call.args, "--resume") != "" {
			resumed = true
		}
	}
	if !resumed {
		t.Error("second attempt must resume the architect session with --resume and prompt \"continue\", not restart brainstorm-0")
	}
	// The park-then-resume cycle must never have deleted the worktree.
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("resuming must never have deleted the worktree along the way")
	}
}

// TestHandleIssueResumesWithDiffAfterNeedsInfo simulates the ai-needs-info
// answered trigger: the first cycle escalates to needs-info (recording the
// snapshot and a brainstorm session); the second cycle, with a new human
// comment in the fetched issue content, must resume with a prompt containing
// the diffed comment, not the bare literal "continue".
func TestHandleIssueResumesWithDiffAfterNeedsInfo(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" && strings.Contains(c.stdin, "triage agent") {
			return claudeJSON(`{"issueNumber": 7, "kind": "feature", "reason": "needs design"}`, "t1"), "", nil
		}
		if c.name == "claude" && strings.Contains(c.stdin, "brainstorming") {
			return claudeJSON("CONFIDENCE: 30\nNo acceptance criteria — what should the export contain?", "arch-1"), "", nil
		}
		return base(c)
	}
	o := env.orchestrator()
	o.cfg.ConfidenceThreshold = 70
	if err := runCycle(o); err != nil {
		t.Fatalf("needs-info is a clean outcome, want nil error, got %v", err)
	}

	// Second cycle: the issue now carries a human's answer in its comments, and
	// the architect resumes instead of scoring low again.
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.HasPrefix(joined, "issue view") {
			return `{"title": "Fix crash", "body": "boom", "comments": [{"author": {"login": "alice"}, "body": "export should include CSV rows only"}]}`, "", nil
		}
		if c.name == "claude" && strings.Contains(c.stdin, "triage agent") {
			return claudeJSON(`{"issueNumber": 7, "kind": "feature", "reason": "answered"}`, "t1"), "", nil
		}
		return base(c)
	}
	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	var diffPrompt string
	for _, call := range env.f.calls {
		if call.name == "claude" && argAfter(call.args, "--resume") == "arch-1" {
			diffPrompt = call.stdin
		}
	}
	if diffPrompt == "" {
		t.Fatal("want a resumed architect call with --resume arch-1")
	}
	if diffPrompt == "continue" || !strings.Contains(diffPrompt, "export should include CSV rows only") {
		t.Errorf("resume prompt = %q, want the diffed new comment, not a bare continue", diffPrompt)
	}
}

// TestHandleIssueNoSessionUsesFreshPath is the control: an issue with no
// session file at all (first-ever attempt) must call brainstorm-0/debug
// exactly as today, with no --resume anywhere.
func TestHandleIssueNoSessionUsesFreshPath(t *testing.T) {
	env := newFakeEnv(t)
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	for _, call := range env.f.calls {
		if call.name == "claude" && argAfter(call.args, "--resume") != "" {
			t.Errorf("a first-ever attempt must never use --resume, got call: %+v", call)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestHandleIssueResumes|TestHandleIssueNoSessionUsesFreshPath' -v`
Expected: FAIL — resumed calls never happen; the second cycle still calls `brainstorm-0` fresh.

- [ ] **Step 3: Wire the dispatch in `loop.go`**

Replace `handleIssue` (lines 222–270) with:

```go
func (o *Orchestrator) handleIssue(ctx context.Context, issue Issue, kind, base string) error {
	n := issue.Number
	branch := branchName(n)
	logDir := o.issueLogDir(n)
	// Captured BEFORE recordState below overwrites it with ai-wip: this is the
	// state the issue was in immediately prior to this re-entry (e.g.
	// ai-needs-info, if a human's answer is what re-queued it), which decides
	// the resume-prompt strategy a few lines down.
	priorState := readState(logDir)
	if err := o.gh.AddLabel(ctx, n, o.cfg.StateLabels.WIP); err != nil {
		return err
	}
	recordState(logDir, o.cfg.StateLabels.WIP)
	// Mirror the title next to the state marker: the dashboard otherwise knows
	// it only for as long as the issue keeps matching its label-scoped query.
	recordTitle(logDir, issue.Title)
	_ = o.gh.Comment(ctx, n, pickupComment(kind, branch))

	wtPath, err := o.wt.Create(ctx, o.cfg.WorkDir, n, base)
	if err != nil {
		return o.abort(ctx, n, err)
	}
	content, err := o.gh.FetchIssueContent(ctx, n)
	if err != nil {
		return o.abort(ctx, n, err)
	}
	content = DownloadIssueImages(ctx, o.runner, content, logDir)

	c := &Claude{runner: o.runner, logDir: logDir, configDir: o.cfg.ClaudeConfigDir}
	uat := &UAT{Target: o.gh, Num: n}
	persona := readPersona(o.cfg.PersonaPath)
	var perr error
	if session, ok := loadResumableSession(logDir); ok {
		// A session already exists for this ticket's worktree — every re-entry
		// trigger (rework removed, needs-info answered, dashboard Continue, a
		// daemon-restart requeue via SweepOrphans) converges on this same check,
		// so there is no separate code path per trigger (spec §1).
		prompt := resumePrompt(logDir, priorState, o.cfg.StateLabels.NeedsInfo, content)
		if session.Kind == "bug" {
			perr = ResumeBugPipeline(ctx, c, o.cfg, wtPath, content, base, uat, session, prompt)
		} else {
			perr = ResumeFeaturePipeline(ctx, c, o.cfg, wtPath, content, persona, uat, session, prompt)
		}
	} else if kind == "bug" {
		perr = RunBugPipeline(ctx, c, o.cfg, wtPath, content, base, uat)
	} else {
		perr = RunFeaturePipeline(ctx, c, o.cfg, wtPath, content, persona, uat)
	}
	// A Stop landed during the pipeline: skip the normal park/ship/finish outcome
	// and leave the ticket ai-wip. The launching goroutine's consumeStopping+pause
	// transitions it to ai-stopped on the live parent ctx.
	if o.isStopping(n) {
		return nil
	}
	var done *alreadyDoneError
	if errors.As(perr, &done) {
		return o.finishDone(ctx, n, done.reason)
	}
	var lowConf *lowConfidenceError
	if errors.As(perr, &lowConf) {
		return o.finishNeedsInfo(ctx, n, lowConf)
	}
	if perr != nil {
		return o.park(ctx, n, perr)
	}
	return o.ship(ctx, issue, wtPath, branch, base, kind)
}
```

Note `finishDone`/`finishNeedsInfo` calls above already use the Task 7 signature (dropped `wtPath, branch`) — Task 7 lands that change; if Task 6 is executed before Task 7 in a differently-ordered run, keep the old 5-arg calls (`o.finishDone(ctx, n, wtPath, branch, done.reason)` / `o.finishNeedsInfo(ctx, n, wtPath, branch, lowConf)`) until Task 7 lands, to keep every intermediate commit compiling. This plan's task order (6 then 7) means Task 6 should temporarily keep the old signatures; **do this instead in Step 3 above**:

```go
	var done *alreadyDoneError
	if errors.As(perr, &done) {
		return o.finishDone(ctx, n, wtPath, branch, done.reason)
	}
	var lowConf *lowConfidenceError
	if errors.As(perr, &lowConf) {
		return o.finishNeedsInfo(ctx, n, wtPath, branch, lowConf)
	}
```

(Task 7 then trims these two call sites down to the 3-arg form in the same edit that drops the parameters.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestHandleIssueResumes|TestHandleIssueNoSessionUsesFreshPath' -v`
Expected: PASS

Run the full suite to confirm no regressions:

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add loop.go loop_test.go
git commit -m "feat: resume a persisted session in handleIssue instead of restarting the pipeline"
```

---

### Task 7: Stop deleting worktrees/branches in `finishDone`, `finishNeedsInfo`, `ship`

**Files:**
- Modify: `loop.go:222-270` (the two `finishDone`/`finishNeedsInfo` call sites, trimmed to 3 args), `loop.go:278-318` (`finishDone`/`finishNeedsInfo` bodies), `loop.go:471-503` (`ship`)
- Test: `loop_test.go` (regression tests for all three, per spec's Testing section)

**Interfaces:**
- Produces: `func (o *Orchestrator) finishDone(ctx context.Context, n int, reason string) error`; `func (o *Orchestrator) finishNeedsInfo(ctx context.Context, n int, lc *lowConfidenceError) error` (both drop `wtPath, branch`). `ship`'s signature is unchanged.

- [ ] **Step 1: Write the failing tests**

Update `TestProcessOnceLowConfidenceEscalatesToNeedsInfo` (loop_test.go:90-137): change the trailing assertion from "worktree was removed" to "worktree is preserved":

```go
	// Worktree is preserved (never-delete): a human answering needs-info must
	// resume into it, not restart from zero.
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("needs-info path must preserve the worktree, not remove it")
	}
```

Update `TestProcessOnceAlreadyDoneClosesIssue` (loop_test.go:392-426): change the "Worktree was created ... then cleaned up" assertion:

```go
	// Worktree is preserved (never-delete), even for a closed/already-done issue.
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("already-done path must preserve the worktree, not remove it")
	}
```

Update `TestProcessOnceHappyPathBug` (loop_test.go:191-224): drop `{"git", "worktree remove"}` from the `want` table (a shipped issue no longer removes its worktree) and add an explicit negative assertion:

```go
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Error("a shipped issue must preserve its worktree, not remove it")
	}
```

Add a new regression test asserting the branch itself also survives `finishDone`/`finishNeedsInfo` (there was no assertion on `branch -D` for these two paths before):

```go
// TestFinishDoneAndNeedsInfoPreserveBranch is a direct regression test for the
// spec's "never delete" rule: finishDone and finishNeedsInfo must not delete
// the branch any more than they delete the worktree.
func TestFinishDoneAndNeedsInfoPreserveBranch(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "claude" && !strings.Contains(c.stdin, "triage agent") {
			return claudeJSON("PIPELINE_ALREADY_DONE: already in place", "d1"), "", nil
		}
		return base(c)
	}
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("git", "branch -D")) != 0 {
		t.Error("finishDone must not delete the branch")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestProcessOnceLowConfidenceEscalatesToNeedsInfo|TestProcessOnceAlreadyDoneClosesIssue|TestProcessOnceHappyPathBug|TestFinishDoneAndNeedsInfoPreserveBranch' -v`
Expected: FAIL — `worktree remove`/`branch -D` calls are still present.

- [ ] **Step 3: Stop deleting in `finishDone`, `finishNeedsInfo`, `ship`**

In `loop.go`, update the two call sites inside `handleIssue` (trim to 3 args, completing the note from Task 6 Step 3):

```go
	var done *alreadyDoneError
	if errors.As(perr, &done) {
		return o.finishDone(ctx, n, done.reason)
	}
	var lowConf *lowConfidenceError
	if errors.As(perr, &lowConf) {
		return o.finishNeedsInfo(ctx, n, lowConf)
	}
```

Replace `finishDone` (lines 278–293):

```go
// finishDone closes an issue a pipeline judged already implemented. It runs on
// the handleIssue path, so ai-wip is already applied: comment the reason, swap
// WIP->Done, and close the issue. The worktree and branch are left in place
// (never deleted — spec Decision 3), same as every other terminal outcome. Uses
// a cancellation-proof context so a Ctrl-C still finishes cleanup and labeling.
// The Done label is swapped in before the close, so even if the close fails the
// issue is de-queued (hasStateLabel) and won't be re-picked.
func (o *Orchestrator) finishDone(ctx context.Context, n int, reason string) error {
	cctx := context.WithoutCancel(ctx)
	_ = o.gh.Comment(cctx, n, alreadyDoneComment(reason))
	if err := o.gh.SwapLabels(cctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Done); err != nil {
		return fmt.Errorf("issue #%d: already implemented but marking done failed: %w", n, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Done)
	clearParkCause(o.issueLogDir(n))
	return o.gh.CloseIssue(cctx, n)
}
```

Replace `finishNeedsInfo` (lines 295–318, keep the doc comment's substance but drop the now-false worktree/branch removal claim):

```go
// finishNeedsInfo escalates an issue the brainstorm session judged too
// under-specified to implement. Modeled on finishDone: comment the score and
// the architect's questions, swap WIP->NeedsInfo, and record state. It does
// NOT close the issue: it waits out of the queue until a human removes the
// needs-info label, which re-queues it — into the SAME worktree and branch,
// left untouched here (never deleted — spec Decision 3), so the answer resumes
// the paused session instead of starting over. Returns nil: escalation is a
// clean terminal outcome, not a pipeline failure. Uses a cancellation-proof
// context so a Ctrl-C mid-pipeline still records the state.
func (o *Orchestrator) finishNeedsInfo(ctx context.Context, n int, lc *lowConfidenceError) error {
	cctx := context.WithoutCancel(ctx)
	_ = o.gh.Comment(cctx, n, needsInfoComment(lc.score, o.cfg.StateLabels.NeedsInfo, lc.feedback))
	if err := o.gh.SwapLabels(cctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.NeedsInfo); err != nil {
		return fmt.Errorf("issue #%d: low confidence but marking needs-info failed: %w", n, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.NeedsInfo)
	clearParkCause(o.issueLogDir(n))
	return nil
}
```

Update `ship` (lines 471–503) — remove both `wt.Remove` calls:

```go
// ship pushes the branch, opens (or recovers) the PR, comments the URL, and
// swaps WIP->Done. A deterministic tooling failure here (commit count, push, PR
// create) happens AFTER the pipeline has already produced commits, so it parks
// for rework — preserving the worktree, branch, and session, so a human who
// removes the label gets a run that builds on those commits instead of
// re-running the whole pipeline from zero. A pipeline that produced no commits
// also parks. The worktree and branch are never removed here either (spec
// Decision 3): a shipped issue's worktree sits on disk permanently, same as a
// parked or stopped one already does — an accepted, explicit trade-off with no
// cleanup mechanism in scope. Returns nil only when fully shipped.
func (o *Orchestrator) ship(ctx context.Context, issue Issue, wtPath, branch, base, kind string) error {
	n := issue.Number
	onInfra := func(err error) error {
		return o.park(ctx, n, err)
	}
	count, err := o.wt.CommitCount(ctx, wtPath, base)
	if err != nil {
		return onInfra(err)
	}
	if count == 0 {
		return o.park(ctx, n, errors.New("pipeline finished but produced no commits"))
	}
	if err := o.wt.Push(ctx, wtPath, branch); err != nil {
		return onInfra(err)
	}
	url, err := o.gh.CreatePR(ctx, branch, prTitle(issue.Title, n), prBody(n, kind))
	if err != nil {
		return onInfra(err)
	}
	_ = o.gh.Comment(ctx, n, prComment(url))
	recordPR(o.issueLogDir(n), url)
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

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestProcessOnceLowConfidenceEscalatesToNeedsInfo|TestProcessOnceAlreadyDoneClosesIssue|TestProcessOnceHappyPathBug|TestFinishDoneAndNeedsInfoPreserveBranch|TestFinishDoneUsesConfiguredDoneLabel|TestDoneSwapFailureIsSurfaced|TestParkWritesCauseAndShipClearsIt' -v`
Expected: PASS

Run the full suite:

Run: `go test ./...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add loop.go loop_test.go
git commit -m "fix: stop deleting worktrees/branches on finishDone, finishNeedsInfo, and ship"
```

---

### Task 8: Daemon-restart auto-resume regression test

**Files:**
- Test: `loop_test.go`

**Interfaces:**
- Consumes: `SweepOrphans` (existing, unchanged), the resume dispatch from Task 6.

This task adds the one piece of coverage spec §5 calls out explicitly and that isn't implied by Task 6's tests: `SweepOrphans` needs zero new logic, but the plan should still prove that a session persisted before a crash gets picked up automatically by the very next cycle after the sweep — no separate "was this a restart" signal anywhere.

- [ ] **Step 1: Write the failing test**

Append to `loop_test.go`:

```go
// TestSweepOrphansThenNextCycleResumesSession is the daemon-restart case (spec
// §5): an issue stranded in ai-wip by a crash, with a session already recorded
// from before the crash, must have that session resumed on the very next cycle
// after SweepOrphans requeues it — with no bespoke "was this a restart" signal
// anywhere, just the ordinary loadResumableSession check in handleIssue.
func TestSweepOrphansThenNextCycleResumesSession(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()

	// Simulate a crash: ai-wip is set, and a brainstorm session was recorded
	// before the process died mid-pipeline.
	n := 7
	if err := o.gh.AddLabel(context.Background(), n, o.cfg.StateLabels.WIP); err != nil {
		t.Fatal(err)
	}
	logDir := o.issueLogDir(n)
	c := &Claude{logDir: logDir}
	c.RecordSession("stranded-sess", "feature", stageBrainstorm)
	c.RecordSnapshot("# Fix crash (#7)\n\nboom\n")

	if err := o.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if len(env.callsMatching("gh", "--remove-label ai-wip")) == 0 {
		t.Fatal("setup: SweepOrphans must strip the stale ai-wip label")
	}

	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	var resumed bool
	for _, call := range env.f.calls {
		if call.name == "claude" && argAfter(call.args, "--resume") == "stranded-sess" {
			resumed = true
		}
	}
	if !resumed {
		t.Error("the next cycle after a sweep must resume the crash-stranded session, not restart brainstorm-0")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestSweepOrphansThenNextCycleResumesSession -v`
Expected: FAIL if Task 6 is somehow incomplete; otherwise this should already PASS immediately since no new production code is needed — it is asserting the emergent behavior of Task 6's dispatch. If it fails, that means Task 6's wiring has a gap (e.g. `SweepOrphans`/`clearState` clearing something Task 6 depends on) — treat a failure here as a bug in Task 6, not something to patch locally.

- [ ] **Step 3: If it fails, diagnose against Task 6's wiring; if it passes, no production code changes are needed here**

`SweepOrphans` calls `clearState(logDir)` and `clearParkCause(logDir)` when requeueing (loop.go:456-460) — confirm neither of those touches the `session` or `issue-snapshot` files (they don't; they only remove `state` and `park-cause`). If the test fails, the most likely cause is `readState` returning something that defeats `resumePrompt`'s needs-info check, or `loadResumableSession` not finding the session — inspect `logDir` contents in the failing test with `t.Logf` before assuming a deeper bug.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestSweepOrphansThenNextCycleResumesSession -v`
Expected: PASS

Run the full suite one more time:

Run: `go test ./... -race`
Expected: PASS (the `-race` flag matches the existing convention for the multi-goroutine `TestProcessOnceHandlesMultipleTickets`; running it here is a cheap final sanity check that nothing in this feature introduced a data race around the new file reads/writes under `logDir`).

- [ ] **Step 5: Commit**

```bash
git add loop_test.go
git commit -m "test: cover daemon-restart auto-resume via SweepOrphans"
```

---

## Self-Review Notes

**Spec coverage:**
- §1 (one decision point) → Task 6.
- §2 (Stage field, per-stage resume dispatch) → Tasks 1, 3, 4.
- §3 (stop deleting worktrees/branches) → Task 7.
- §4 (resume prompt: continue vs. needs-info diff, snapshot file) → Tasks 2, 5, 6.
- §5 (daemon-restart auto-resume via existing SweepOrphans, no new logic) → Task 8 (test-only; confirms no new code was needed).
- Error handling (readSession failure → fresh path; `--resume` failure → normal park) → Task 2's `loadResumableSession`/`resumePrompt` (fresh-path fallback) and Task 6/7 (no special-casing added to the park path — an errored resume call flows through the exact same `perr != nil → o.park` branch as every other failure).
- Testing section → every bullet maps to a task: `SessionInfo`/`Stage` round-trip (Task 1), snapshot-diff fixtures (Task 2), `handleIssue` dispatch by stage (Tasks 3, 4, 6), park→resume and needs-info→diff integration tests (Task 6), `finishDone`/`finishNeedsInfo`/`ship` regression tests (Task 7).

**Placeholder scan:** none — every step has literal code, not a description of code.

**Type consistency:** `SessionInfo{SessionID, Kind, Stage}` (Task 1) is used identically in Tasks 3, 4, 6, 8. `RecordSession(id, kind, stage string)` (Task 1) is called with matching argument order everywhere it's used (Tasks 3, 4). `resumePrompt(logDir, priorState, needsInfoLabel, content string) string` (Task 2) is called from `handleIssue` (Task 6) with `o.cfg.StateLabels.NeedsInfo` as `needsInfoLabel`, matching the label already in scope there. `finishDone`/`finishNeedsInfo`'s trimmed 3-/2-arg signatures (Task 7) match their call sites in `handleIssue` (updated in the same task, per the note carried over from Task 6 Step 3).
