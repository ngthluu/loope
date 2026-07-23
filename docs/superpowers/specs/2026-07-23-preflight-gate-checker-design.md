# Preflight gate checker

Issue: #1 — "Enhance: Gate-checker when run this app"

## Problem

`loope` is a wrapper around a local toolchain. It shells out to `git`, `gh`,
`claude`, and `curl`, and its pipeline prompts are superpowers slash commands.
None of that is verified at startup: `main.go` goes straight from `LoadConfig`
to the poll loop.

When a dependency is missing the failure surfaces late and expensively. A
missing `claude` binary fails only once an issue has already been labeled
`ai-wip` and a worktree created; the issue is then parked as `ai-rework` with a
stack-trace-shaped comment. Worse, a missing superpowers plugin fails
*silently*: `pipeline_feature.go` and `pipeline_bug.go` send
`/superpowers:brainstorming`, `/superpowers:writing-plans`,
`/superpowers:executing-plans`, and `/superpowers:systematic-debugging` as
prompts, and without the plugin those are inert text. The session burns its
full model budget and produces nothing usable.

The fix is a preflight gate: verify the toolchain before any mode runs, and
when something is missing, print what is missing and how to fix it.

## Scope

In scope: verifying that the external tools loope invokes are installed, that
`gh` is authenticated, that the superpowers plugin is present, and that the
configured repo is reachable — "tools installed / access works".

Out of scope: Go (a build-time dependency only, not needed at run time),
creating missing labels automatically, and any change to how the pipeline
itself runs.

## Architecture

One new file `preflight.go` with its companion `preflight_test.go`. The only
other production changes are the wiring in `main.go` and a README section.

Every probe goes through the existing `Runner` interface — the single process
seam in this codebase — so the whole feature is testable against `fakeRunner`
with no real `git`/`gh`/`claude` on the machine.

```go
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

// Preflight runs every check in order and returns the results.
func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult

// ReportPreflight writes the human-readable report to w and reports whether
// any required check failed.
func ReportPreflight(w io.Writer, results []CheckResult) (failed bool)
```

Running and reporting are split deliberately. Tests assert on the structured
`[]CheckResult`; a single separate test covers rendering. `main.go` never
reaches into a check's internals.

Each probe runs under its own `context.WithTimeout` of 10 seconds, derived from
the context passed to `Preflight`, so a hung `gh` cannot stall startup
indefinitely. A timed-out probe is reported as a failure of its check with the
timeout named in `Detail`.

## Checks

Checks run in the order below. Dependency skipping keeps one missing binary
from producing a wall of cascading failures: a skipped check is reported as
`statusSkip`, is never fatal on its own (its blocker already is), and names the
blocker in `Detail`.

| # | Name | Probe | Severity | Skipped when |
|---|------|-------|----------|--------------|
| 1 | `git` | `git --version` | required | — |
| 2 | `gh` | `gh --version` | required | — |
| 3 | `gh auth` | `gh auth status` | required | check 2 failed |
| 4 | `claude` | `claude --version` | required | — |
| 5 | `superpowers` | `claude plugin list` | required | check 4 failed |
| 6 | `repoPath` | `git rev-parse --is-inside-work-tree` in `cfg.RepoPath` | required | check 1 failed |
| 7 | `repo access` | `gh repo view <repoSlug> --json name` | required | check 2 or 3 failed |
| 8 | `labels` | `gh label list --repo <repoSlug> --json name` | **warning** | check 7 failed |
| 9 | `curl` | `curl --version` | **warning** | — |

Notes on individual checks:

- **superpowers (5)** passes when `claude plugin list` stdout contains a line
  matching `superpowers@`. The probe must be run with `CLAUDE_CONFIG_DIR` set
  to `cfg.ClaudeConfigDir` when that field is non-empty — the same environment
  `Claude.Call` uses (`claude.go`). Without this, a user running under
  `claudeConfigDir: ~/.claude-personal` would get a false pass from the plugins
  installed in their default `~/.claude` profile.
- **labels (8)** compares the fetched label names against `cfg.EligibleLabel`
  plus all five `cfg.StateLabels` fields. Any missing label is a warning, not a
  failure, and the fix lines are one
  `gh label create <name> --repo <slug>` per missing label — the exact commands
  the README currently asks users to assemble by hand.
- **curl (9)** is a warning because `images.go` already degrades gracefully: a
  missing `curl` costs issue image attachments and nothing else.

Fix hints for the required binaries name both a package-manager command and the
official docs URL, e.g.:

- `git`: `brew install git` / `apt install git` / <https://git-scm.com/downloads>
- `gh`: `brew install gh` / <https://cli.github.com>
- `gh auth`: `gh auth login`
- `claude`: `npm install -g @anthropic-ai/claude-code` /
  <https://docs.anthropic.com/en/docs/claude-code>
- `superpowers`: `claude plugin install superpowers@claude-plugins-official`

## Report format

The report is written to **stderr**:

```
loope preflight

  ✔ git           git version 2.39.5
  ✔ gh            gh version 2.63.2
  ✘ gh auth       not authenticated
      → gh auth login
  ✔ claude        2.0.1 (Claude Code)
  ✘ superpowers   plugin not installed (CLAUDE_CONFIG_DIR=~/.claude-personal)
      → claude plugin install superpowers@claude-plugins-official
  ✔ repoPath      /Users/you/src/your-repo
  - repo access   skipped (gh auth failed)
  - labels        skipped (repo access failed)
  ! curl          not found — issue image attachments will be skipped

2 required checks failed. Fix them and re-run `loope -doctor` to verify.
```

Status markers: `✔` ok, `✘` required failure, `!` warning, `-` skipped. When
every required check passes, the trailing summary line is omitted and, outside
`-doctor`, nothing is printed at all — a healthy machine sees no new output.

## Wiring

In `main.go`, the gate runs immediately after `LoadConfig` and before the
`-rework` branch, the `acquireLock` call, and the `-serve` setup, so every mode
is gated by the same code path.

- On any required failure: print the report to stderr and exit 1.
- `-version` continues to short-circuit before config loading and the gate, so
  it still works on a bare machine.
- New `-doctor` flag: run the checks, print the report unconditionally (even
  when everything passes), then return. Exit 1 when a required check failed,
  0 otherwise. Warnings never affect the exit code. `-doctor` returns before
  the lock is acquired and before the loop starts.
- `-doctor` requires a valid config and fails with the existing `LoadConfig`
  error when there is none. Checks 5–8 read `claudeConfigDir`, `repoPath`,
  `repoSlug`, and the label names, so a config-less run could only produce a
  partial report; a clear "fix your config first" is better than half a report.

## Testing

All tests drive `fakeRunner` (`helpers_test.go`) via its `handler` field, keyed
on `rcall.name` and `rcall.args`. No real binaries are involved.

`Preflight` table tests:

- every check passes → all `statusOK`, no failures
- each required binary missing in turn → that check `statusFail`, dependent
  checks `statusSkip`
- `gh` present but `gh auth status` fails → `gh auth` fails, `repo access` and
  `labels` skip
- `claude` present, `claude plugin list` without a `superpowers@` line →
  `superpowers` fails
- `claude plugin list` is invoked with `CLAUDE_CONFIG_DIR=<claudeConfigDir>` in
  its env when the config sets one, and with no such env var when it does not
- `repoPath` not a git worktree → `repoPath` fails
- `gh label list` missing some configured labels → `labels` is `statusWarn`
  with one `gh label create` fix line per missing label, and the run is not
  fatal
- `curl` missing → `statusWarn`, not fatal
- a skipped check is not reported as a second failure

`ReportPreflight` test: given a fixed `[]CheckResult` covering all four
statuses, assert the rendered text and the returned `failed` boolean.

Wiring test: `-doctor` exits non-zero when a required check fails and zero when
only warnings are present.

## Documentation

The README's Prerequisites section gains a short paragraph: loope verifies this
toolchain at startup and refuses to run when a required piece is missing, and
`loope -doctor -config loope.json` runs the same checks standalone. The manual
`gh label create` block stays, cross-referenced from the labels warning.
