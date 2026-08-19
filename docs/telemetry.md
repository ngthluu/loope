# Fleet telemetry server

Running `loope` across several machines or repos normally means opening N
terminals or N per-repo dashboards, with no single place to see which workers
are online, what they're doing, or how close each is to its Claude Code usage
limit. `loope-telemetry-server` — its own binary since the monorepo split
(`telemetry-server/`) — is a central, opt-in view across all of them.

## Running the server

```bash
loope-telemetry-server -addr :9090 -token your-shared-secret
```

Or with Docker. Every release publishes a multi-arch image to GHCR:

```bash
docker run --rm -p 9090:9090 ghcr.io/ngthluu/loope-telemetry-server:latest \
  -token your-shared-secret
```

`:latest` always tracks the newest **stable** release — release candidates
never move it. To run an rc build, use its versioned tag (the release version
without the `v` prefix):

```bash
docker run --rm -p 9090:9090 ghcr.io/ngthluu/loope-telemetry-server:0.2.2-rc.1 \
  -token your-shared-secret
```

To build the image from source instead (from the repo root — the build needs
`shared/`):

```bash
docker build -f telemetry-server/Dockerfile -t loope-telemetry-server .
docker run --rm -p 9090:9090 loope-telemetry-server -token your-shared-secret
```

No volume is needed (state is in-memory) and the image terminates no TLS —
put a reverse proxy in front for HTTPS.

`-token` is required — it is the shared bearer token every worker must send.
`-addr` defaults to `:9090`. `-data-dir` is accepted for forward compatibility
with future persistence but is not used today: the telemetry server's state is
entirely in-memory, so a restart shows a blank fleet until workers' next push
repopulates it (matching `loope`'s "state lives in GitHub / gets rebuilt"
philosophy for its per-repo dashboard).

Open `http://<host>:9090` for the fleet dashboard: workers are grouped by
`repoSlug`, each with an online/offline badge, current 5-hour/7-day Claude
usage, and a live tail of its daemon log.

## Opting a worker in

Add a `telemetry` block to that worker's `loope.json`:

```json
"telemetry": {
  "serverURL": "http://telemetry-host:9090",
  "token": "your-shared-secret",
  "pushIntervalSec": 15
}
```

`pushIntervalSec` defaults to 15 if omitted. When the block is absent
entirely, nothing changes: no exporter goroutine starts, and no extra network
calls are made. When present, the daemon also starts writing its own log
output to `<workDir>/logs/daemon.log` (in addition to stderr), rotating at
10MB with one previous generation kept — this is what the exporter tails and
pushes.

## Capturing Claude usage (optional)

The 5-hour/7-day usage numbers come from the JSON Claude Code feeds to a
configured `statusLine` command. Wire it up with:

```bash
loope status-line --config /path/to/loope.json
```

This wraps (or sets, if none exists) your `~/.claude/settings.json`
`statusLine` command so it also feeds `loope claude-usage-hook`, without
disturbing whatever your existing statusline already shows. Run it again
with `--remove` to undo. Under the hood, this produces (or you can wire up
by hand instead, for example to review or tweak the wrapping) something like:

```bash
# ~/.claude/settings.json
"statusLine": {
  "type": "command",
  "command": "bash -c 'tee >('\\''/path/to/loope'\\'' claude-usage-hook) | /path/to/your/real-statusline.sh'"
}
```

`loope claude-usage-hook` writes the latest rate-limit snapshot to
`~/.claude/loope-usage.json` and prints nothing, so it never affects what your
real statusline displays. If this file is missing, or its capture is older
than 30 minutes, the dashboard shows "usage: unknown" for that worker rather
than a stale or fabricated number — whether headless `claude -p` runs (how
loope's own pipeline steps invoke Claude) trigger the statusLine hook the same
way interactive sessions do is unconfirmed; the degraded "unknown" state is
what you'll see until that's verified on your setup.
