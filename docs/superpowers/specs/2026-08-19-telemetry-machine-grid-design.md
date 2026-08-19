# Telemetry admin screen: card grid instead of rail list

Issue: #66

## Problem

The fleet telemetry screen (`telemetry-server/templates/`) currently shows
workers as a narrow, single-column "rail" list on the left: one thin row per
machine (a status dot, hostname, and 5h usage), with a wider detail pane on
the right showing the selected worker's full usage stats and log tail. The
issue asks to replace the thin rail rows with a block/card grid per
machine — similar to a "manage machines" screen (see attached reference
mockup) — while keeping the same underlying data and page structure.

## Decisions (from the issue author, via clarifying rounds)

1. **Detail view**: keep today's separate detail pane, shown beside/below the
   grid (not a modal, not folded into the card itself). Clicking a card loads
   that worker's detail into the existing `#main` pane, same as today.
2. **Card banner**: a solid/neutral color block with a generic machine icon
   (not per-repo color, not a photo).
3. **Card content**: the worker's MachineID as the title, plus both the
   5-hour usage and 7-day (weekly) usage figures on the card.
4. **Status dot**: two states only — online (green) / offline (gray) — same
   semantics as today, no third "usage unknown" color.
5. **Grouping**: keep the existing repo-slug section headers above each
   group's grid (unchanged from today).

## Design

### Layout

Keep the existing two-pane shell in `templates/page.html`: `#rail` on the
left (htmx-polled every 3s, morph-swapped) and `#main` detail pane on the
right (unchanged, still polls `/detail`). Only the *contents* of `#rail`
change — from a single-column list to a 2-column grid of cards. Widen
`#rail`'s fixed width (currently `w-[340px]`) enough to comfortably fit two
cards per row (e.g. `w-[420px]`); `#main` remains `flex-1`.

Repo-slug group headers stay as-is, sitting above each group's card grid
(`grid grid-cols-2 gap-2` or similar, replacing the current `flex flex-col`
list container).

### Card

Each worker renders as one card (replacing the current single-line row),
roughly:

```
┌─────────────────────────┐
│   [server icon, centered]│  ← banner: solid neutral bg (bg-panel2), generic icon
├─────────────────────────┤
│ ● m1a2b3c4               │  ← status dot + MachineID, bold, truncated
│ 5h: 42%   7d: 18%        │  ← or "usage: unknown" when UsageUnknown
└─────────────────────────┘
```

- Banner: fixed-height div, `bg-panel2` (or existing neutral token), centered
  inline SVG server/machine icon (no external asset, no per-repo coloring).
- Status dot: reuse existing `bg-ok` (online, green) / `bg-muted` (offline,
  gray) classes from `rail.html`/`detail.html` — no new color needed.
- Title: `.MachineID` (not `.Hostname` — the issue author specifically wants
  the worker/machine ID as the card title), truncated, bold, monospace to
  match the rest of the UI.
- Stats line: `5h: {{.FiveHourPct}}%` and `7d: {{.SevenDayPct}}%` when usage
  is known; when `.UsageUnknown` is true, show `usage: unknown` (matching the
  existing wording so the current test assertion in `web_test.go` —
  `TestTelemetryDetailShowsUsageUnknownWhenAbsent` — style stays consistent,
  though that specific test only asserts on the detail pane).
- Selected state: the currently-selected card (`.MachineID` ==
  `$.Selected.MachineID`) gets a visible highlight (e.g. `border-live`/ring),
  since the grid no longer relies on list position to show selection. This
  requires capturing the outer `.Selected` via `$` before the `{{range
  .Groups}}` / `{{range .Workers}}` nesting in the template, since `range`
  rebinds `.`.
- Click behavior unchanged: `<a href="?worker=...">` for no-JS fallback, plus
  `hx-get="/detail?worker=..." hx-target="#main" hx-swap="morph:innerHTML"`.

### Files touched

- `telemetry-server/templates/rail.html` — rewritten: card grid markup
  instead of the single-column list of rows.
- `telemetry-server/templates/page.html` — widen the `#rail` container's
  fixed-width class to fit two card columns.

No changes needed to `telemetry-server/web.go` (view model already exposes
`MachineID`, `Hostname`, `Online`, `FiveHourPct`, `SevenDayPct`,
`UsageUnknown`), `telemetry-server/templates/detail.html`, or any Go
handler/routing code — this is a template/markup-only change.

### Testing

Existing tests in `telemetry-server/web_test.go` assert on substrings in the
rendered HTML (hostnames, repo slugs, usage percentages, log lines, the
literal word "unknown") rather than specific markup/classes, so they should
continue to pass unmodified. No new Go tests are required for this
markup-only change; verify by rendering the page (`go test ./telemetry-server/...`
plus a manual look at `/` with a couple of pushed workers) to confirm the
grid renders, the selected card highlights, and click-to-detail still works.
