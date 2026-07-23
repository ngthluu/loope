# Preflight Gate Checker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Verify loope's external toolchain (git, gh, gh auth, claude, superpowers plugin, repo path, repo access, labels, curl) before any mode runs, and print what is missing plus how to fix it.

**Architecture:** One new file `preflight.go` (+ `preflight_test.go`) holds every check. Checks are pure functions over the existing `Runner` seam, each probe bounded by its own 10s timeout, returning a `[]CheckResult`. Rendering is split out into `ReportPreflight` so tests assert on structured results, not text. `main.go` gains a `gate` helper called right after `LoadConfig` (before `-rework`, `acquireLock`, and `-serve`) plus a new `-doctor` flag.

**Tech Stack:** Go 1.25.5, stdlib only (`context`, `encoding/json`, `fmt`, `io`, `strings`, `time`), `testing` for tests. No new dependencies.

## Global Constraints

- Go module `loope`, package `main`, Go 1.25.5. No new third-party dependencies.
- All process execution goes through the `Runner` interface (`runner.go`). Never call `os/exec` directly from `preflight.go`.
- Every probe runs under `context.WithTimeout(ctx, 10*time.Second)` derived from the context passed to `Preflight`. A timed-out probe is a failure of its check with the timeout named in `Detail`.
- Exact public API from the spec:
  - `func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult`
  - `func ReportPreflight(w io.Writer, results []CheckResult) (failed bool)`
  - `type CheckResult struct { Name string; Status checkStatus; Detail string; Fix []string }`
  - `type checkStatus int` with `statusOK`, `statusWarn`, `statusFail`, `statusSkip` (in that iota order).
- Check names, exactly: `git`, `gh`, `gh auth`, `claude`, `superpowers`, `repoPath`, `repo access`, `labels`, `curl` — in that order.
- Severity: `git`, `gh`, `gh auth`, `claude`, `superpowers`, `repoPath`, `repo access` are required; `labels` and `curl` are warnings only.
- Report goes to **stderr**. Status markers: `✔` ok, `✘` required failure, `!` warning, `-` skipped.
- The `claude plugin list` probe must run with `CLAUDE_CONFIG_DIR=<cfg.ClaudeConfigDir>` in its env when that field is non-empty, and with no env override when it is empty (mirrors `claude.go:110-113`).
- Tests drive `fakeRunner` (`helpers_test.go`) via its `handler` field. No real binaries.
- `-version` keeps short-circuiting before config loading and before the gate.

## Assumptions (spec gaps resolved)

1. **Report visibility outside `-doctor`:** the report is printed only when a required check failed (or when `-doctor` is set). A run whose only non-OK checks are warnings prints nothing and proceeds — this is the strictest reading of "a healthy machine sees no new output" and keeps daemon logs quiet.
2. **`gh label list` uses exactly the spec's command** (`gh label list --repo <slug> --json name`, no `--limit`). Repos with more than gh's default page size of labels could therefore produce a spurious *warning*; it is never fatal.
3. **Empty label names are skipped** by the labels check (a partially-specified `stateLabels` block cannot normally produce one, since `LoadConfig` seeds defaults, but a zero-value `Config` in a test can).
4. **`main` exits via `os.Exit`** from the gate, skipping the deferred `stop()`. That is harmless at process exit and keeps the wiring one branch.
5. Detail strings are the first line of the probe's stdout for passing checks (`git version 2.39.5`), matching the spec's sample report.

## File Structure

| File | Responsibility |
|------|----------------|
| `preflight.go` (create) | `checkStatus`, `CheckResult`, the `probe` timeout helper, the nine check functions, `Preflight`, `ReportPreflight`. |
| `preflight_test.go` (create) | Table tests for `Preflight`, a rendering test for `ReportPreflight`. |
| `main.go` (modify) | `-doctor` flag, `gate` helper, gate call after `LoadConfig`. |
| `main_test.go` (modify) | Wiring test for `gate`. |
| `README.md` (modify) | Prerequisites paragraph documenting the gate and `-doctor`. |

---

### Task 1: Preflight skeleton and the three required binary checks

Creates the types, the timeout-bounded `probe` helper, and checks 1 (`git`), 2 (`gh`), 4 (`claude`). `Preflight` returns those three results in spec order for now; later tasks insert the rest.

**Files:**
- Create: `preflight.go`
- Test: `preflight_test.go`

**Interfaces:**
- Consumes: `Runner` (`runner.go:16`), `Config` (`config.go:90`), `fakeRunner`/`rcall`/`rresp` (`helpers_test.go`).
- Produces:
  - `type checkStatus int`, consts `statusOK`, `statusWarn`, `statusFail`, `statusSkip`
  - `type CheckResult struct { Name string; Status checkStatus; Detail string; Fix []string }`
  - `func probe(ctx context.Context, r Runner, dir string, env []string, name string, args ...string) (string, error)`
  - `func firstLine(s string) string`
  - `func binaryCheck(ctx context.Context, r Runner, name string, fix []string, args ...string) CheckResult`
  - `func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult`
  - test helpers `okHandler(map[string]rresp) func(rcall) (string, string, error)`, `preflightConfig() *Config`, `resultByName([]CheckResult, string) CheckResult`

- [ ] **Step 1: Write the failing test**

Create `preflight_test.go`:

```go
package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// okHandler answers every preflight probe with a healthy default. Overrides are
// keyed by the full command line ("gh auth status") and replace the default.
func okHandler(overrides map[string]rresp) func(rcall) (string, string, error) {
	defaults := map[string]string{
		"git --version":                       "git version 2.39.5",
		"gh --version":                        "gh version 2.63.2",
		"gh auth status":                      "Logged in to github.com as you",
		"claude --version":                    "2.0.1 (Claude Code)",
		"claude plugin list":                  "superpowers@claude-plugins-official  enabled",
		"git rev-parse --is-inside-work-tree": "true",
		"gh repo view your-org/your-repo --json name":         `{"name":"your-repo"}`,
		"gh label list --repo your-org/your-repo --json name": `[{"name":"ai-agent"},{"name":"ai-wip"},{"name":"ai-failed"},{"name":"ai-done"},{"name":"ai-rework"},{"name":"ai-needs-info"}]`,
		"curl --version": "curl 8.7.1 (x86_64-apple-darwin23.0)",
	}
	return func(c rcall) (string, string, error) {
		key := strings.TrimSpace(c.name + " " + strings.Join(c.args, " "))
		if r, ok := overrides[key]; ok {
			return r.stdout, r.stderr, r.err
		}
		return defaults[key], "", nil
	}
}

func preflightConfig() *Config {
	return &Config{
		RepoPath:      "/tmp/repo",
		RepoSlug:      "your-org/your-repo",
		EligibleLabel: "ai-agent",
		StateLabels:   defaultStateLabels(),
	}
}

func resultByName(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, c := range results {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %v", name, results)
	return CheckResult{}
}

func TestPreflightBinariesPass(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	results := Preflight(context.Background(), f, preflightConfig())
	for _, name := range []string{"git", "gh", "claude"} {
		c := resultByName(t, results, name)
		if c.Status != statusOK {
			t.Fatalf("%s: status = %d, want statusOK (detail %q)", name, c.Status, c.Detail)
		}
	}
	if got := resultByName(t, results, "git").Detail; got != "git version 2.39.5" {
		t.Fatalf("git detail = %q", got)
	}
}

func TestPreflightMissingBinaryFails(t *testing.T) {
	for _, name := range []string{"git", "gh", "claude"} {
		f := &fakeRunner{handler: okHandler(map[string]rresp{
			name + " --version": {err: errors.New("executable file not found in $PATH")},
		})}
		results := Preflight(context.Background(), f, preflightConfig())
		c := resultByName(t, results, name)
		if c.Status != statusFail {
			t.Fatalf("%s: status = %d, want statusFail", name, c.Status)
		}
		if len(c.Fix) == 0 {
			t.Fatalf("%s: missing binary must carry fix hints", name)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestPreflight -v`
Expected: FAIL — `undefined: Preflight`, `undefined: CheckResult`, `undefined: statusOK`.

- [ ] **Step 3: Write minimal implementation**

Create `preflight.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
	statusSkip
)

// CheckResult is one preflight check's outcome. Fix holds remediation
// commands, printed only when Status is not statusOK.
type CheckResult struct {
	Name   string
	Status checkStatus
	Detail string
	Fix    []string
}

// probeTimeout bounds each individual probe so one hung `gh` cannot stall
// startup indefinitely.
const probeTimeout = 10 * time.Second

var (
	fixGit    = []string{"brew install git  (macOS)", "apt install git  (Debian/Ubuntu)", "https://git-scm.com/downloads"}
	fixGH     = []string{"brew install gh  (macOS)", "https://cli.github.com"}
	fixClaude = []string{"npm install -g @anthropic-ai/claude-code", "https://docs.anthropic.com/en/docs/claude-code"}
)

// probe runs one command under its own timeout derived from ctx and returns
// trimmed stdout. On failure the error names the timeout or carries the first
// line of stderr, which is what the report shows the user.
func probe(ctx context.Context, r Runner, dir string, env []string, name string, args ...string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	stdout, stderr, err := r.Run(pctx, dir, env, "", name, args...)
	if err != nil {
		if pctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("timed out after %s", probeTimeout)
		}
		if msg := firstLine(stderr); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// binaryCheck probes for an installed binary by running its version command.
func binaryCheck(ctx context.Context, r Runner, name string, fix []string, args ...string) CheckResult {
	out, err := probe(ctx, r, "", nil, name, args...)
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Detail: "not found: " + err.Error(), Fix: fix}
	}
	return CheckResult{Name: name, Status: statusOK, Detail: firstLine(out)}
}

// Preflight runs every check in order and returns the results.
func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult {
	git := binaryCheck(ctx, r, "git", fixGit, "--version")
	gh := binaryCheck(ctx, r, "gh", fixGH, "--version")
	claude := binaryCheck(ctx, r, "claude", fixClaude, "--version")
	return []CheckResult{git, gh, claude}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestPreflight -v`
Expected: PASS (`TestPreflightBinariesPass`, `TestPreflightMissingBinaryFails`).

- [ ] **Step 5: Commit**

```bash
git add preflight.go preflight_test.go
git commit -m "feat(preflight): check for git, gh and claude binaries"
```

---

### Task 2: Dependent checks — `gh auth` and `superpowers`

Adds check 3 (`gh auth`, skipped when `gh` failed) and check 5 (`superpowers`, skipped when `claude` failed), plus the shared skip helper. The superpowers probe must carry `CLAUDE_CONFIG_DIR`.

**Files:**
- Modify: `preflight.go`
- Test: `preflight_test.go`

**Interfaces:**
- Consumes: `probe`, `firstLine`, `binaryCheck`, `CheckResult`, statuses (Task 1).
- Produces:
  - `func skipIfBlocked(name string, blockers ...CheckResult) (CheckResult, bool)`
  - `func checkGHAuth(ctx context.Context, r Runner, gh CheckResult) CheckResult`
  - `func checkSuperpowers(ctx context.Context, r Runner, cfg *Config, claude CheckResult) CheckResult`
  - `Preflight` now returns, in order: `git`, `gh`, `gh auth`, `claude`, `superpowers`.

- [ ] **Step 1: Write the failing test**

Append to `preflight_test.go`:

```go
func TestPreflightGHAuthFailsAndBlocksNothingElseYet(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh auth status": {stderr: "You are not logged into any GitHub hosts.", err: errors.New("exit status 1")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "gh auth")
	if c.Status != statusFail {
		t.Fatalf("gh auth status = %d, want statusFail", c.Status)
	}
	if len(c.Fix) != 1 || c.Fix[0] != "gh auth login" {
		t.Fatalf("gh auth fix = %v, want [gh auth login]", c.Fix)
	}
}

func TestPreflightSkipsDependentChecks(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh --version":     {err: errors.New("not found")},
		"claude --version": {err: errors.New("not found")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	for name, blocker := range map[string]string{"gh auth": "gh", "superpowers": "claude"} {
		c := resultByName(t, results, name)
		if c.Status != statusSkip {
			t.Fatalf("%s status = %d, want statusSkip", name, c.Status)
		}
		if !strings.Contains(c.Detail, blocker) {
			t.Fatalf("%s detail = %q, want it to name %q", name, c.Detail, blocker)
		}
	}
}

func TestPreflightSuperpowersMissingPlugin(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"claude plugin list": {stdout: "some-other-plugin@vendor  enabled"},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "superpowers")
	if c.Status != statusFail {
		t.Fatalf("superpowers status = %d, want statusFail", c.Status)
	}
	if len(c.Fix) == 0 || !strings.Contains(c.Fix[0], "claude plugin install superpowers@") {
		t.Fatalf("superpowers fix = %v", c.Fix)
	}
}

func TestPreflightSuperpowersUsesClaudeConfigDir(t *testing.T) {
	cfg := preflightConfig()
	cfg.ClaudeConfigDir = "/home/you/.claude-personal"
	f := &fakeRunner{handler: okHandler(nil)}
	Preflight(context.Background(), f, cfg)
	var got []string
	for _, c := range f.calls {
		if c.name == "claude" && hasArg(c.args, "plugin") {
			got = c.env
		}
	}
	if len(got) != 1 || got[0] != "CLAUDE_CONFIG_DIR=/home/you/.claude-personal" {
		t.Fatalf("plugin list env = %v, want [CLAUDE_CONFIG_DIR=/home/you/.claude-personal]", got)
	}

	cfg.ClaudeConfigDir = ""
	f2 := &fakeRunner{handler: okHandler(nil)}
	Preflight(context.Background(), f2, cfg)
	for _, c := range f2.calls {
		if c.name == "claude" && hasArg(c.args, "plugin") && len(c.env) != 0 {
			t.Fatalf("plugin list env = %v, want none when claudeConfigDir is unset", c.env)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestPreflight -v`
Expected: FAIL — `no check named "gh auth"` / `no check named "superpowers"`.

- [ ] **Step 3: Write minimal implementation**

Add to `preflight.go` (and extend `Preflight`):

```go
var fixSuperpowers = []string{"claude plugin install superpowers@claude-plugins-official"}

// skipIfBlocked returns a statusSkip result naming the first blocker that did
// not pass. A skipped check is never fatal on its own — its blocker already is.
func skipIfBlocked(name string, blockers ...CheckResult) (CheckResult, bool) {
	for _, b := range blockers {
		if b.Status == statusFail || b.Status == statusSkip {
			return CheckResult{Name: name, Status: statusSkip, Detail: fmt.Sprintf("skipped (%s failed)", b.Name)}, true
		}
	}
	return CheckResult{}, false
}

func checkGHAuth(ctx context.Context, r Runner, gh CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("gh auth", gh); skipped {
		return res
	}
	out, err := probe(ctx, r, "", nil, "gh", "auth", "status")
	if err != nil {
		return CheckResult{Name: "gh auth", Status: statusFail, Detail: "not authenticated", Fix: []string{"gh auth login"}}
	}
	detail := firstLine(out)
	if detail == "" {
		detail = "authenticated"
	}
	return CheckResult{Name: "gh auth", Status: statusOK, Detail: detail}
}

// checkSuperpowers verifies the superpowers plugin is installed in the *same*
// Claude profile the pipeline runs under: without CLAUDE_CONFIG_DIR a user on a
// dedicated profile would get a false pass from their default ~/.claude.
func checkSuperpowers(ctx context.Context, r Runner, cfg *Config, claude CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("superpowers", claude); skipped {
		return res
	}
	var env []string
	if cfg.ClaudeConfigDir != "" {
		env = []string{"CLAUDE_CONFIG_DIR=" + cfg.ClaudeConfigDir}
	}
	out, err := probe(ctx, r, "", env, "claude", "plugin", "list")
	if err != nil {
		return CheckResult{Name: "superpowers", Status: statusFail, Detail: "claude plugin list failed: " + err.Error(), Fix: fixSuperpowers}
	}
	if !strings.Contains(out, "superpowers@") {
		detail := "plugin not installed"
		if cfg.ClaudeConfigDir != "" {
			detail += " (CLAUDE_CONFIG_DIR=" + cfg.ClaudeConfigDir + ")"
		}
		return CheckResult{Name: "superpowers", Status: statusFail, Detail: detail, Fix: fixSuperpowers}
	}
	return CheckResult{Name: "superpowers", Status: statusOK, Detail: "installed"}
}
```

Replace `Preflight`'s body with:

```go
func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult {
	git := binaryCheck(ctx, r, "git", fixGit, "--version")
	gh := binaryCheck(ctx, r, "gh", fixGH, "--version")
	ghAuth := checkGHAuth(ctx, r, gh)
	claude := binaryCheck(ctx, r, "claude", fixClaude, "--version")
	superpowers := checkSuperpowers(ctx, r, cfg, claude)
	return []CheckResult{git, gh, ghAuth, claude, superpowers}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestPreflight -v`
Expected: PASS (all six preflight tests).

- [ ] **Step 5: Commit**

```bash
git add preflight.go preflight_test.go
git commit -m "feat(preflight): check gh auth and the superpowers plugin"
```

---

### Task 3: Repo checks — `repoPath` and `repo access`

Adds check 6 (`git rev-parse --is-inside-work-tree` run **in `cfg.RepoPath`**, skipped when `git` failed) and check 7 (`gh repo view <slug> --json name`, skipped when `gh` or `gh auth` failed).

**Files:**
- Modify: `preflight.go`
- Test: `preflight_test.go`

**Interfaces:**
- Consumes: `probe`, `skipIfBlocked`, `CheckResult` (Tasks 1–2).
- Produces:
  - `func checkRepoPath(ctx context.Context, r Runner, cfg *Config, git CheckResult) CheckResult`
  - `func checkRepoAccess(ctx context.Context, r Runner, cfg *Config, gh, ghAuth CheckResult) CheckResult`
  - `Preflight` order is now: `git`, `gh`, `gh auth`, `claude`, `superpowers`, `repoPath`, `repo access`.

- [ ] **Step 1: Write the failing test**

Append to `preflight_test.go`:

```go
func TestPreflightRepoPathNotAWorktree(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"git rev-parse --is-inside-work-tree": {stderr: "fatal: not a git repository", err: errors.New("exit status 128")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "repoPath")
	if c.Status != statusFail {
		t.Fatalf("repoPath status = %d, want statusFail", c.Status)
	}
	if !strings.Contains(c.Detail, "/tmp/repo") {
		t.Fatalf("repoPath detail = %q, want it to name the configured path", c.Detail)
	}
}

func TestPreflightRepoPathRunsInRepoDir(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	Preflight(context.Background(), f, preflightConfig())
	found := false
	for _, c := range f.calls {
		if c.name == "git" && hasArg(c.args, "rev-parse") {
			found = true
			if c.dir != "/tmp/repo" {
				t.Fatalf("rev-parse dir = %q, want /tmp/repo", c.dir)
			}
		}
	}
	if !found {
		t.Fatal("git rev-parse was never run")
	}
}

func TestPreflightRepoAccessSkippedWhenAuthFails(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh auth status": {err: errors.New("exit status 1")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "repo access")
	if c.Status != statusSkip {
		t.Fatalf("repo access status = %d, want statusSkip", c.Status)
	}
	if !strings.Contains(c.Detail, "gh auth") {
		t.Fatalf("repo access detail = %q, want it to name the blocker", c.Detail)
	}
}

func TestPreflightRepoAccessFails(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh repo view your-org/your-repo --json name": {stderr: "GraphQL: Could not resolve to a Repository", err: errors.New("exit status 1")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "repo access")
	if c.Status != statusFail {
		t.Fatalf("repo access status = %d, want statusFail", c.Status)
	}
	if !strings.Contains(c.Detail, "your-org/your-repo") {
		t.Fatalf("repo access detail = %q, want it to name the slug", c.Detail)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestPreflight -v`
Expected: FAIL — `no check named "repoPath"` / `no check named "repo access"`.

- [ ] **Step 3: Write minimal implementation**

Add to `preflight.go`:

```go
func checkRepoPath(ctx context.Context, r Runner, cfg *Config, git CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("repoPath", git); skipped {
		return res
	}
	out, err := probe(ctx, r, cfg.RepoPath, nil, "git", "rev-parse", "--is-inside-work-tree")
	if err != nil || out != "true" {
		return CheckResult{
			Name:   "repoPath",
			Status: statusFail,
			Detail: fmt.Sprintf("%s is not a git worktree", cfg.RepoPath),
			Fix:    []string{"git clone <your-repo> " + cfg.RepoPath, "or point repoPath at an existing clone in your config"},
		}
	}
	return CheckResult{Name: "repoPath", Status: statusOK, Detail: cfg.RepoPath}
}

func checkRepoAccess(ctx context.Context, r Runner, cfg *Config, gh, ghAuth CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("repo access", gh, ghAuth); skipped {
		return res
	}
	if _, err := probe(ctx, r, "", nil, "gh", "repo", "view", cfg.RepoSlug, "--json", "name"); err != nil {
		return CheckResult{
			Name:   "repo access",
			Status: statusFail,
			Detail: fmt.Sprintf("cannot access %s: %v", cfg.RepoSlug, err),
			Fix:    []string{"gh auth refresh -h github.com -s repo", "or fix repoSlug in your config"},
		}
	}
	return CheckResult{Name: "repo access", Status: statusOK, Detail: cfg.RepoSlug}
}
```

Extend `Preflight`:

```go
func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult {
	git := binaryCheck(ctx, r, "git", fixGit, "--version")
	gh := binaryCheck(ctx, r, "gh", fixGH, "--version")
	ghAuth := checkGHAuth(ctx, r, gh)
	claude := binaryCheck(ctx, r, "claude", fixClaude, "--version")
	superpowers := checkSuperpowers(ctx, r, cfg, claude)
	repoPath := checkRepoPath(ctx, r, cfg, git)
	access := checkRepoAccess(ctx, r, cfg, gh, ghAuth)
	return []CheckResult{git, gh, ghAuth, claude, superpowers, repoPath, access}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestPreflight -v`
Expected: PASS (all ten preflight tests).

- [ ] **Step 5: Commit**

```bash
git add preflight.go preflight_test.go
git commit -m "feat(preflight): check repoPath is a worktree and the repo is reachable"
```

---

### Task 4: Warning checks — `labels` and `curl`

Adds check 8 (`labels`, warning, skipped when `repo access` did not pass) and check 9 (`curl`, warning, never skipped). `Preflight` is complete after this task.

**Files:**
- Modify: `preflight.go`
- Test: `preflight_test.go`

**Interfaces:**
- Consumes: `probe`, `skipIfBlocked`, `CheckResult` (Tasks 1–3), `Config.EligibleLabel` and `Config.StateLabels` (`config.go:62-104`).
- Produces:
  - `func wantedLabels(cfg *Config) []string`
  - `func checkLabels(ctx context.Context, r Runner, cfg *Config, access CheckResult) CheckResult`
  - `func checkCurl(ctx context.Context, r Runner) CheckResult`
  - `Preflight` returns all nine results in spec order.

- [ ] **Step 1: Write the failing test**

Append to `preflight_test.go`:

```go
func TestPreflightMissingLabelsWarn(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh label list --repo your-org/your-repo --json name": {stdout: `[{"name":"ai-agent"},{"name":"ai-wip"},{"name":"ai-done"}]`},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "labels")
	if c.Status != statusWarn {
		t.Fatalf("labels status = %d, want statusWarn", c.Status)
	}
	want := []string{
		"gh label create ai-failed --repo your-org/your-repo",
		"gh label create ai-rework --repo your-org/your-repo",
		"gh label create ai-needs-info --repo your-org/your-repo",
	}
	if len(c.Fix) != len(want) {
		t.Fatalf("labels fix = %v, want %v", c.Fix, want)
	}
	for i := range want {
		if c.Fix[i] != want[i] {
			t.Fatalf("labels fix[%d] = %q, want %q", i, c.Fix[i], want[i])
		}
	}
	if ReportPreflightFailedCount(results) != 0 {
		t.Fatal("a labels warning must not be fatal")
	}
}

func TestPreflightAllLabelsPresent(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	results := Preflight(context.Background(), f, preflightConfig())
	if c := resultByName(t, results, "labels"); c.Status != statusOK {
		t.Fatalf("labels status = %d (detail %q), want statusOK", c.Status, c.Detail)
	}
}

func TestPreflightMissingCurlWarns(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"curl --version": {err: errors.New("not found")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "curl")
	if c.Status != statusWarn {
		t.Fatalf("curl status = %d, want statusWarn", c.Status)
	}
	if !strings.Contains(c.Detail, "image attachments") {
		t.Fatalf("curl detail = %q, want it to explain the degradation", c.Detail)
	}
	if ReportPreflightFailedCount(results) != 0 {
		t.Fatal("a missing curl must not be fatal")
	}
}

func TestPreflightHealthyMachineHasNoFailures(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	results := Preflight(context.Background(), f, preflightConfig())
	if len(results) != 9 {
		t.Fatalf("got %d checks, want 9", len(results))
	}
	for _, c := range results {
		if c.Status != statusOK {
			t.Fatalf("%s: status = %d (detail %q), want statusOK", c.Name, c.Status, c.Detail)
		}
	}
}

func TestPreflightSkippedChecksAreNotFailures(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh --version": {err: errors.New("not found")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	// Only `gh` itself is a failure; gh auth / repo access / labels all skip.
	if n := ReportPreflightFailedCount(results); n != 1 {
		t.Fatalf("failed count = %d, want 1 (only gh)", n)
	}
	if c := resultByName(t, results, "labels"); c.Status != statusSkip {
		t.Fatalf("labels status = %d, want statusSkip", c.Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestPreflight -v`
Expected: FAIL — `undefined: ReportPreflightFailedCount`, `no check named "labels"`.

- [ ] **Step 3: Write minimal implementation**

Add to `preflight.go` (note the new `encoding/json` import):

```go
// wantedLabels is every label the loop applies: the eligible label plus all
// five state labels. Empty names are skipped.
func wantedLabels(cfg *Config) []string {
	names := []string{
		cfg.EligibleLabel,
		cfg.StateLabels.WIP,
		cfg.StateLabels.Failed,
		cfg.StateLabels.Done,
		cfg.StateLabels.Rework,
		cfg.StateLabels.NeedsInfo,
	}
	out := names[:0:0]
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// checkLabels warns (never fails) about labels the loop needs but the repo does
// not have, handing the user the exact `gh label create` commands.
func checkLabels(ctx context.Context, r Runner, cfg *Config, access CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("labels", access); skipped {
		return res
	}
	out, err := probe(ctx, r, "", nil, "gh", "label", "list", "--repo", cfg.RepoSlug, "--json", "name")
	if err != nil {
		return CheckResult{Name: "labels", Status: statusWarn, Detail: "could not list labels: " + err.Error()}
	}
	var got []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		return CheckResult{Name: "labels", Status: statusWarn, Detail: "could not parse gh label list output: " + err.Error()}
	}
	have := make(map[string]bool, len(got))
	for _, l := range got {
		have[l.Name] = true
	}
	var missing, fix []string
	for _, want := range wantedLabels(cfg) {
		if !have[want] {
			missing = append(missing, want)
			fix = append(fix, fmt.Sprintf("gh label create %s --repo %s", want, cfg.RepoSlug))
		}
	}
	if len(missing) > 0 {
		return CheckResult{
			Name:   "labels",
			Status: statusWarn,
			Detail: "missing: " + strings.Join(missing, ", "),
			Fix:    fix,
		}
	}
	return CheckResult{Name: "labels", Status: statusOK, Detail: fmt.Sprintf("all %d configured labels exist", len(wantedLabels(cfg)))}
}

// checkCurl is a warning: images.go already degrades gracefully, so a missing
// curl costs issue image attachments and nothing else.
func checkCurl(ctx context.Context, r Runner) CheckResult {
	out, err := probe(ctx, r, "", nil, "curl", "--version")
	if err != nil {
		return CheckResult{
			Name:   "curl",
			Status: statusWarn,
			Detail: "not found — issue image attachments will be skipped",
			Fix:    []string{"brew install curl  (macOS)", "apt install curl  (Debian/Ubuntu)"},
		}
	}
	return CheckResult{Name: "curl", Status: statusOK, Detail: firstLine(out)}
}

// ReportPreflightFailedCount counts required-check failures. Warnings and
// skipped checks never count: a skipped check's blocker is already counted.
func ReportPreflightFailedCount(results []CheckResult) int {
	n := 0
	for _, c := range results {
		if c.Status == statusFail {
			n++
		}
	}
	return n
}
```

Complete `Preflight`:

```go
func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult {
	git := binaryCheck(ctx, r, "git", fixGit, "--version")
	gh := binaryCheck(ctx, r, "gh", fixGH, "--version")
	ghAuth := checkGHAuth(ctx, r, gh)
	claude := binaryCheck(ctx, r, "claude", fixClaude, "--version")
	superpowers := checkSuperpowers(ctx, r, cfg, claude)
	repoPath := checkRepoPath(ctx, r, cfg, git)
	access := checkRepoAccess(ctx, r, cfg, gh, ghAuth)
	labels := checkLabels(ctx, r, cfg, access)
	curl := checkCurl(ctx, r)
	return []CheckResult{git, gh, ghAuth, claude, superpowers, repoPath, access, labels, curl}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestPreflight -v`
Expected: PASS (all fifteen preflight tests).

- [ ] **Step 5: Commit**

```bash
git add preflight.go preflight_test.go
git commit -m "feat(preflight): warn on missing labels and missing curl"
```

---

### Task 5: `ReportPreflight` rendering

Renders the report exactly as the spec's sample and returns whether any required check failed.

**Files:**
- Modify: `preflight.go`
- Test: `preflight_test.go`

**Interfaces:**
- Consumes: `CheckResult`, statuses, `ReportPreflightFailedCount` (Tasks 1–4).
- Produces:
  - `func statusMarker(s checkStatus) string`
  - `func ReportPreflight(w io.Writer, results []CheckResult) (failed bool)`

- [ ] **Step 1: Write the failing test**

Append to `preflight_test.go` (add `"bytes"` to its imports):

```go
func TestReportPreflightRendersAllStatuses(t *testing.T) {
	results := []CheckResult{
		{Name: "git", Status: statusOK, Detail: "git version 2.39.5"},
		{Name: "gh auth", Status: statusFail, Detail: "not authenticated", Fix: []string{"gh auth login"}},
		{Name: "repo access", Status: statusSkip, Detail: "skipped (gh auth failed)"},
		{Name: "curl", Status: statusWarn, Detail: "not found — issue image attachments will be skipped"},
	}
	var buf bytes.Buffer
	failed := ReportPreflight(&buf, results)
	if !failed {
		t.Fatal("failed = false, want true (gh auth is a required check)")
	}
	want := "loope preflight\n\n" +
		"  ✔ git           git version 2.39.5\n" +
		"  ✘ gh auth       not authenticated\n" +
		"      → gh auth login\n" +
		"  - repo access   skipped (gh auth failed)\n" +
		"  ! curl          not found — issue image attachments will be skipped\n" +
		"\n1 required check failed. Fix them and re-run `loope -doctor` to verify.\n"
	if got := buf.String(); got != want {
		t.Fatalf("report =\n%q\nwant\n%q", got, want)
	}
}

func TestReportPreflightHealthyOmitsSummary(t *testing.T) {
	var buf bytes.Buffer
	failed := ReportPreflight(&buf, []CheckResult{{Name: "git", Status: statusOK, Detail: "git version 2.39.5"}})
	if failed {
		t.Fatal("failed = true, want false")
	}
	if strings.Contains(buf.String(), "required check") {
		t.Fatalf("healthy report must omit the summary line, got %q", buf.String())
	}
}

func TestReportPreflightPluralSummary(t *testing.T) {
	var buf bytes.Buffer
	ReportPreflight(&buf, []CheckResult{
		{Name: "gh", Status: statusFail, Detail: "not found"},
		{Name: "claude", Status: statusFail, Detail: "not found"},
	})
	if !strings.Contains(buf.String(), "2 required checks failed.") {
		t.Fatalf("report = %q, want a plural summary line", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestReportPreflight -v`
Expected: FAIL — `undefined: ReportPreflight`.

- [ ] **Step 3: Write minimal implementation**

Add to `preflight.go` (add `"io"` to the imports):

```go
func statusMarker(s checkStatus) string {
	switch s {
	case statusOK:
		return "✔"
	case statusFail:
		return "✘"
	case statusWarn:
		return "!"
	default:
		return "-"
	}
}

// ReportPreflight writes the human-readable report to w and reports whether
// any required check failed. Fix hints print only for non-OK checks; the
// trailing summary line is omitted when nothing required failed.
func ReportPreflight(w io.Writer, results []CheckResult) (failed bool) {
	fmt.Fprintf(w, "loope preflight\n\n")
	for _, c := range results {
		fmt.Fprintf(w, "  %s %-13s %s\n", statusMarker(c.Status), c.Name, c.Detail)
		if c.Status != statusOK {
			for _, f := range c.Fix {
				fmt.Fprintf(w, "      → %s\n", f)
			}
		}
	}
	n := ReportPreflightFailedCount(results)
	if n == 0 {
		return false
	}
	noun := "checks"
	if n == 1 {
		noun = "check"
	}
	fmt.Fprintf(w, "\n%d required %s failed. Fix them and re-run `loope -doctor` to verify.\n", n, noun)
	return true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestReportPreflight -v`
Expected: PASS (three rendering tests).

- [ ] **Step 5: Commit**

```bash
git add preflight.go preflight_test.go
git commit -m "feat(preflight): render the preflight report"
```

---

### Task 6: Wire the gate into `main.go`, add `-doctor`, document it

The gate runs immediately after `LoadConfig` and before the `-rework` branch, `acquireLock`, and the `-serve` setup, so every mode is gated by one code path. `-version` still short-circuits before config loading. Includes the README update, since the flag is user-facing.

**Files:**
- Modify: `main.go:20-50`
- Modify: `main_test.go`
- Modify: `README.md:91-112` (Prerequisites)

**Interfaces:**
- Consumes: `Preflight`, `ReportPreflight` (Tasks 1–5), `Runner`, `Config`.
- Produces: `func gate(ctx context.Context, w io.Writer, r Runner, cfg *Config, doctor bool) (exitCode int, proceed bool)`.

- [ ] **Step 1: Write the failing test**

Append to `main_test.go` (imports become `bytes`, `context`, `errors`, `strings`, `testing`):

```go
func TestGateBlocksOnRequiredFailure(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"claude --version": {err: errors.New("not found")},
	})}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), false)
	if proceed {
		t.Fatal("proceed = true, want false when a required check failed")
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "claude") {
		t.Fatalf("report must name the failing check, got %q", buf.String())
	}
}

func TestGateWarningsOnlyProceedSilently(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"curl --version": {err: errors.New("not found")},
	})}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), false)
	if !proceed || code != 0 {
		t.Fatalf("gate = (%d, %v), want (0, true) for warnings only", code, proceed)
	}
	if buf.String() != "" {
		t.Fatalf("a healthy run must print nothing, got %q", buf.String())
	}
}

func TestGateDoctorAlwaysReportsAndNeverProceeds(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), true)
	if proceed {
		t.Fatal("-doctor must never proceed into the loop")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when everything passes", code)
	}
	if !strings.Contains(buf.String(), "loope preflight") {
		t.Fatalf("-doctor must print the report even when healthy, got %q", buf.String())
	}

	f2 := &fakeRunner{handler: okHandler(map[string]rresp{"gh --version": {err: errors.New("not found")}})}
	var buf2 bytes.Buffer
	code2, _ := gate(context.Background(), &buf2, f2, preflightConfig(), true)
	if code2 != 1 {
		t.Fatalf("-doctor exit code = %d, want 1 when a required check failed", code2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestGate -v`
Expected: FAIL — `undefined: gate`.

- [ ] **Step 3: Write minimal implementation**

Add to `main.go` (add `"io"` to the imports):

```go
// gate runs the preflight checks before any mode starts. It returns the process
// exit code and whether the caller should continue. The report is printed only
// when a required check failed or when -doctor asked for it, so a healthy
// daemon run adds no output.
func gate(ctx context.Context, w io.Writer, r Runner, cfg *Config, doctor bool) (exitCode int, proceed bool) {
	results := Preflight(ctx, r, cfg)
	failed := ReportPreflightFailedCount(results) > 0
	if doctor || failed {
		ReportPreflight(w, results)
	}
	if failed {
		return 1, false
	}
	if doctor {
		return 0, false
	}
	return 0, true
}
```

In `main`, add the flag next to the others:

```go
	doctor := flag.Bool("doctor", false, "run the preflight checks, print the report, and exit")
```

And insert the gate between the signal context and the `*rework` branch, moving `r := execRunner{}` above it:

```go
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := execRunner{}
	if code, proceed := gate(ctx, os.Stderr, r, cfg, *doctor); !proceed {
		os.Exit(code)
	}

	o := &Orchestrator{cfg: cfg, runner: r, gh: NewGitHub(r, cfg),
		wt: &Worktree{runner: r, repoPath: cfg.RepoPath, retry: cfg.GitHubRetry.policy()}}
```

(The existing `r := execRunner{}` line at `main.go:41` is removed — it is now above the gate.)

- [ ] **Step 4: Run the full suite and build**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS — build and vet clean, all packages ok.

- [ ] **Step 5: Update the README**

In `README.md`, replace the Prerequisites list's trailing content so the section reads (keeping the existing warning block and `gh label create` block exactly as they are, and inserting the two new paragraphs):

After the `- **claude** (Claude Code CLI), logged in.` bullet, add:

~~~markdown
- The **superpowers** plugin installed in the Claude profile loope runs under
  (`claude plugin install superpowers@claude-plugins-official`) — the pipeline
  prompts are superpowers slash commands and are inert text without it.
- **curl** (optional) — used to download issue image attachments; without it
  those are skipped.

loope verifies this toolchain at startup and refuses to run when a required
piece is missing, printing what is missing and the command that fixes it. To
run the same checks standalone:

```bash
./loope -doctor -config loope.json
```

`-doctor` prints the full report even when everything passes and exits non-zero
when a required check failed. Missing labels and a missing `curl` are warnings:
they are reported but never block the run.
~~~

Then, immediately above the existing `gh label create` block, keep the existing
sentence and append the cross-reference:

```markdown
The state labels and the eligible label must exist in the repo before the
loop can apply them — the `labels` preflight check warns with exactly these
commands when any are missing:
```

- [ ] **Step 6: Verify the docs and suite together**

Run: `go test ./... && grep -n "doctor" README.md`
Expected: tests PASS and `grep` shows the new `-doctor` lines.

- [ ] **Step 7: Commit**

```bash
git add main.go main_test.go README.md
git commit -m "feat: gate every mode on preflight checks and add -doctor"
```

---

## Verification

Final check before calling the feature done:

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: clean build, no vet findings, `ok  	loope`. Every check in the spec's
table has a test in `preflight_test.go`, and `-doctor`'s exit code is covered by
`main_test.go`.
