# Configuration

All settings live in one JSON file (see `loope.json.example`). Paths may start
with `~/`.

| Field                 | Required | Default          | Description                                             |
|-----------------------|----------|------------------|---------------------------------------------------------|
| `repoPath`            | yes      | —                | Local clone of the target repo (worktrees branch from it) |
| `repoSlug`            | yes      | —                | `org/repo` used for all `gh` calls                      |
| `workDir`             | yes      | —                | Where worktrees and logs are created                    |
| `eligibleLabel`       | no       | `ai-agent`       | Label that marks an issue as available to the loop      |
| `pollIntervalSec`     | no       | `60`             | Seconds between poll cycles                             |
| `addr`                | no       | `localhost:8080` | Address the progress dashboard listens on               |
| `ticketsPerCycle`     | no       | `1`              | Maximum pipelines running concurrently. Each poll cycle tops the in-flight set back up to this limit from the eligible queue, so a newly labelled issue starts within one poll interval whenever a slot is free. Auto-resumes of parked issues draw from the same limit and claim from it first. Values below 1 are treated as 1 |
| `personaPath`         | no       | —                | Markdown persona for the answerer agent (see `persona.example.md`) |
| `claudeConfigDir`     | no       | —                | Claude Code profile dir; sets `CLAUDE_CONFIG_DIR` for every `claude` call ([details](#claudeconfigdir)) |
| `maxQARounds`         | no       | `20`             | Max architect↔answerer rounds before a feature fails    |
| `confidenceThreshold` | no       | `70`             | Confidence score (0–100) below which an issue is escalated to `needsInfo` instead of implemented, on both the bug and feature routes; `0` disables the gate |
| `stateLabels`         | no       | see below        | Names of the state labels (including `needsInfo`)       |
| `githubRetry`         | no       | see below        | Retry policy for transient GitHub failures              |
| `models`              | no       | —                | Per-role model settings ([details](#models))            |

## `stateLabels`

The state labels are configurable; unset fields keep their defaults:

```json
"stateLabels": {"wip": "ai-wip", "failed": "ai-failed", "done": "ai-done", "rework": "ai-rework", "needsInfo": "ai-needs-info"}
```

Partial overrides work — `{"wip": "bot-wip"}` renames only the WIP label. If you
change these on a live repo, migrate any issues still carrying the old label
names: the loop only recognizes the configured names, so an issue with a stale
label is treated as eligible again.

## Confidence gate

Both routes score how confidently the issue can be implemented as written
(0–100) before committing to an implementation. The feature pipeline's
brainstorm session scores from the issue text, before designing anything. The
bug pipeline's debug session may read the codebase first — a terse bug report can
still be trivially fixable once you open the file — but writes nothing until
after it has scored.

When that score is below `confidenceThreshold` (default `70`), the loop does
**not** guess: it comments the score and the session's specific questions on the
issue, applies the `ai-needs-info` label, removes the worktree, and stops. The
issue leaves the queue and is **not** auto-resumed — a human answers the
questions and removes the `ai-needs-info` label, which re-queues the issue from
scratch. Set `confidenceThreshold` to `0` to disable the gate on both routes and
always attempt an implementation.

## `githubRetry`

Retries transient GitHub failures — rate limits, HTTP 5xx errors, and network
errors on `gh`, `git fetch`, and `git push`. Permanent errors (not-found,
already-exists, auth) fail immediately. Unset fields keep their defaults:

```json
"githubRetry": {"maxAttempts": 0, "baseDelaySec": 2, "maxDelaySec": 60}
```

- `maxAttempts` (default `0`): `0` means retry until success or shutdown ("until
  GitHub is live"); a positive number caps the number of attempts.
- `baseDelaySec` (default `2`): initial backoff in seconds, doubled with each
  attempt.
- `maxDelaySec` (default `60`): backoff ceiling in seconds.

## `claudeConfigDir`

By default `claude` uses the `~/.claude` profile (accounts, settings, MCP
servers, logins). Set `claudeConfigDir` to run the loop under a separate profile
without disturbing your interactive one:

```json
"claudeConfigDir": "~/.claude-personal"
```

The loop exports it as `CLAUDE_CONFIG_DIR` on every `claude` invocation, so that
directory must already be a logged-in Claude Code profile. Leave it unset to
inherit whatever `CLAUDE_CONFIG_DIR` (or the default `~/.claude`) the daemon was
started with. `~` is expanded.

## `models`

Four roles, each `{model, effort, maxBudgetUSD, maxTurns}`:

- `architect` — the heavy lifter: brainstorms, plans, debugs, and (unless
  `execute` overrides it) executes.
- `answerer` — the product-owner proxy answering the architect's questions.
- `triage` — picks and classifies the next issue.
- `execute` — optional. The feature pipeline's plan-execution step, which
  implements the whole plan in one session and so usually wants a much higher
  `maxTurns`/`maxBudgetUSD` than the bounded architect Q&A rounds. Any field left
  unset inherits from `architect`, so omitting the block entirely keeps the old
  behavior (execute runs with the architect config).

`maxBudgetUSD` and `maxTurns` are passed straight to the `claude` CLI as hard
caps per session; `0` omits the cap. `effort` maps to `--effort`.

When a session hits one of these caps (`terminal_reason: max_turns`) or a Claude
usage/rate limit, the loop parks the issue as `ai-rework` with the cause noted in
the issue comment, and the daemon auto-resumes it (with backoff) once the limit
resets.

## Persona

`personaPath` points at a markdown file describing how the answerer should make
product decisions (bias to simplicity, testing requirements, PR size limits, …).
Missing file is fine — the answerer just runs without one. `persona.example.md`
is a reasonable starting point.
