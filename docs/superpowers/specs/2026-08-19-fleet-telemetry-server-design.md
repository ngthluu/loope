# Fleet telemetry server: watch every worker's logs, process, and Claude usage

Issue: #57

## Problem

Each `loope` daemon runs on its own machine, watching one repo, with its own
local dashboard (`serve.go`) showing that machine's ticket queue and pipeline
steps. Running loope across several machines/repos means opening N terminals
or N dashboards, with no single place to see which machines are online, what
they're currently doing, or how close each is to its Claude Code usage limit.

## Goal

A central `loope telemetry-server` that N local `loope` daemons ("workers")
push to, showing — grouped by project (`repoSlug`) — which workers are
online, a tail of each worker's daemon log, and each worker's Claude Code
usage (5-hour / 7-day rate-limit percentage and reset time), with graceful
degradation when usage data isn't available.

## Decisions (from the issue author, via clarifying rounds)

1. **Wire format inspiration**: imitate Genkit's data *shape* — resource
   attributes + structured log records — not its OpenTelemetry SDK or OTLP
   wire bytes. No new OTel dependency.
2. **Transport**: push, not poll — workers may be on different
   networks/laptops, so the server must not need inbound reachability to
   them. Best-practice choice delegated to this design.
3. **Logs**: the daemon's own log output, in full (not OS process
   list/CPU/mem).
4. **Usage**: the Claude Code account's 5-hour/weekly rolling usage and time
   until reset — not per-pipeline-step token/cost accounting (that already
   exists in the per-machine dashboard via `tracker.go`).
5. **Grouping**: the existing `repoSlug` project concept from `loope.json`.

## Feasibility notes (researched during design)

- **Genkit's model**: Genkit instruments flows with OpenTelemetry —
  spans/logs carry resource + structured attributes, and a local telemetry
  server ingests that data for its Dev UI. The reusable idea is the data
  model (resource attributes identifying the source, plus structured log
  records), not the transport. Adopting the full OTel Go SDK/OTLP would add
  a heavy dependency for no benefit at this scale, so this design borrows
  the shape only.
- **Claude Code usage risk**: there is no CLI subcommand or cached file that
  exposes 5h/7d rate-limit usage. The only local source found is the JSON
  Claude Code feeds to a configured `statusLine` command
  (`rate_limits.five_hour.used_percentage`, `.seven_day.used_percentage`,
  and reset timestamps). It is **unconfirmed** whether headless `claude -p`
  runs (which is how loope's pipeline steps run Claude) trigger the
  statusLine hook the same way interactive sessions do. This design does
  not resolve that; it specifies a capture mechanism plus a documented
  fallback so the feature degrades gracefully either way (see below). Root
  cause should be verified empirically as an early implementation step.

## Design

### Deployment shape

One new subcommand on the existing `loope` binary — no second binary or
release artifact:

```
loope telemetry-server --addr :9090 --data-dir ~/.loope/telemetry
```

Worker-side participation is **opt-in** via a new optional config block in
`loope.json`:

```json
"telemetry": {
  "serverURL": "http://telemetry-host:9090",
  "token": "shared-secret",
  "pushIntervalSec": 15
}
```

When `telemetry` is absent, no exporter goroutine starts and nothing about
existing daemon behavior changes. `pushIntervalSec` defaults to 15 when the
block is present but the field is zero.

### Data model

Plain JSON over HTTP, shaped like Genkit's resource + structured-log model,
without adopting OTel's wire format:

```go
type Resource struct {
    RepoSlug  string // grouping key — from cfg.RepoSlug
    MachineID string // stable id: sha256(hostname + workDir)[:12]
    Hostname  string
    WorkDir   string
    Version   string // loope build version
}

type LogRecord struct {
    Timestamp time.Time
    Body      string
}

type UsageSnapshot struct {
    FiveHourUsedPct  float64
    FiveHourResetAt  time.Time
    SevenDayUsedPct  float64
    SevenDayResetAt  time.Time
    CapturedAt       time.Time // when the statusLine hook wrote this
}

type PushRequest struct {
    Resource Resource
    Logs     []LogRecord    // new daemon-log lines since the last successful push
    Usage    *UsageSnapshot // nil when unavailable or stale (see below)
    SentAt   time.Time
}
```

`POST /v1/push` accepts a `PushRequest` and returns `204`. Auth is a single
shared bearer token: `Authorization: Bearer <token>`, checked against the
server's configured token; a mismatch is `401`. No per-worker tokens or
OAuth — matches loope's existing single-operator simplicity, extensible
later if multi-tenant use ever arises.

### Server storage

In-memory only, following the existing `Server.ghIssues` cache pattern in
`serve.go` (a mutex-guarded map, no DB):

```go
type WorkerState struct {
    Resource   Resource
    LastPushAt time.Time
    Usage      *UsageSnapshot
    LogLines   *ring.Buffer // bounded, 2000 lines
}

type TelemetryServer struct {
    mu      sync.Mutex
    workers map[string]*WorkerState // keyed by MachineID
    token   string
}
```

Rendering groups `workers` by `Resource.RepoSlug`. A worker is **online** if
`now - LastPushAt < 3 * pushIntervalSec` (using the interval the worker last
reported, defaulting to 15s if never seen). No persistence: a server restart
just goes blank until workers' next push repopulates it, matching loope's
existing "state lives in GitHub / gets rebuilt" philosophy — the telemetry
server is a live view, not a system of record.

### Daemon log capture (worker side)

Today `log.Printf` writes only to stderr; "the daemon log" only exists on
disk if an external supervisor (e.g. launchd, per `com.loope.plist.example`)
redirects it there. To give the exporter a reliable source regardless of how
the process is run, the daemon changes its logger to:

```go
log.SetOutput(io.MultiWriter(os.Stderr, rotatingFile))
```

writing to `<workDir>/logs/daemon.log` (new file; rotated at a fixed size,
e.g. 10MB, keeping one previous file — simple size-based rotation, no new
dependency). The exporter goroutine tails this file, batching new lines
(capped at 500 per push) into the next `PushRequest.Logs`.

### Claude usage capture (worker side)

loope does not silently rewrite the user's global `~/.claude/settings.json`.
Instead it ships a small helper script (`loope claude-usage-hook`, or a
standalone shell script in the repo) that a user manually chains into their
own `statusLine` command; each invocation appends the incoming
`rate_limits` JSON to `~/.claude/loope-usage.json`. The exporter reads that
file once per push cycle:

- If the file doesn't exist, or its `CapturedAt` is older than 30 minutes,
  `Usage` is sent as `nil`.
- The dashboard renders "usage: unknown" for that worker rather than a
  stale or fabricated number.

This keeps the mechanism opt-in and user-controlled, and isolates the
unconfirmed "does headless `-p` trigger statusLine" risk to a single,
clearly-labeled degraded state instead of a hard failure.

### Dashboard UI

Reuses the existing embedded-template + poll-fragment pattern from
`serve.go`/`web.go` (no new frontend framework):

- Index page: projects (`repoSlug`) as groups, each listing its workers with
  an online/offline badge, current usage %, and reset time.
- Per-worker view: tail of `LogLines`, polling on the same few-second cadence
  as the existing rail/detail fragments.

### Testing

- `PushRequest` marshal/unmarshal round-trip.
- Auth: missing/wrong token → `401`; correct token → `204`.
- Online/offline threshold transitions as `LastPushAt` ages past
  `3 * pushIntervalSec`.
- Log ring buffer caps at 2000 lines and drops oldest first.
- Usage staleness: a `CapturedAt` older than 30 minutes yields "unknown" in
  the rendered view, not stale numbers.
- Exporter: opt-in behavior — no goroutine, no HTTP calls, when `telemetry`
  config block is absent.
- Log rotation: file rotates at the size threshold without dropping the
  tail the exporter is reading.

## Out of scope

- OS-level process/CPU/mem metrics per worker (issue answer #3 scoped this
  to daemon logs only).
- Persisting telemetry server state to disk/DB.
- Per-worker auth tokens or multi-tenant access control.
- Automatically modifying the user's `~/.claude/settings.json` statusLine
  config.
