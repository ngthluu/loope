# loope — event-driven loop

[![CI](https://github.com/ngthluu/loope/actions/workflows/ci.yml/badge.svg)](https://github.com/ngthluu/loope/actions/workflows/ci.yml)
[![Release](https://github.com/ngthluu/loope/actions/workflows/release.yml/badge.svg)](https://github.com/ngthluu/loope/actions/workflows/release.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go 1.25+](https://img.shields.io/badge/Go-1.25%2B-00ADD8.svg)](go.mod)

`loope` is an event-driven loop that watches one GitHub repository for issues
labeled `ai-agent`, picks the best one, and drives it all the way to a pull
request using headless
[Claude Code](https://docs.anthropic.com/en/docs/claude-code) sessions running
inside git worktrees. Issue state lives entirely in GitHub labels, so the
daemon itself is stateless and safe to restart.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/ngthluu/loope/main/install.sh | sh
```

This downloads the prebuilt binary for your OS/arch from the
[latest release](https://github.com/ngthluu/loope/releases/latest), verifies its
checksum, and installs it to `/usr/local/bin` (override with
`LOOPE_INSTALL_DIR`, pin a version with `LOOPE_VERSION=v0.1.0`). Binaries are
published for macOS and Linux on `amd64` and `arm64`.

Prefer to do it yourself? Grab an archive from the
[releases page](https://github.com/ngthluu/loope/releases), or build from
source (see [Build and run](#build-and-run)). Check the installed version with
`loope -version`.

> loope is a wrapper around your local toolchain: it needs `git`, `gh`
> (authenticated), and `claude` on your `PATH` at run time. See
> [Prerequisites](#prerequisites).

## How it works

Each poll cycle:

1. **List** open issues carrying the eligible label (default `ai-agent`) that
   don't yet have a state label.
2. **Triage** — a Claude agent picks the single best issue and classifies it:
   - `bug`: small, well-scoped defect → one systematic-debugging session that
     reproduces with a failing test, fixes, and commits.
   - `feature`: anything needing design → three sessions. An architect
     brainstorm session scores how confidently the issue can be implemented
     and, below `confidenceThreshold`, escalates it to `ai-needs-info` instead
     of guessing (see below); otherwise it brainstorms with a cheaper "product
     owner proxy" agent in a Q&A loop, then writes and commits the spec. A
     **fresh** session turns that spec into a committed implementation plan,
     and a third session executes the plan.
   - `done`: the work is already fully implemented in the codebase → the loop
     comments, applies the `ai-done` label, and closes the issue without
     opening a PR.
3. **Work** happens on branch `ai/issue-<N>` in a dedicated git worktree under
   `workDir`, created from the remote default branch.
4. **Ship** — if the pipeline produced at least one commit, the branch is
   pushed and a PR is opened (`Closes #N`); the PR URL is commented on the
   issue.

A poll cycle does **not** wait for the pipelines it starts. It fills the free
`ticketsPerCycle` slots, returns, and polls again one interval later — so work
labelled while other pipelines are running is picked up as soon as a slot frees,
rather than at the end of a batch. `-once` fills the slots one time, waits for
them to drain, and exits.

Within a cycle, auto-resumes of parked issues claim slots **before** new
eligible issues: continuing work that already has a worktree and session on disk
outranks starting more of it, so a permanently busy queue can't starve a parked
issue. Resumes are backoff-gated, so they leave the rest of the budget for new
work.

On shutdown (Ctrl-C / SIGTERM) the daemon stops polling and waits for in-flight
pipelines to finish, so the `workDir` lock is never released while a pipeline is
live. Signal a second time to quit immediately without draining.

Label lifecycle (names configurable, see below):

| Label       | Meaning                                              |
|-------------|------------------------------------------------------|
| `ai-agent`  | You add this: the issue is eligible for the loop     |
| `ai-wip`    | The loop is working on it                            |
| `ai-done`   | PR created; issue leaves the queue                   |
| `ai-rework` | Pipeline hit an error; progress preserved for manual rework      |
| `ai-needs-info` | Brainstorm wasn't confident enough; awaiting author clarification |

On failure the loop comments the error on the issue, swaps `ai-wip` →
`ai-rework`, and **preserves** the worktree, branch, logs, and the Claude
session id (saved in `logs/issue-<N>/session`). Nothing is deleted, so no
progress is lost.

To recover a parked issue, resume its Claude session and drive it to a PR:

```bash
./loope -rework <N> -config loope.json
```

This resumes the saved session in the preserved worktree, finishes the work,
and ships the PR (swapping `ai-rework` → `ai-done`). It is idempotent — if it
fails again the issue stays `ai-rework` with the worktree intact, so you can
re-run it. If the worktree or session file is gone, remove the `ai-rework`
label to re-queue the issue from scratch.

> `ai-failed` is deprecated: the loop no longer applies it, though existing
> `ai-failed` issues are still recognized and stay out of the queue.

## Prerequisites

- **Go 1.25+** to build.
- **git**, with the target repo cloned locally.
- **gh** (GitHub CLI), authenticated (`gh auth login`) with permission to
  edit issues, push branches, and open PRs on the target repo.
- **claude** (Claude Code CLI), logged in.
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

> **Warning:** pipeline sessions run with `--dangerously-skip-permissions` so
> they can work unattended. Only point the loop at repositories where you are
> comfortable with an autonomous agent reading, running, and committing code.

The state labels and the eligible label must exist in the repo before the
loop can apply them — the `labels` preflight check warns with exactly these
commands when any are missing:

```bash
gh label create ai-agent  --repo your-org/your-repo
gh label create ai-wip    --repo your-org/your-repo
gh label create ai-done   --repo your-org/your-repo
gh label create ai-rework --repo your-org/your-repo
gh label create ai-needs-info --repo your-org/your-repo
```

## Build and run

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

The dashboard side rebuilds the view from two sources: the `logs/issue-<N>/`
artifacts on disk and current issue label/title state from `gh` (TTL-cached for
a few seconds so labels added after startup appear without a restart). A
master-detail page lists tickets in the left rail (auto-refreshing every few
seconds); selecting one shows its steps with expandable prompt and output,
per-step cost and Claude session id, and totals. The worker side is the same
poll loop as the plain daemon, so it swaps labels, opens PRs, and writes under
`logs/` exactly as `./loope` does — both stop together on a signal. If the
dashboard listener fails, the worker keeps running; the error is only logged.

### Web assets

The dashboard's front end lives in `web/`: Go templates in `web/templates/`,
and htmx, the idiomorph morph extension, `app.js` and the compiled stylesheet
in `web/static/`. Everything there is embedded with `go:embed`, so the release
binary stays self-contained and `go build` remains the only build command —
there is no Node, npm, or asset pipeline in CI. Editing a template or script
needs a rebuild, which takes about a second.

Styling is Tailwind CSS v4, compiled ahead of time with the [standalone
CLI](https://tailwindcss.com/blog/standalone-cli) (a single binary — no npm)
and **committed** as `web/static/app.css`. The source is `web/tailwind.css`.
Regenerate after changing any Tailwind class:

```bash
tailwindcss -i web/tailwind.css -o web/static/app.css --minify
```

⚠️ This step is manual, and it matters for **both** class sources: the
templates *and* the Go helpers (`stripeClass`, `nodeClass`, `cardClass`,
`divClass`, `statusChip`) that build class strings in `render.go`. Skipping it
ships a half-styled dashboard. `TestAppCSSCoversBothClassSources` in
`serve_test.go` fails if the regeneration was forgotten.

If `gh` is unreachable, the page still renders from local logs and shows a
"GitHub unreachable" banner. The server shuts down cleanly on Ctrl-C /
SIGTERM. Bind stays on `localhost` by default since the dashboard exposes
prompt/output content.

## Configuration

All settings live in one JSON file (see `loope.json.example`). Paths may start
with `~/`.

| Field             | Required | Default    | Description                                             |
|-------------------|----------|------------|---------------------------------------------------------|
| `repoPath`        | yes      | —          | Local clone of the target repo (worktrees branch from it) |
| `repoSlug`        | yes      | —          | `org/repo` used for all `gh` calls                      |
| `workDir`         | yes      | —          | Where worktrees and logs are created                    |
| `eligibleLabel`   | no       | `ai-agent` | Label that marks an issue as available to the loop      |
| `pollIntervalSec` | no       | `60`       | Seconds between poll cycles                             |
| `ticketsPerCycle` | no       | `1`        | Maximum number of pipelines running concurrently. Each poll cycle tops the in-flight set back up to this limit from the eligible queue, so a newly labelled issue starts within one poll interval whenever a slot is free. Auto-resumes of parked issues draw from the same limit and claim from it first. Values below 1 are treated as 1 |
| `personaPath`     | no       | —          | Markdown persona for the answerer agent (see `persona.example.md`) |
| `claudeConfigDir` | no       | —          | Claude Code profile dir; sets `CLAUDE_CONFIG_DIR` for every `claude` call (see below) |
| `maxQARounds`     | no       | `20`       | Max architect↔answerer rounds before a feature fails    |
| `confidenceThreshold` | no       | `70`       | Brainstorm confidence (0–100) below which an issue is escalated to `needsInfo` instead of implemented; `0` disables the gate |
| `stateLabels`     | no       | see below  | Names of the state labels (including `needsInfo`)       |
| `githubRetry`     | no       | see below  | Retry policy for transient GitHub failures              |
| `models`          | no       | —          | Per-role model settings (see below)                     |

### `stateLabels`

The state labels are configurable; unset fields keep their defaults:

```json
"stateLabels": {"wip": "ai-wip", "failed": "ai-failed", "done": "ai-done", "rework": "ai-rework", "needsInfo": "ai-needs-info"}
```

Partial overrides work — `{"wip": "bot-wip"}` renames only the WIP label.
If you change these on a live repo, migrate any issues still carrying the old
label names: the loop only recognizes the configured names, so an issue with
a stale label is treated as eligible again.

### Confidence gate

Before designing anything, the feature pipeline's brainstorm session scores how
confidently the issue can be implemented as written (0–100). When that score is
below `confidenceThreshold` (default `70`), the loop does **not** guess: it
comments the score and the architect's specific questions on the issue, applies
the `ai-needs-info` label, removes the worktree, and stops. The issue leaves the
queue and is **not** auto-resumed — a human answers the questions and removes the
`ai-needs-info` label, which re-queues the issue from scratch. Set
`confidenceThreshold` to `0` to disable the gate and always attempt an
implementation.

### `githubRetry`

Retries transient GitHub failures — rate limits, HTTP 5xx errors, and network errors on `gh`, `git fetch`, and `git push`. Permanent errors (not-found, already-exists, auth) fail immediately. Unset fields keep their defaults:

```json
"githubRetry": {"maxAttempts": 0, "baseDelaySec": 2, "maxDelaySec": 60}
```

- `maxAttempts` (default `0`): `0` means retry until success or shutdown ("until GitHub is live"); a positive number caps the number of attempts.
- `baseDelaySec` (default `2`): initial backoff in seconds, doubled with each attempt.
- `maxDelaySec` (default `60`): backoff ceiling in seconds.

### `claudeConfigDir`

By default `claude` uses the `~/.claude` profile (accounts, settings, MCP
servers, logins). Set `claudeConfigDir` to run the loop under a separate
profile without disturbing your interactive one:

```json
"claudeConfigDir": "~/.claude-personal"
```

The loop exports it as `CLAUDE_CONFIG_DIR` on every `claude` invocation, so
that directory must already be a logged-in Claude Code profile. Leave it unset
to inherit whatever `CLAUDE_CONFIG_DIR` (or the default `~/.claude`) the daemon
was started with. `~` is expanded.

### `models`

Four roles, each `{model, effort, maxBudgetUSD, maxTurns}`:

- `architect` — the heavy lifter: brainstorms, plans, debugs, and (unless
  `execute` overrides it) executes.
- `answerer` — the product-owner proxy answering the architect's questions.
- `triage` — picks and classifies the next issue.
- `execute` — optional. The feature pipeline's plan-execution step, which
  implements the whole plan in one session and so usually wants a much higher
  `maxTurns`/`maxBudgetUSD` than the bounded architect Q&A rounds. Any field
  left unset inherits from `architect`, so omitting the block entirely keeps the
  old behavior (execute runs with the architect config).

`maxBudgetUSD` and `maxTurns` are passed straight to the `claude` CLI as
hard caps per session; `0` omits the cap. `effort` maps to `--effort`.

When a session hits one of these caps (`terminal_reason: max_turns`) or a Claude
usage/rate limit, the loop parks the issue as `ai-rework` with the cause noted in
the issue comment, and the daemon auto-resumes it (with backoff) once the limit
resets; `loope -rework <N>` still works for manual resumes.

### Persona

`personaPath` points at a markdown file describing how the answerer should
make product decisions (bias to simplicity, testing requirements, PR size
limits, …). Missing file is fine — the answerer just runs without one.
`persona.example.md` is a reasonable starting point.

## Logs

Every Claude call is saved for postmortems. Each call writes three files to
the issue's log dir: the prompt (`NNN-<label>.prompt.md`), the model's result
text (`NNN-<label>.output.md`), and the raw CLI JSON (`NNN-<label>.json`):

```
<workDir>/logs/triage/NNN-triage.{prompt.md,output.md,json}          # one per poll cycle
<workDir>/logs/issue-<N>/NNN-<label>.{prompt.md,output.md,json}      # brainstorm-*, answer-*, plan, execute, debug
```

Numbering continues across restarts; nothing is overwritten.

## Always-on operation

The daemon is designed to run until you stop it:

- **Transient failures auto-resume.** An issue parked as `ai-rework` because of
  a Claude usage/rate limit, a turn/budget ceiling, or a network outage is
  retried automatically each poll cycle, with per-issue exponential backoff
  (5 min doubling to 60 min). Only genuine errors — anything else — stay parked
  for a human `loope -rework <N>`.
- **Crashes self-heal on restart.** On startup the daemon sweeps issues left in
  `ai-wip` by a crashed run. If the worktree and a recorded Claude session
  survived, the run is resumable: the issue is parked as `ai-rework` with its
  worktree intact and auto-resumed, so the crash costs no pipeline work. Only
  when nothing resumable remains are the leftover worktree/branch removed and
  the label stripped to re-queue the issue from scratch. No manual cleanup.
- **One daemon per workDir.** A pid lock at `<workDir>/logs/daemon.lock`
  refuses a second instance while one is alive and is taken over when stale.
- **Panics don't kill the loop.** A panic in one issue's pipeline parks that
  issue with the panic recorded; the daemon and sibling pipelines continue. In
  `-serve` mode a dashboard listener error is logged, never fatal.

GitHub stays current throughout: labels, comments, and PRs are retried with
backoff (see `githubRetry`) until connectivity returns.

If a daemon is running against the same workDir, prefer letting it auto-resume:
a manual `loope -rework <N>` races the daemon's own resume of the same issue.

## Run as a service (macOS)

To have launchd start the daemon at login and restart it if it ever dies:

1. `go build -o loope .`
2. Copy `launchd/com.loope.plist.example` to
   `~/Library/LaunchAgents/com.loope.plist` and replace the placeholder
   paths (binary, config, log dir, `PATH`, `HOME`).
3. `launchctl bootstrap gui/$UID ~/Library/LaunchAgents/com.loope.plist`

Logs land in `~/Library/Logs/loope/`. Stop it with
`launchctl bootout gui/$UID/com.loope`. `KeepAlive` and the daemon lock
compose safely: if you also start `./loope` by hand while the service runs, the
second copy exits immediately with a "another loop instance" error.

## Development

```bash
go test ./...                                  # unit tests (no network, no CLIs)
go test -tags integration -run TestIntegrationTriage -v   # real claude CLI smoke test
```

All process execution goes through the `Runner` interface (`runner.go`);
tests inject a fake, so the suite runs without git/gh/claude installed.

## Releasing

Releases are cut by [GoReleaser](https://goreleaser.com) from a pushed tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `Release` workflow builds the darwin/linux · amd64/arm64 binaries, uploads
them plus `checksums.txt` to a GitHub Release, and the `install.sh` one-liner
picks them up automatically. Dry-run the build locally with
`goreleaser release --snapshot --clean`.

## Contributing

Issues and pull requests are welcome. CI (`go build`, `go vet`, `go test ./...`)
must pass; please keep new behavior covered by tests that run without the
network or external CLIs.

## License

[MIT](LICENSE) © ngthluu
