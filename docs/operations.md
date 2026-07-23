# Operations

## Always-on operation

The daemon is designed to run until you stop it:

- **Transient failures auto-resume.** An issue parked as `ai-rework` because of a
  Claude usage/rate limit, a turn/budget ceiling, or a network outage is retried
  automatically each poll cycle, with per-issue exponential backoff (5 min
  doubling to 60 min). Only genuine errors — anything else — stay parked for a
  human to inspect.
- **Crashes self-heal on restart.** On startup the daemon sweeps issues left in
  `ai-wip` by a crashed run. If the worktree and a recorded Claude session
  survived, the run is resumable: the issue is parked as `ai-rework` with its
  worktree intact and auto-resumed, so the crash costs no pipeline work. Only
  when nothing resumable remains are the leftover worktree/branch removed and the
  label stripped to re-queue the issue from scratch. No manual cleanup.
- **One daemon per workDir.** A pid lock at `<workDir>/logs/daemon.lock` refuses a
  second instance while one is alive and is taken over when stale.
- **Panics don't kill the loop.** A panic in one issue's pipeline parks that issue
  with the panic recorded; the daemon and sibling pipelines continue. A dashboard
  listener error is logged, never fatal.

GitHub stays current throughout: labels, comments, and PRs are retried with
backoff (see [`githubRetry`](configuration.md#githubretry)) until connectivity
returns.

Parked issues are resumed by the daemon that owns the workDir; there is no manual
resume entry point to race it.

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
