# Telemetry: full-screen worker grid, per-worker page, and persisted-log archive

Issue: #79

## Problem

The fleet telemetry screen (`telemetry-server/templates/`) still uses the
narrow rail + detail-pane shell added for #57/#66: a fixed-width `#rail`
(worker cards, 2 per row) beside a `#main` detail pane, both htmx-polled and
morph-swapped in place. The issue asks for three changes:

1. The index screen should be full-width, no rail — worker cards laid out
   directly on the page, one section per `repoSlug`, 6 cards per row, online
   workers before offline.
2. Clicking a worker should navigate to a real, separate page for that
   worker's detail — not a sheet, modal, or in-page pane swap.
3. The telemetry server should also collect and let you browse each worker's
   *persisted per-issue logs* (the `<WorkDir>/logs/issue-N/*` files the
   daemon already writes for every pipeline run — postmortem JSON, prompts,
   outputs, stream transcripts, session/state files), not just the daemon-log
   tail and Claude usage it collects today.

## Decisions (from the issue author, via clarifying rounds)

1. **What to collect**: push full file contents of every file under each
   worker's `logs/` tree, not just a listing — matching how the daemon
   already treats these files as small, already-persisted-to-disk artifacts.
2. **Transport**: extend the existing worker→server push
   (`shared.PushRequest`), not a new pull/callback path. The author asked
   this be researched against Genkit's OpenTelemetry-inspired model — see
   below.
3. **Scope**: this is the per-issue pipeline log tree
   (`<WorkDir>/logs/issue-N/*`, plus `logs/triage/*`), explicitly separate
   from `logs/daemon.log`, which the exporter already tails independently.
4. **Worker detail page**: a real page navigation with its own URL, not a
   pane swap.
5. **Grid**: fixed 6 columns, desktop-only — no responsive breakpoints.

## Genkit research note

Genkit's Dev UI is built on the OpenTelemetry SDK: each flow run becomes a
trace (a tree of spans), and the local telemetry server ingests and stores
full trace/span records, which the UI lists and lets you drill into per
trace. The reusable idea — already adopted by the original fleet-telemetry
design (#57) — is the *shape*, not the transport: a resource-scoped bundle of
structured records pushed to the server, rather than a query API the server
polls back to the client. This design extends that same shape: today's
`PushRequest` already carries `Resource` (identity) and `Logs`
(daemon-log lines); it gains `IssueLogs`, a per-issue bundle of the pipeline's
persisted files, pushed in full each cycle exactly like the rest of the
request — no new OTel dependency, no callback/pull endpoint, consistent with
decision 2.

## Design

### Wire format (`shared/wire.go`)

```go
// IssueLogFile is one persisted file from a worker's logs/<dir> tree.
type IssueLogFile struct {
    Name    string    `json:"name"`    // e.g. "003-answer-1.output.md", "state", "session"
    Content string    `json:"content"`
    ModTime time.Time `json:"modTime"`
}

// IssueLogDir is one directory under <WorkDir>/logs — one issue's pipeline
// run ("issue-42") or the shared "triage" dir.
type IssueLogDir struct {
    Name  string         `json:"name"` // dir name as on disk: "issue-42", "triage"
    Files []IssueLogFile `json:"files"`
}
```

`PushRequest` gains `IssueLogs []IssueLogDir`. `IssueLogDir.Name` sorts
naturally to reconstruct "issue-42" → issue number 42 for display; `triage`
displays as-is.

### Worker exporter (`worker/telemetry_exporter.go`)

Each push cycle, alongside the existing daemon-log tail and usage read, scan
`<cfg.WorkDir>/logs/*`: for every directory entry, read all regular files
inside (non-recursive — the existing writers in `tracker.go`/`claude.go`
never nest subdirectories) into an `IssueLogDir`. This is a full re-read and
re-send every cycle (decision 1) — these files are small (prompts/outputs/
postmortems for one Claude call, or single-line state/pr/session files), so
re-sending them at a 15s-default interval is cheap compared to the
already-larger `Logs` payload cap.

### Server storage (`telemetry-server/server.go`)

```go
type WorkerState struct {
    Resource   shared.Resource
    LastPushAt time.Time
    Usage      *shared.UsageSnapshot
    Logs       *LogRingBuffer
    IssueLogs  map[string]shared.IssueLogDir // keyed by dir name, replaced wholesale each push
}
```

`handlePush` replaces `IssueLogs` outright from the incoming request (the
exporter always sends its current full scan, same replace-not-merge
semantics as `Usage`). To keep memory bounded on a long-running repo (this is
still a live, in-memory view per the #57 design — no DB, no persistence
across restarts), the server caps retained directories per worker at the 50
most-recently-modified (by max `ModTime` across a dir's files), evicting the
rest on ingest. This mirrors the existing 2000-line cap on `Logs`, just
scoped to directories instead of lines.

### Routing and pages

Replace the rail/pane shell with two pages:

- `GET /` — full-width index. Sections per `repoSlug` (sorted), each a
  `grid-cols-6` grid of worker cards (today's card markup from #66,
  unchanged visually), online workers first then offline, both buckets
  sorted by hostname. Cards link to `/workers/{machineID}` (plain `<a
  href>`, no `hx-target`). The page keeps the existing 3s poll
  (`hx-get="/" hx-trigger="every 3s" hx-swap="morph:innerHTML"` on the body
  content, replacing today's rail-only poll) so online/offline state and
  card stats stay live without navigating away.
- `GET /workers/{machineID}` — the worker detail page: today's detail pane
  content (usage stats, daemon-log tail) plus a new persisted-logs section.
  Polls itself every 3s the same way. A worker ID with no match renders a
  simple "worker not found" state (e.g. offline workers evicted after a
  server restart) instead of erroring.

`/rail` and `/detail` (today's htmx-fragment routes) are removed along with
`page.html`'s two-pane shell; their template logic folds into the two page
handlers above.

### Persisted-logs browser (worker detail page)

Renders each `IssueLogDir` as a collapsible tree node (matching the
reference screenshot: one node per issue, files listed underneath), sorted
by `Name` with numeric issue dirs (`issue-42`) before `triage`, newest-first
by max file `ModTime`. Selecting a file is a query-param navigation on the
same page — `/workers/{id}?dir=issue-42&file=003-answer-1.output.md` — which
renders that file's content in a pane beside the tree (plain `<pre>`,
monospace, no syntax highlighting — matches the existing minimal-JS
approach; `.json` files are pretty-printed server-side before display since
`Claude.saveLog` writes raw API responses).

### Files touched

- `shared/wire.go` — `IssueLogFile`, `IssueLogDir`, `PushRequest.IssueLogs`.
- `worker/telemetry_exporter.go` — scan and attach `logs/*` contents each push.
- `telemetry-server/server.go` — `WorkerState.IssueLogs`, ingest-time replace
  + eviction cap.
- `telemetry-server/web.go` — new view-model fields/handlers for `/` and
  `/workers/{machineID}`; remove `/rail`/`/detail`.
- `telemetry-server/templates/page.html` — full-width single-page shell.
- `telemetry-server/templates/rail.html` → folds into the index page's
  section/grid markup (6 columns, online-then-offline ordering).
- `telemetry-server/templates/detail.html` — gains the log-tree/file-viewer
  section; becomes the `/workers/{machineID}` page body instead of a
  poll-fragment partial.

### Testing

- `IssueLogDir`/`IssueLogFile` marshal round-trip in `shared`.
- Exporter: a `logs/issue-42` dir with a few files produces one `IssueLogDir`
  with matching `Files`; an empty `logs/` produces an empty slice, not nil
  vs. omitted-field ambiguity issues.
- Server: ingest replaces `IssueLogs` wholesale (stale dirs from a prior push
  that no longer exist on disk are dropped, not merged); eviction keeps only
  the 50 most-recently-modified dirs when a push exceeds the cap.
- Web: index page renders 6-column grids, online workers sorted before
  offline within a `repoSlug` section; `/workers/{id}` renders the log tree
  and serves a selected file's content; an unknown `{id}` renders "not
  found" rather than a panic/500.
- Existing substring-based assertions in `web_test.go` (hostnames, repo
  slugs, usage text, "unknown") get re-pointed at the new page bodies since
  `/rail`/`/detail` no longer exist.

## Out of scope

- Syntax highlighting or rendering `.md`/`.jsonl` files as anything other
  than plain preformatted text.
- Persisting `IssueLogs` (or any telemetry state) to disk on the server —
  still a live, in-memory view per the #57 design.
- Deleting or rotating the on-disk `logs/issue-N` files themselves (worker
  side) — this design only reads and forwards them.
- Changing the 6-column grid to be responsive — desktop-only, per decision 5.
