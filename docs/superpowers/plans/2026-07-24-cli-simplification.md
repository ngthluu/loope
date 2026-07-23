# Simplify the loope CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collapse the `loope` CLI to one run mode (`--config <FILE>`) plus three utility flags (`--doctor`, `--version`, `--help`), driven entirely by a required config file, with the dashboard always served on an address read from the config.

**Architecture:** The listen address moves from the `-addr` flag into `Config.Addr` (optional, defaulting to `localhost:8080`). `main.go`'s flag set drops to `--config`, `--version`, `--doctor`; a small pure helper `resolveMode` maps parsed flags to a run mode enum that `main` switches on. The workDir lock, startup orphan sweep, and dashboard start-up all become unconditional (the old `-once`/`-serve` gating is deleted). `runLoop` drops its `once` parameter and returns only on context cancellation.

**Tech Stack:** Go 1.25+, standard library only (`flag`, `net/http`, `os/signal`). No new dependencies.

## Global Constraints

- Go 1.25+; standard library only — no new flag-parsing dependency (Go's `flag` accepts both `-config` and `--config`).
- Help text and docs present the **double-dash** forms (`--config`, `--doctor`, `--version`, `--help`); single-dash forms keep working because Go's `flag` treats `-x` and `--x` identically.
- Exit codes are exact: clean shutdown / help / version = `0`; preflight required-failure = `1`; `--doctor` without `--config` and genuine flag parse errors = `2`.
- `--version` must print `loope <version>` and exit **without reading the config**.
- Existing config files without an `addr` field must behave exactly as the old `-addr` default did (`localhost:8080`).
- Removed flags `-once`, `-rework`, `-serve`, `-addr` are deleted from the flag set (breaking change; docs/examples updated in the same change).
- `go build ./...` and `go test ./...` must be green at the end of every task.

## Assumptions (headless-mode calls)

- **`resolveMode` seam.** The spec offers `func resolveMode(args)` returning an enum as "the cleanest seam." This plan implements it as a **pure** function `resolveMode(configPath string, showVersion, doctor bool) cliMode` — the flag *parsing* stays in `main`, and the pure decision function is unit-tested directly. This keeps `resolveMode` trivially testable and covers the "`--version` does not read config" requirement (it returns `modeVersion` before `main` ever calls `LoadConfig`).
- **Help/error output routing.** To satisfy "help → stdout / exit 0" and "genuine parse error → stderr / exit 2" with a single `flag.FlagSet`, `main` gives the FlagSet a `bytes.Buffer` as output, parses, then flushes the buffer to `os.Stdout` on `flag.ErrHelp` and to `os.Stderr` on any other parse error. The bare-invocation help path (`modeHelp`) prints usage directly to `os.Stdout`.
- **Test rename.** The spec names the updated test `TestRunLoopOnceDrainsInFlightPipelines`. Because the `once` concept is gone, this plan renames it to `TestRunLoopDrainsInFlightPipelinesOnCancel` (the "Once" in the old name is now a misnomer). Behavior asserted is identical: the loop drains the in-flight pipeline before returning, now driven by context cancellation.
- **`loope.json.example` placement.** The `"addr"` line is inserted immediately after `"eligibleLabel"` so the network-facing settings sit near the top.

## File Structure

| File | Responsibility |
|---|---|
| `config.go` | Add `Config.Addr` field + `localhost:8080` default in `LoadConfig`. |
| `config_test.go` | Assert `Addr` defaults and is honored when present. |
| `main.go` | New flag set + `resolveMode`/`cliMode` + `usage` helper; unconditional lock/sweep/dashboard; `runLoop` loses its `once` parameter. |
| `main_test.go` | Add `TestResolveMode`; rewrite the drain test to use context cancellation. |
| `loope.json.example` | Add `"addr": "localhost:8080"`. |
| `launchd/com.loope.plist.example` | Program args become `--config /ABSOLUTE/PATH/TO/loope.json` (drop `-serve`). |
| `README.md` | Rewrite CLI/flags/dashboard sections; remove `-once`/`-serve`/`-addr`/manual `-rework`; document required `--config`. |

---

## Task 1: Config gains the listen address

**Files:**
- Modify: `config.go:90-104` (add `Addr` field to `Config`), `config.go:111` (default in `LoadConfig`)
- Test: `config_test.go` (append two tests)

**Interfaces:**
- Consumes: nothing new.
- Produces: `Config.Addr string` (JSON key `addr`), defaulting to `"localhost:8080"`. `main.go` (Task 3) reads `cfg.Addr` for the dashboard listener.

- [ ] **Step 1: Write the failing tests**

Append to `config_test.go`:

```go
func TestLoadConfigAddrDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "localhost:8080" {
		t.Errorf("Addr = %q, want localhost:8080", cfg.Addr)
	}
}

func TestLoadConfigAddrOverride(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w", "addr": "localhost:9000"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "localhost:9000" {
		t.Errorf("Addr = %q, want localhost:9000", cfg.Addr)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run TestLoadConfigAddr -v`
Expected: FAIL — `cfg.Addr` is `""` (undefined field would be a compile error; add the field in Step 3). If it is a compile error at this point, that also counts as "red" — proceed to Step 3.

- [ ] **Step 3: Add the field and its default**

In `config.go`, add the `Addr` field to the `Config` struct (place it right after `WorkDir` so path/network settings group together):

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
	StateLabels         StateLabels `json:"stateLabels"`
	GitHubRetry         RetryConfig `json:"githubRetry"`
	Models              Models      `json:"models"`
}
```

In `LoadConfig`, add `Addr: "localhost:8080"` to the defaults literal (the same literal that seeds `EligibleLabel` etc.):

```go
	cfg := &Config{Addr: "localhost:8080", EligibleLabel: "ai-agent", PollIntervalSec: 60, MaxQARounds: 20, ConfidenceThreshold: 70, TicketsPerCycle: 1, StateLabels: defaultStateLabels(), GitHubRetry: RetryConfig{BaseDelaySec: 2, MaxDelaySec: 60}}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run TestLoadConfig -v`
Expected: PASS (all `TestLoadConfig*` tests, including the two new ones).

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add optional addr config field with localhost:8080 default"
```

---

## Task 2: Add the `resolveMode` dispatch helper

**Files:**
- Modify: `main.go` (add `cliMode` type, its constants, and `resolveMode` — additive; `main` is not rewired until Task 3)
- Test: `main_test.go` (add `TestResolveMode`)

**Interfaces:**
- Consumes: nothing.
- Produces: `type cliMode int` with constants `modeRun`, `modeVersion`, `modeHelp`, `modeDoctorNoConfig`; and `func resolveMode(configPath string, showVersion, doctor bool) cliMode`. Task 3's `main` switches on the return value.

Note: adding an unused package-level function/type does not break the Go build, so this task is safe to land before `main` is rewired.

- [ ] **Step 1: Write the failing test**

Append to `main_test.go`:

```go
func TestResolveMode(t *testing.T) {
	cases := []struct {
		name       string
		configPath string
		version    bool
		doctor     bool
		want       cliMode
	}{
		{"version wins over config and doctor", "loope.json", true, true, modeVersion},
		{"version without config", "", true, false, modeVersion},
		{"config runs", "loope.json", false, false, modeRun},
		{"config with doctor still runs", "loope.json", false, true, modeRun},
		{"bare invocation is help", "", false, false, modeHelp},
		{"doctor without config is a usage error", "", false, true, modeDoctorNoConfig},
	}
	for _, c := range cases {
		if got := resolveMode(c.configPath, c.version, c.doctor); got != c.want {
			t.Errorf("%s: resolveMode(%q, %v, %v) = %d, want %d",
				c.name, c.configPath, c.version, c.doctor, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./... -run TestResolveMode -v`
Expected: FAIL — compile error `undefined: resolveMode` / `undefined: cliMode` / `undefined: modeRun`.

- [ ] **Step 3: Add the type and function**

In `main.go`, add near the top (below the `version` var, above `main`):

```go
// cliMode is the run mode resolved from the parsed command-line flags.
type cliMode int

const (
	modeRun            cliMode = iota // start the daemon (config given)
	modeVersion                       // print version and exit, without reading config
	modeHelp                          // print usage and exit 0 (bare invocation / --help)
	modeDoctorNoConfig                // --doctor without --config: usage error, exit 2
)

// resolveMode maps the parsed flags to a run mode. --version wins over
// everything (the config is never read); a missing --config means help unless
// --doctor was asked for, which is a usage error.
func resolveMode(configPath string, showVersion, doctor bool) cliMode {
	switch {
	case showVersion:
		return modeVersion
	case configPath == "" && doctor:
		return modeDoctorNoConfig
	case configPath == "":
		return modeHelp
	default:
		return modeRun
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./... -run TestResolveMode -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add resolveMode CLI dispatch helper"
```

---

## Task 3: Rewire main — new flag set, unconditional lock/sweep/dashboard, `runLoop` signature

This is the core change. `runLoop` loses its `once` parameter (which breaks the existing drain test), so the test rewrite ships in the same task to keep the build green.

**Files:**
- Modify: `main.go:21-102` (`main`), `main.go:131` (`runLoop` signature + body), `main.go:3-15` (imports)
- Test: `main_test.go:30-57` (rewrite `TestRunLoopOnceDrainsInFlightPipelines` → `TestRunLoopDrainsInFlightPipelinesOnCancel`)

**Interfaces:**
- Consumes: `resolveMode`/`cliMode` (Task 2); `cfg.Addr` (Task 1); existing `gate`, `acquireLock`, `NewServer`, `Orchestrator`, `execRunner`, `NewGitHub`, `Worktree`.
- Produces: `func runLoop(ctx context.Context, o *Orchestrator, cfg *Config, sweep bool)` (the `once bool` parameter is removed); `func usage(fs *flag.FlagSet, w io.Writer)`.

- [ ] **Step 1: Rewrite the drain test (failing)**

Replace `TestRunLoopOnceDrainsInFlightPipelines` (`main_test.go:30-57`) with:

```go
// The workDir lock is released by main's deferred release() after runLoop
// returns, so runLoop must not return while pipelines are still running — a
// second daemon could otherwise steal live ai-wip work. Shutdown is now driven
// only by context cancellation, so the loop must drain in-flight pipelines on
// that path before returning.
func TestRunLoopDrainsInFlightPipelinesOnCancel(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	// A long poll interval parks the loop in its select so cancellation is the
	// only wake-up, making the drain deterministic.
	o.cfg.PollIntervalSec = 3600
	started, release := gatePipelines(o, env.f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runLoop(ctx, o, o.cfg, false /* sweep */)
		close(done)
	}()

	awaitStarted(t, started, 1)
	cancel() // signal shutdown while the pipeline is still gated

	select {
	case <-done:
		t.Fatal("runLoop returned while a pipeline was still in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not return after pipelines drained")
	}
	if n := len(env.callsMatching("gh", "pr create")); n != 1 {
		t.Fatalf("pr create count = %d, want 1 (the pipeline must have completed)", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./... -run TestRunLoopDrainsInFlightPipelinesOnCancel -v`
Expected: FAIL — compile error: `runLoop` still has the old `once` parameter, so `runLoop(ctx, o, o.cfg, false)` passes too few arguments.

- [ ] **Step 3: Update the `runLoop` signature and body**

In `main.go`, change the signature and drop the `once` branch. Replace the doc comment's last line and the signature/`if once` block:

Signature (`main.go:131`):

```go
func runLoop(ctx context.Context, o *Orchestrator, cfg *Config, sweep bool) {
```

Update the doc comment's final sentence (`main.go:130`) from:

```go
// Returns when the context is cancelled or after a single cycle when once is set.
```

to:

```go
// Returns only when the context is cancelled, draining in-flight pipelines
// via o.Wait() on that path before returning.
```

Delete the `if once { ... }` block (`main.go:151-156`):

```go
		if once {
			// -once fills slots once and drains them; it does not top up as
			// pipelines complete.
			o.Wait()
			return
		}
```

The `select` on `ctx.Done()` / `time.After(...)` immediately below it stays exactly as-is.

- [ ] **Step 4: Rewrite `main` and add `usage`**

Replace the entire `main` function (`main.go:21-102`) with:

```go
func main() {
	fs := flag.NewFlagSet("loope", flag.ContinueOnError)
	var help bytes.Buffer
	fs.SetOutput(&help)
	configPath := fs.String("config", "", "path to config file (required)")
	showVersion := fs.Bool("version", false, "print the loope version and exit")
	doctor := fs.Bool("doctor", false, "run preflight checks, print the report, and exit")
	fs.Usage = func() { usage(fs, &help) }

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Stdout.Write(help.Bytes()) // -h/--help: usage to stdout, exit 0
			os.Exit(0)
		}
		os.Stderr.Write(help.Bytes()) // genuine parse error: to stderr, exit 2
		os.Exit(2)
	}

	switch resolveMode(*configPath, *showVersion, *doctor) {
	case modeVersion:
		fmt.Println("loope", version)
		return
	case modeHelp:
		usage(fs, os.Stdout)
		os.Exit(0)
	case modeDoctorNoConfig:
		fmt.Fprintln(os.Stderr, "--config is required")
		usage(fs, os.Stderr)
		os.Exit(2)
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Shutdown drains in-flight pipelines, which can take as long as the work
	// they are doing. Unregistering the handlers as soon as the first signal
	// lands restores default signal behaviour, so a second Ctrl-C (or SIGTERM)
	// terminates immediately instead of being swallowed by the handler that is
	// still installed until main returns.
	go func() {
		<-ctx.Done()
		stop()
	}()

	r := execRunner{}
	if code, proceed := gate(ctx, os.Stderr, r, cfg, *doctor); !proceed {
		os.Exit(code)
	}

	o := &Orchestrator{cfg: cfg, runner: r, gh: NewGitHub(r, cfg),
		wt: &Worktree{runner: r, repoPath: cfg.RepoPath, retry: cfg.GitHubRetry.policy()}}

	// The daemon owns the workDir exclusively. The lock both stops a second
	// daemon from stealing live ai-wip work and proves any ai-wip issue found at
	// startup is an orphan from a crashed run — which is why the sweep only runs
	// when the lock is held.
	release, err := acquireLock(cfg.WorkDir)
	if err != nil {
		log.Fatal(err)
	}
	defer release()

	srv, err := NewServer(r, cfg)
	if err != nil {
		log.Fatalf("dashboard: %v", err)
	}
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		httpSrv.Close()
	}()
	// The dashboard is auxiliary: it runs in a goroutine and a listener error is
	// logged, never fatal, so the worker keeps shipping PRs.
	go func() {
		log.Printf("progress dashboard on http://%s (reading %s)", cfg.Addr, cfg.WorkDir)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("dashboard server stopped: %v", err)
		}
	}()

	runLoop(ctx, o, cfg, true /* sweep */)
}

// usage prints a one-line description and the flag defaults to w. It backs the
// FlagSet's Usage func (so -h/--help and parse errors reach it) and is also
// called directly for the bare-invocation and --doctor-without-config paths.
func usage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintln(w, "loope — autonomous GitHub issue pipeline daemon")
	fmt.Fprintf(w, "\nUsage:\n  %s --config <FILE>\n\nFlags:\n", fs.Name())
	fs.SetOutput(w)
	fs.PrintDefaults()
}
```

- [ ] **Step 5: Fix the imports**

`main.go` now uses `bytes` and `errors`, and no longer needs anything removed. Update the import block (`main.go:3-15`) to:

```go
import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
)
```

- [ ] **Step 6: Run the full build and test suite**

Run: `go build ./... && go test ./...`
Expected: build succeeds; all packages PASS (including `TestRunLoopDrainsInFlightPipelinesOnCancel`, `TestResolveMode`, and every `TestGate*` unchanged).

- [ ] **Step 7: Smoke-test the CLI surface by hand**

```bash
go build -o /tmp/loope . && \
  /tmp/loope --version && \
  /tmp/loope --help; echo "help exit: $?"; \
  /tmp/loope; echo "bare exit: $?"; \
  /tmp/loope --doctor; echo "doctor-no-config exit: $?"
```

Expected:
- `--version` prints `loope dev` and exits 0.
- `--help` prints the `loope — autonomous …` banner + flag list to stdout; `help exit: 0`.
- bare `loope` prints the same usage to stdout; `bare exit: 0`.
- `--doctor` prints `--config is required` + usage to stderr; `doctor-no-config exit: 2`.

- [ ] **Step 8: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: single-config-mode CLI with always-on dashboard and lock"
```

---

## Task 4: Update examples and documentation

No code paths change here; this task updates the two example files and `README.md` so they match the new CLI. It ends the plan with a green `go build`/`go test` (unchanged) plus a doc-consistency grep.

**Files:**
- Modify: `loope.json.example:2-5`
- Modify: `launchd/com.loope.plist.example:16-21`
- Modify: `README.md` (multiple sections, exact edits below)

**Interfaces:** none (docs only).

- [ ] **Step 1: Add `addr` to the example config**

In `loope.json.example`, insert the `addr` line after `"eligibleLabel"`:

```json
  "repoPath": "/Users/you/src/your-repo",
  "repoSlug": "your-org/your-repo",
  "eligibleLabel": "ai-agent",
  "addr": "localhost:8080",
  "pollIntervalSec": 60,
```

- [ ] **Step 2: Update the launchd example**

In `launchd/com.loope.plist.example`, replace the `ProgramArguments` array (drop `-serve`, use `--config`):

```xml
	<key>ProgramArguments</key>
	<array>
		<string>/ABSOLUTE/PATH/TO/loope</string>
		<string>--config</string>
		<string>/ABSOLUTE/PATH/TO/loope.json</string>
	</array>
```

- [ ] **Step 3: README — install/version line**

Change `README.md` line ~30 from:

```markdown
source (see [Build and run](#build-and-run)). Check the installed version with
`loope -version`.
```

to:

```markdown
source (see [Build and run](#build-and-run)). Check the installed version with
`loope --version`.
```

- [ ] **Step 4: README — remove the `-once` sentence from the poll-cycle paragraph**

Change the paragraph at `README.md` ~64-68 from:

```markdown
A poll cycle does **not** wait for the pipelines it starts. It fills the free
`ticketsPerCycle` slots, returns, and polls again one interval later — so work
labelled while other pipelines are running is picked up as soon as a slot frees,
rather than at the end of a batch. `-once` fills the slots one time, waits for
them to drain, and exits.
```

to:

```markdown
A poll cycle does **not** wait for the pipelines it starts. It fills the free
`ticketsPerCycle` slots, returns, and polls again one interval later — so work
labelled while other pipelines are running is picked up as soon as a slot frees,
rather than at the end of a batch.
```

- [ ] **Step 5: README — replace the manual `-rework` recovery block with auto-resume**

Change `README.md` ~95-105 from:

```markdown
To recover a parked issue, resume its Claude session and drive it to a PR:

```bash
./loope -rework <N> -config loope.json
```

This resumes the saved session in the preserved worktree, finishes the work,
and ships the PR (swapping `ai-rework` → `ai-done`). It is idempotent — if it
fails again the issue stays `ai-rework` with the worktree intact, so you can
re-run it. If the worktree or session file is gone, remove the `ai-rework`
label to re-queue the issue from scratch.
```

to:

```markdown
Parked issues recover automatically: each poll cycle the daemon auto-resumes
resumable `ai-rework` issues (backoff-gated), continuing the saved Claude
session in the preserved worktree, finishing the work, and shipping the PR
(swapping `ai-rework` → `ai-done`). If the worktree or session file is gone,
remove the `ai-rework` label to re-queue the issue from scratch.
```

- [ ] **Step 6: README — doctor command uses `--` forms**

Change `README.md` ~128 from:

```bash
./loope -doctor -config loope.json
```

to:

```bash
./loope --doctor --config loope.json
```

- [ ] **Step 7: README — Build and run section**

Change `README.md` ~153-163 from:

````markdown
```bash
go build -o loope .
cp loope.json.example loope.json   # then edit repoPath / repoSlug / workDir
./loope -config loope.json -once   # single poll cycle, then exit
./loope -config loope.json         # daemon: poll every pollIntervalSec
```

`-once` is the easiest way to smoke-test a new config: with no eligible
issues it logs `watching …` and exits cleanly. The daemon shuts down
gracefully on Ctrl-C / SIGTERM; if a pipeline is interrupted mid-issue, the
failure path still cleans up labels and worktrees.
````

to:

````markdown
```bash
go build -o loope .
cp loope.json.example loope.json   # then edit repoPath / repoSlug / workDir
./loope --config loope.json        # daemon: poll every pollIntervalSec, serve the dashboard
```

`--config` is required — there is no default config path. The daemon shuts down
gracefully on Ctrl-C / SIGTERM; if a pipeline is interrupted mid-issue, the
failure path still cleans up labels and worktrees. To validate a new config
without starting the loop, run `./loope --doctor --config loope.json`.
````

- [ ] **Step 8: README — Progress dashboard section**

Change the heading and body at `README.md` ~165-180 from:

````markdown
## Progress dashboard (`loope -serve`)

`loope -serve` runs the poll loop **and** serves a live web dashboard from the
same process, so one command both picks up labeled issues and shows every
issue the loop has touched, its live state, and a full per-issue pipeline
timeline:

```bash
./loope -serve -config loope.json              # http://localhost:8080
./loope -serve -config loope.json -addr localhost:9000
```

| Flag     | Default          | Description                        |
|----------|------------------|------------------------------------|
| `-serve` | off              | Serve the dashboard while also running the poll loop |
| `-addr`  | `localhost:8080` | Address to listen on               |
````

to:

````markdown
## Progress dashboard

The daemon always serves a live web dashboard from the same process, so one
command both picks up labeled issues and shows every issue the loop has touched,
its live state, and a full per-issue pipeline timeline. The listen address is
the `addr` config field (default `localhost:8080`):

```bash
./loope --config loope.json                    # dashboard on http://localhost:8080
```

Point it elsewhere by setting `"addr": "localhost:9000"` in the config.
````

- [ ] **Step 9: README — drop the manual-rework aside in the model-caps paragraph**

Change `README.md` ~320-323 from:

```markdown
When a session hits one of these caps (`terminal_reason: max_turns`) or a Claude
usage/rate limit, the loop parks the issue as `ai-rework` with the cause noted in
the issue comment, and the daemon auto-resumes it (with backoff) once the limit
resets; `loope -rework <N>` still works for manual resumes.
```

to:

```markdown
When a session hits one of these caps (`terminal_reason: max_turns`) or a Claude
usage/rate limit, the loop parks the issue as `ai-rework` with the cause noted in
the issue comment, and the daemon auto-resumes it (with backoff) once the limit
resets.
```

- [ ] **Step 10: README — Always-on operation bullets**

Change the "Transient failures auto-resume" bullet at `README.md` ~364-368 from:

```markdown
- **Transient failures auto-resume.** An issue parked as `ai-rework` because of
  a Claude usage/rate limit, a turn/budget ceiling, or a network outage is
  retried automatically each poll cycle, with per-issue exponential backoff
  (5 min doubling to 60 min). Only genuine errors — anything else — stay parked
  for a human `loope -rework <N>`.
```

to:

```markdown
- **Transient failures auto-resume.** An issue parked as `ai-rework` because of
  a Claude usage/rate limit, a turn/budget ceiling, or a network outage is
  retried automatically each poll cycle, with per-issue exponential backoff
  (5 min doubling to 60 min). Only genuine errors — anything else — stay parked
  for a human to inspect.
```

Change the "Panics don't kill the loop" bullet at `README.md` ~377-379 from:

```markdown
- **Panics don't kill the loop.** A panic in one issue's pipeline parks that
  issue with the panic recorded; the daemon and sibling pipelines continue. In
  `-serve` mode a dashboard listener error is logged, never fatal.
```

to:

```markdown
- **Panics don't kill the loop.** A panic in one issue's pipeline parks that
  issue with the panic recorded; the daemon and sibling pipelines continue. A
  dashboard listener error is logged, never fatal.
```

- [ ] **Step 11: README — remove the manual-vs-daemon race note**

Change `README.md` ~384-385 from:

```markdown
If a daemon is running against the same workDir, prefer letting it auto-resume:
a manual `loope -rework <N>` races the daemon's own resume of the same issue.
```

to:

```markdown
Parked issues are resumed by the daemon that owns the workDir; there is no
manual resume entry point to race it.
```

- [ ] **Step 12: README — add the `addr` row to the config table**

In the config table (`README.md` ~230-244), add an `addr` row immediately after the `pollIntervalSec` row:

```markdown
| `pollIntervalSec` | no       | `60`       | Seconds between poll cycles                             |
| `addr`            | no       | `localhost:8080` | Address the progress dashboard listens on        |
```

- [ ] **Step 13: Verify no stale flag references remain**

Run: `grep -rn -E "\-once|\-serve|\-addr|\-rework|loope -version|loope -config|loope -doctor" README.md launchd/ loope.json.example`
Expected: no matches (empty output). If any line matches, fix it to the `--` form or remove it per the edits above.

- [ ] **Step 14: Confirm the build and tests are still green**

Run: `go build ./... && go test ./...`
Expected: build succeeds; all tests PASS (docs-only task must not regress anything).

- [ ] **Step 15: Commit**

```bash
git add README.md loope.json.example launchd/com.loope.plist.example
git commit -m "docs: rewrite CLI docs and examples for single-config mode"
```

---

## Self-Review

**1. Spec coverage:**

| Spec item | Task |
|---|---|
| `Config.Addr` field + `localhost:8080` default, optional | Task 1 |
| Flag set reduced to `--config`/`--version`/`--doctor` | Task 3 (Step 4) |
| `flag.ContinueOnError` + `ErrHelp` → usage-to-stdout, exit 0 | Task 3 (Steps 4–5) |
| Genuine parse error → stderr, exit 2 | Task 3 (Step 4, buffer flush) |
| `--version` prints version, config not read | Task 2 (`modeVersion` before `LoadConfig`) + Task 3 |
| bare `./loope` / `--help` → usage, exit 0 | Task 3 (`modeHelp`) |
| `--doctor` without `--config` → error, exit 2 | Task 3 (`modeDoctorNoConfig`) |
| Unconditional workDir lock + sweep | Task 3 (Step 4: `acquireLock` always; `runLoop(..., true)`) |
| Unconditional dashboard on `cfg.Addr` | Task 3 (Step 4) |
| `runLoop` drops `once` param, drains on cancel only | Task 3 (Steps 1–3) |
| Custom `flag.Usage` with top-line description | Task 3 (`usage` helper) |
| `loope.json.example` gains `"addr"` | Task 4 (Step 1) |
| launchd example drops `-serve`, uses `--config` | Task 4 (Step 2) |
| README CLI/flags/dashboard rewrite | Task 4 (Steps 3–12) |
| `main_test.go` drain test updated | Task 3 (Step 1) |
| `config_test.go` `Addr` default + honored | Task 1 |
| Existing `TestGate*` unaffected | Preserved (gate signature unchanged) |

No gaps found.

**2. Placeholder scan:** No `TBD`/`TODO`/"handle edge cases"/"similar to Task N" placeholders — every code step contains the literal code, and every command lists its expected output.

**3. Type consistency:** `cliMode`/`resolveMode` defined in Task 2 are consumed with identical names/signature in Task 3. `runLoop(ctx, o, cfg, sweep)` (Task 3 signature) matches the call in the rewritten `main` and in the Task 3 Step-1 test. `usage(fs *flag.FlagSet, w io.Writer)` is defined and called consistently. `Config.Addr` (Task 1) is read as `cfg.Addr` in Task 3. Consistent throughout.
