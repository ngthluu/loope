# Telemetry admin screen: card grid Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the telemetry admin screen's single-column "rail" list of
workers with a 2-column card grid (server icon banner, status dot, MachineID,
5h/7d usage), keeping the existing detail pane, group headers, and polling
behavior unchanged.

**Architecture:** This is a template/markup change to
`telemetry-server/templates/rail.html` (card grid instead of list rows) and
`telemetry-server/templates/page.html` (wider `#rail` to fit two columns), plus
a one-time fix to stand up a Tailwind CSS build pipeline for `telemetry-server`
— see "Assumption" below — so the new utility classes actually render. No Go
handler, routing, or view-model code changes.

**Tech Stack:** Go `html/template`, htmx + idiomorph (existing, unchanged),
Tailwind CSS v4 (standalone CLI, no npm/Node).

**Spec:** `docs/superpowers/specs/2026-08-19-telemetry-machine-grid-design.md`

## Assumption (read before starting)

`telemetry-server/static/app.css` is **not** built from a Tailwind source file
that lives in `telemetry-server/`. Per the comment at the top of
`telemetry-server/static.go`, it is "a copy of the worker dashboard's bundle
(`worker/web/static`)" taken at the monorepo split and never regenerated
since. That copy only contains utility classes `worker/web/templates/*.html`
happened to use at split time — not the classes `telemetry-server`'s own
templates use. This is independently verifiable and already broken today,
before this plan touches anything:

- `telemetry-server/templates/page.html` uses `w-[340px]` on `#rail`, but
  `grep -c '340px' telemetry-server/static/app.css` is `0` — the rail is
  **not actually 340px wide in a browser today**, it falls back to intrinsic
  width.
- `telemetry-server/templates/rail.html` and `detail.html` both use
  `bg-muted` for the offline status dot, but `grep -c 'bg-muted'
  telemetry-server/static/app.css` is `0` — the offline dot **has no
  background color today**, it's invisible.

Since the card grid design needs several classes that are equally absent
(`w-[420px]`, `border-live` un-suffixed, `ring-1`, `h-*`/`w-*` for the icon,
etc.), patching around this by hand-picking only classes that happen to
already be compiled would still leave the pre-existing `w-[340px]` /
`bg-muted` gaps unfixed and would silently drop any new class this plan
needs. Task 1 below stands up the same Tailwind pipeline `worker/` already
has (source file + standalone CLI regen + a regression test), scoped to
`telemetry-server`'s own templates. This is a **build-tooling fix, not a
scope change to the feature** — the spec's "no Go changes" holds; only a new
non-Go source file (`telemetry-server/tailwind.css`) and the regenerated,
committed `telemetry-server/static/app.css` are added.

## Global Constraints

- Detail pane stays a separate pane beside/below the grid (not a modal, not
  folded into the card) — clicking a card still loads `/detail?worker=...`
  into `#main` via htmx, same as today.
- Card banner is a solid/neutral color block with a generic machine icon —
  no per-repo color, no photo.
- Card shows the worker's `MachineID` as the title (not `Hostname`), plus
  both `FiveHourPct` and `SevenDayPct`.
- Status dot has exactly two states: online (`bg-ok`, green) / offline
  (`bg-muted`, gray) — no third "usage unknown" color.
- Repo-slug group headers stay exactly as they render today, above each
  group's grid.
- No changes to `telemetry-server/web.go`, `telemetry-server/templates/detail.html`,
  or any Go handler/routing code.
- Existing substring-assertion tests in `telemetry-server/web_test.go` must
  keep passing unmodified.

---

## File Structure

- `telemetry-server/tailwind.css` — **new.** Tailwind v4 source (theme
  tokens, keyframes, hand-written CSS, `@source` glob) for `telemetry-server`,
  mirroring `worker/web/tailwind.css`.
- `telemetry-server/static/app.css` — **regenerated, committed.** Compiled
  output of `tailwind.css`, scoped to `telemetry-server/templates/*.html`.
- `telemetry-server/web_test.go` — **modified.** New
  `TestAppCSSCoversBothClassSources`-style guard test (append).
- `telemetry-server/templates/rail.html` — **rewritten.** Card grid markup
  instead of the single-column list of rows.
- `telemetry-server/templates/page.html` — **modified.** `#rail`'s fixed
  width class widened from `w-[340px]` to `w-[420px]`.

---

## Task 1: Vendor a Tailwind build pipeline for `telemetry-server`

**Files:**
- Create: `telemetry-server/tailwind.css`
- Modify: `telemetry-server/static/app.css` (regenerated)
- Test: `telemetry-server/web_test.go` (append)

**Interfaces:**
- Consumes: `telemetry-server/templates/*.html` as the sole Tailwind class
  source (no Go-side class-building helpers exist in this package — confirmed
  by grepping `web.go`/`server.go` for class-name fragments, none found).
- Produces: `telemetry-server/static/app.css`, served at `/static/app.css` by
  the existing `staticHandler()` in `static.go` (unchanged) via the existing
  `//go:embed static` directive on `staticFS` in `static.go` (unchanged).

- [ ] **Step 1: Install the Tailwind v4 standalone CLI**

A single binary — no npm, no `package.json`, no Node. On Apple Silicon:

```bash
curl -fsSL -o /tmp/tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/latest/download/tailwindcss-macos-arm64
chmod +x /tmp/tailwindcss
/tmp/tailwindcss --help | head -5
```

Expected: prints the CLI usage banner. (Use `tailwindcss-macos-x64` or
`tailwindcss-linux-x64` as appropriate for the machine running this step.) Do
**not** commit the binary.

- [ ] **Step 2: Write `telemetry-server/tailwind.css`**

Same theme tokens and hand-written CSS as `worker/web/tailwind.css` (the two
dashboards must look identical), scoped to this package's own templates:

```css
/* Tailwind source for the telemetry admin dashboard.
 *
 * static/app.css is generated from this file and committed. Regenerate it
 * after changing any Tailwind class in a template:
 *
 *   tailwindcss -i tailwind.css -o static/app.css --minify
 */
@import "tailwindcss";

@source "./templates/*.html";

@theme {
  --color-ink: #F3F5F8;
  --color-panel: #FFFFFF;
  --color-panel2: #EAEEF3;
  --color-line: #E4E8ED;
  --color-line2: #D2D9E1;
  --color-text: #16202B;
  --color-muted: #55636F;
  --color-faint: #6E7A87;
  --color-ok: #0B7D43;
  --color-err: #C42B1C;
  --color-warn: #B45309;
  --color-live: #0A7E95;

  --font-sans: "IBM Plex Sans", system-ui, sans-serif;
  --font-mono: "IBM Plex Mono", ui-monospace, monospace;
}

:root{color-scheme:light} body{background:#F3F5F8}
@keyframes hb{0%,100%{opacity:.35;transform:scale(.8)}50%{opacity:1;transform:scale(1)}}
@keyframes ring{0%{box-shadow:0 0 0 0 rgba(10,126,149,.45)}70%{box-shadow:0 0 0 8px rgba(10,126,149,0)}100%{box-shadow:0 0 0 0 rgba(10,126,149,0)}}
@keyframes fadein{from{opacity:0;transform:translateY(3px)}to{opacity:1;transform:none}}
.hb{animation:hb 1.6s ease-in-out infinite}.ring{animation:ring 1.8s ease-out infinite}.fadein{animation:fadein .35s ease both}
.node-ok{box-shadow:0 0 0 3px rgba(11,125,67,.16)}.node-err{box-shadow:0 0 0 3px rgba(196,43,28,.16)}.node-live{box-shadow:0 0 0 3px rgba(10,126,149,.2)}
details>summary{list-style:none}details>summary::-webkit-details-marker{display:none}details[open] .chev{transform:rotate(90deg)}
.scroll::-webkit-scrollbar{width:10px;height:10px}.scroll::-webkit-scrollbar-thumb{background:#D2D9E1;border-radius:6px;border:2px solid #F3F5F8}.scroll::-webkit-scrollbar-track{background:transparent}
@media (prefers-reduced-motion:reduce){.hb,.ring,.fadein{animation:none!important}}
```

- [ ] **Step 3: Generate `static/app.css` from the current (pre-redesign) templates**

Run from `telemetry-server/`:

```bash
cd telemetry-server
/tmp/tailwindcss -i tailwind.css -o static/app.css --minify
wc -c static/app.css
```

Expected: the CLI reports "Done in Nms" and the file is at least several KB
(not a few hundred bytes — a tiny file means the `@source` glob matched
nothing; confirm the command ran from `telemetry-server/`, not the repo
root).

- [ ] **Step 4: Confirm the pre-existing gap is fixed**

```bash
grep -c '340px' telemetry-server/static/app.css
grep -c 'bg-muted' telemetry-server/static/app.css
```

Expected: both now print `1` or more (they printed `0` before this step —
this is the pre-existing staleness bug described in "Assumption" above,
fixed as a side effect of standing up the pipeline).

- [ ] **Step 5: Write the failing stale-CSS guard test**

Append to `telemetry-server/web_test.go`:

```go
// TestAppCSSCoversTemplateClasses is the guard against the manual Tailwind
// regeneration step being skipped. telemetry-server/static/app.css must be
// regenerated from tailwind.css whenever a template's classes change, or the
// dashboard renders half-styled (or, for a brand-new class, unstyled). A miss
// here means someone changed a template class without re-running:
//
//	tailwindcss -i tailwind.css -o static/app.css --minify
func TestAppCSSCoversTemplateClasses(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if len(css) < 2048 {
		t.Fatalf("static/app.css is only %d bytes — the Tailwind build produced nothing useful", len(css))
	}
	for _, want := range []string{
		`bg-muted`,      // status dot (offline) — templates/rail.html, templates/detail.html
		`w-\[420px\]`,   // #rail width — templates/page.html
		`grid-cols-2`,   // card grid — templates/rail.html
	} {
		if !strings.Contains(string(css), want) {
			t.Fatalf("static/app.css missing %q — regenerate it: tailwindcss -i tailwind.css -o static/app.css --minify", want)
		}
	}
}
```

`web_test.go` already imports `strings`, `net/http`, `net/http/httptest`, and
`testing`; no new imports are needed. `staticFS` is the package-level
`embed.FS` declared in `static.go` — visible from `web_test.go` since both
are in package `main`.

- [ ] **Step 6: Run the test to verify it fails**

```bash
cd telemetry-server
go test ./... -run TestAppCSSCoversTemplateClasses -v
```

Expected: FAIL — `w-\[420px\]` and `grid-cols-2` are not yet in
`static/app.css` (Task 2 hasn't rewritten the templates yet) and are not
present even after Step 3's regen, since the *current* `rail.html`/`page.html`
don't use them yet either.

This confirms the test is load-bearing. Leave it failing — Task 3 makes it
pass after Task 2 rewrites the templates and this task's CSS is regenerated
one more time.

- [ ] **Step 7: Commit**

```bash
git add telemetry-server/tailwind.css telemetry-server/static/app.css telemetry-server/web_test.go
git commit -m "build: vendor a Tailwind pipeline for telemetry-server, fixing stale app.css"
```

---

## Task 2: Rewrite the rail into a card grid

**Files:**
- Modify: `telemetry-server/templates/rail.html` (full rewrite)
- Modify: `telemetry-server/templates/page.html:15` (widen `#rail`)
- Test: `telemetry-server/web_test.go` (existing tests, run unmodified)

**Interfaces:**
- Consumes: `telemetryView{Groups []telemetryGroup, Selected
  *telemetryWorkerView}` and `telemetryWorkerView{MachineID, Hostname,
  Online, UsageUnknown, FiveHourPct, SevenDayPct, ...}` from
  `telemetry-server/web.go` — **unchanged**, already expose every field the
  card needs.
- Produces: the `"trail"` template block, rendered into `#rail` by both the
  full-page render (`page.html`) and the `/rail` htmx poll endpoint — same
  contract as today.

- [ ] **Step 1: Rewrite `telemetry-server/templates/rail.html`**

Replace the entire file:

```html
{{define "trail"}}<div class="p-3">
{{$selID := ""}}{{with .Selected}}{{$selID = .MachineID}}{{end}}
{{range .Groups}}
 <div class="mb-4">
  <div class="mb-1.5 px-1 font-mono text-[10px] font-semibold uppercase tracking-[0.15em] text-faint">{{.RepoSlug}}</div>
  <div class="grid grid-cols-2 gap-2">
  {{range .Workers}}
   <a href="?worker={{.MachineID}}" hx-get="/detail?worker={{.MachineID}}" hx-target="#main" hx-swap="morph:innerHTML"
      class="block overflow-hidden rounded-md border bg-panel font-mono text-text hover:border-live/40 {{if eq $selID .MachineID}}border-live ring-1 ring-live/40{{else}}border-line2{{end}}">
    <div class="flex h-12 items-center justify-center border-b border-line2 bg-panel2">
     <svg class="h-5 w-5 text-faint" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">
      <rect x="3" y="4" width="18" height="6" rx="1.5"></rect>
      <rect x="3" y="14" width="18" height="6" rx="1.5"></rect>
      <circle cx="7" cy="7" r="0.75" fill="currentColor" stroke="none"></circle>
      <circle cx="7" cy="17" r="0.75" fill="currentColor" stroke="none"></circle>
     </svg>
    </div>
    <div class="px-2.5 py-2">
     <div class="mb-1 flex items-center gap-1.5">
      <span class="inline-block h-2 w-2 shrink-0 rounded-full {{if .Online}}bg-ok{{else}}bg-muted{{end}}"></span>
      <span class="truncate text-[11px] font-semibold">{{.MachineID}}</span>
     </div>
     {{if .UsageUnknown}}<div class="text-[11px] text-faint">usage: unknown</div>
     {{else}}<div class="tabular-nums text-[11px] text-faint">5h: {{printf "%.0f%%" .FiveHourPct}}&nbsp;&nbsp;7d: {{printf "%.0f%%" .SevenDayPct}}</div>{{end}}
    </div>
   </a>
  {{end}}
  </div>
 </div>
{{else}}
 <div class="p-4 font-mono text-[12px] text-faint">No workers have reported yet.</div>
{{end}}
</div>{{end}}
```

Notes on this markup, matching the spec's decisions:

- `$selID` is captured via `{{with .Selected}}` **before** the `{{range
  .Groups}}` / `{{range .Workers}}` nesting, because `range` rebinds `.` —
  the spec calls this out explicitly. Using a plain string (`""` when no
  worker is selected, e.g. an empty fleet) instead of holding onto the
  `*telemetryWorkerView` pointer avoids any nil-pointer risk in the
  comparison — `.MachineID` is never a real empty string, so `eq $selID
  .MachineID` is false whenever nothing is selected.
- Card title is `.MachineID` (not `.Hostname`) per the spec's explicit
  correction — the old `rail.html` showed `.Hostname`.
- Status dot reuses `bg-ok` / `bg-muted`, the same two classes
  `detail.html` already uses for the identical two-state (online/offline)
  semantics — no new color.
- Click behavior is unchanged: `href="?worker=..."` no-JS fallback, plus
  `hx-get="/detail?worker=..." hx-target="#main" hx-swap="morph:innerHTML"`,
  copied verbatim from the old row markup.
- Banner icon is inline SVG (no external asset), `text-faint` (neutral,
  matches other muted iconography in this UI), on a `bg-panel2` strip — the
  spec's "solid/neutral color block with a generic machine icon".

- [ ] **Step 2: Widen `#rail` in `telemetry-server/templates/page.html`**

In `telemetry-server/templates/page.html:15`, change:

```html
 <nav id="rail" class="scroll w-[340px] shrink-0 overflow-y-auto border-r border-line bg-panel"
```

to:

```html
 <nav id="rail" class="scroll w-[420px] shrink-0 overflow-y-auto border-r border-line bg-panel"
```

(`#main` stays `flex-1`, unchanged.)

- [ ] **Step 3: Run the existing test suite**

```bash
cd telemetry-server
go test ./... -run 'TestTelemetry' -v
```

Expected: `TestTelemetryIndexGroupsByRepoSlugAndShowsWorkers`,
`TestTelemetryDetailShowsUsageUnknownWhenAbsent`,
`TestTelemetryDetailShowsFreshUsagePercentage`, and
`TestTelemetryDetailShowsLogTail` all PASS unmodified — they assert on
substrings (hostnames, repo slugs, "unknown", usage percentages, log lines)
that are still present in the new markup. `TestAppCSSCoversTemplateClasses`
from Task 1 is still expected to FAIL at this point (CSS not regenerated
yet) — that's Task 3.

- [ ] **Step 4: Commit**

```bash
git add telemetry-server/templates/rail.html telemetry-server/templates/page.html
git commit -m "feat(telemetry): render workers as a card grid instead of a rail list"
```

---

## Task 3: Regenerate CSS for the new markup and verify end-to-end

**Files:**
- Modify: `telemetry-server/static/app.css` (regenerated)
- Test: `telemetry-server/web_test.go` (`TestAppCSSCoversTemplateClasses` from Task 1, now expected to pass)

**Interfaces:**
- Consumes: `telemetry-server/tailwind.css` (Task 1) and the rewritten
  `telemetry-server/templates/*.html` (Task 2).
- Produces: the final, committed `telemetry-server/static/app.css` containing
  every utility class the new card grid uses.

- [ ] **Step 1: Regenerate `static/app.css`**

```bash
cd telemetry-server
/tmp/tailwindcss -i tailwind.css -o static/app.css --minify
```

(Re-download the CLI per Task 1 Step 1 first if `/tmp/tailwindcss` is gone —
it's not committed.)

- [ ] **Step 2: Run the full `telemetry-server` test suite**

```bash
cd telemetry-server
go test ./... -v
```

Expected: every test passes, including
`TestAppCSSCoversTemplateClasses` (now finds `bg-muted`, `w-\[420px\]`, and
`grid-cols-2` in the regenerated CSS) and all four `TestTelemetry*` tests
from Task 2.

- [ ] **Step 3: Manual render check**

```bash
cd telemetry-server
go run . -addr localhost:8099 -token dev-token &
SERVER_PID=$!
sleep 1
curl -s -X POST localhost:8099/push -H 'Authorization: Bearer dev-token' -H 'Content-Type: application/json' \
  -d '{"resource":{"machineId":"m-aaaa1111","repoSlug":"acme/widgets","hostname":"host-a","pushIntervalSec":15},"usage":{"fiveHourUsedPct":42.3,"sevenDayUsedPct":18.1,"capturedAt":"2026-08-19T00:00:00Z"}}'
curl -s -X POST localhost:8099/push -H 'Authorization: Bearer dev-token' -H 'Content-Type: application/json' \
  -d '{"resource":{"machineId":"m-bbbb2222","repoSlug":"acme/widgets","hostname":"host-b","pushIntervalSec":15}}'
open http://localhost:8099/
kill $SERVER_PID
```

Expected in the browser: two cards side by side under the `acme/widgets`
group header, `#rail` visibly wider than the old rail (fits two columns
comfortably), each card showing a server-icon banner, a status dot (green for
`m-aaaa1111` which has fresh usage, gray for `m-bbbb2222` which has none),
the MachineID as the bold title, and either `5h: 42% 7d: 18%` or `usage:
unknown` depending on the worker. Clicking a card loads its detail into the
right-hand pane and gives the clicked card a visible highlight (colored
border/ring) that the other card doesn't have.

If `go run .` requires additional flags in this checkout, check `main.go`'s
flag parsing (`parseTelemetryServerFlags` in `main.go`, tested in
`main_test.go`) for the exact flag names before running this step.

- [ ] **Step 4: Commit**

```bash
git add telemetry-server/static/app.css
git commit -m "build: regenerate telemetry-server app.css for the card grid"
```

---

## Self-Review Notes

- **Spec coverage:** detail pane placement (unchanged, Task 2 doesn't touch
  `detail.html` or `#main`) — ✅ satisfied by leaving `page.html`'s `#main`
  block untouched; card banner (solid/neutral + generic icon) — ✅ Task 2
  Step 1; card content (MachineID title, 5h + 7d) — ✅ Task 2 Step 1; status
  dot two-state — ✅ Task 2 Step 1 reuses `bg-ok`/`bg-muted`; group headers
  unchanged — ✅ Task 2 Step 1 keeps the `RepoSlug` header div verbatim;
  `#rail` widened — ✅ Task 2 Step 2; `$` capture before `range` — ✅ Task 2
  Step 1's `$selID` pattern; existing tests keep passing — ✅ verified in
  Task 2 Step 3 and Task 3 Step 2.
- **Placeholder scan:** no TBD/TODO markers; every step has literal file
  contents or literal shell commands.
- **Type consistency:** `telemetryView`, `telemetryGroup`,
  `telemetryWorkerView` field names used in Task 2's template
  (`.MachineID`, `.Online`, `.UsageUnknown`, `.FiveHourPct`,
  `.SevenDayPct`) match `telemetry-server/web.go`'s struct definitions
  exactly (verified by reading the file before writing this plan) — no
  renamed/invented fields.
