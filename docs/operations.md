# Operations

## Always-on operation

The daemon is designed to run until you stop it. Each behavior below is one edge
of [the status state machine](how-it-works.md#status-state-machine) — this page
covers what it means to operate, that page covers the full set of transitions.

- **Failures stop, they don't retry.** Whatever the cause — a Claude usage/rate
  limit, a turn/budget ceiling, a network outage, a genuine error — the issue is
  parked as `ai-rework` with the full error commented on it, and the daemon does
  **not** touch it again. Retrying a failure every cycle re-ran the whole
  pipeline on the same broken issue and burned tokens on it indefinitely. To get
  another attempt, remove the `ai-rework` label: the issue becomes eligible again
  and the next run reuses the preserved worktree and branch, so no work is lost.
- **Crashes self-heal on restart.** On startup the daemon sweeps issues left in
  `ai-wip` by a crashed run: it strips the label so the issue is eligible again,
  and leaves the worktree, branch, logs and session exactly where they are. The
  next run reuses that worktree, so the crashed run's commits are built on rather
  than thrown away. No manual cleanup, and no state to un-stick by hand.
- **One daemon per workDir.** A pid lock at `<workDir>/logs/daemon.lock` refuses a
  second instance while one is alive and is taken over when stale.
- **Panics don't kill the loop.** A panic in one issue's pipeline parks that issue
  with the panic recorded; the daemon and sibling pipelines continue. A dashboard
  listener error is logged, never fatal.

GitHub stays current throughout: labels, comments, and PRs are retried with
backoff (see [`githubRetry`](configuration.md#githubretry)) until connectivity
returns.

`ai-rework` is a dead end for the daemon: nothing moves an issue out of it
except you removing the label. There is no resume scan and no manual resume
entry point, so a parked issue cannot be picked up twice or raced by a second
process.

## Run as a service (macOS)

To have launchd start the daemon at login and restart it if it ever dies:

1. `go build -o loope .`
2. Copy `launchd/com.loope.plist.example` to
   `~/Library/LaunchAgents/com.loope.plist` and replace the placeholder paths
   (binary, config, log dir, `PATH`, `HOME`).
3. `launchctl bootstrap gui/$UID ~/Library/LaunchAgents/com.loope.plist`

Logs land in `~/Library/Logs/loope/`. Stop it with
`launchctl bootout gui/$UID/com.loope`. `KeepAlive` and the daemon lock compose
safely: if you also start `./loope` by hand while the service runs, the second
copy exits immediately with a "another loop instance" error.
