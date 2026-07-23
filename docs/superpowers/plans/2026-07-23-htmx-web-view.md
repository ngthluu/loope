# HTMX Web View Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the `-serve` dashboard's hand-rolled poll/morph client and inline Go-string templates with vendored HTMX + idiomorph, real files under `web/` embedded via `go:embed`, and a prebuilt Tailwind `app.css` — with pixel-identical rendered output.

**Architecture:** Templates and static assets move out of Go string constants into `web/templates/*.html` and `web/static/`, embedded by a new `web.go` and parsed once with `ParseFS`. The client keeps only ~40 lines of genuine policy in `web/static/app.js`; polling becomes `hx-get` + `hx-trigger="every 3s"` + `hx-swap="morph:innerHTML"` on the rail and main panes, and header stats arrive as an `hx-swap-oob` fragment. Tailwind's browser CDN is replaced by a committed, manually regenerated `web/static/app.css`.

**Tech Stack:** Go 1.25 (`html/template`, `embed`, `net/http`), htmx 2.x, idiomorph htmx extension, Tailwind CSS v4 standalone CLI (build-time only, no Node in CI or `go build`).

## Global Constraints

- **Pixel-identical output.** The rendered dashboard must look exactly like today's. No visual redesign, no dark mode, no markup restyling beyond what this plan specifies.
- **`go build` stays the only build command.** No npm, no `package.json`, no Node anywhere in CI, `.goreleaser.yaml`, or the test suite. The Tailwind CLI is a manual, local, developer-run step.
- **`go test ./...` is the whole test story.** All tests are pure Go. The `node` dependency disappears with `ui_client_test.go`.
- **Single binary.** Every asset the dashboard serves is embedded with `go:embed`; nothing is read from disk at runtime. No `-web-dir` flag, no dev-mode filesystem path.
- **Existing assertions in `serve_test.go` are the safety net.** They must pass unmodified. Any diff to an existing test in that file is a signal that output drifted — fix the output, not the test. (Adding *new* tests to the file is expected.)
- **Out of scope:** SSE/websockets, dark mode, HTMX-driven ticket selection (plain `<a href="/?issue=N">` links stay), vendoring web fonts. Google Fonts remains a CDN `<link>`.
- **Vendored JS is pinned to an exact version** and committed verbatim — never hand-edited.
- Every file under `web/` is committed to git. Do not add anything under `web/` to `.gitignore`.

## Assumptions

Made while writing this plan; noted here rather than asked, per headless mode.

1. **The Tailwind CDN `<script>` and inline `tailwind.config` stay in `page.html` until Task 5**, where they are replaced by the `app.css` link. This keeps the dashboard fully styled after *every* task, instead of leaving it unstyled between the template move and the CSS build.
2. **Version pins** below are htmx `2.0.4` and idiomorph `0.7.3`. If a download 404s, use the newest `2.x` / `0.7.x` respectively and record the version actually used in the comment at the top of `web/static/app.js`.
3. **The rail/main `hx-get` renders as `/rail?issue=` (empty value) when no ticket is selected.** `Server.load` already treats an empty `issue` query the same as an absent one, so this is behaviour-neutral and avoids a conditional inside a URL attribute.
4. **The header stat block gains a wrapper `<div id="statbar">`** carrying today's exact classes, so the OOB swap can replace it by `outerHTML` without changing layout. The "reading N tracked issues" text to its left is *not* inside the statbar and is not poll-updated — same as today, where `applyMeta` only patched `stat-tickets` / `stat-running` / `stat-spend`.
5. **The stale-CSS sentinels** are `line-clamp-2` (appears only in `web/templates/rail.html`) and `bg-ok/50` (produced only by `stripeClass` in Go). Task 5 has an explicit step to confirm how Tailwind escapes `bg-ok/50` in the generated file and to write the assertion against the real output.

---

## File Structure

**New:**

| File | Responsibility |
|---|---|
| `web.go` | Owns the embedded FS and nothing else: the `//go:embed` directive, `fs.Sub` accessors, and the `/static/` file handler. |
| `render.go` | The template `FuncMap` and every formatter/class helper it binds. Pure functions, no HTTP. |
| `web/templates/page.html` | `{{define "page"}}` — document shell, header, statbar, rail/main containers, script tags. Also `{{define "statbar"}}` is *not* here (see rail.html). |
| `web/templates/rail.html` | `{{define "rail"}}`, `{{define "railpoll"}}`, `{{define "statbar"}}`, `{{define "statbar-oob"}}`. |
| `web/templates/detail.html` | `{{define "detail"}}` |
| `web/templates/stepcard.html` | `{{define "stepcard"}}` |
| `web/static/htmx.min.js` | Vendored, pinned, never hand-edited. |
| `web/static/idiomorph-ext.min.js` | Vendored htmx morph extension (bundles idiomorph). |
| `web/static/app.js` | Residual client policy: ago-ticker, `copySid`, transcript pinning, disclosure rule. |
| `web/static/app.css` | Tailwind CLI output. Committed. Regenerated manually. |
| `web/tailwind.css` | Tailwind v4 source: `@import`, `@source` globs, `@theme` palette/fonts, hand-written CSS. |

**Modified:** `serve.go` (shrinks to HTTP concerns), `serve_test.go` (new tests appended), `README.md`.

**Deleted:** `ui.go`, `ui_client_test.go`.

---

## Task 1: Embedded static assets and the `/static/` route

Vendors htmx and the idiomorph extension, creates `web.go`, and serves `web/static/` from the embedded FS. Nothing wires them into the page yet — the dashboard is unchanged after this task, but the assets are fetchable.

**Files:**
- Create: `web.go`
- Create: `web/static/htmx.min.js` (downloaded)
- Create: `web/static/idiomorph-ext.min.js` (downloaded)
- Create: `web/static/app.js`
- Modify: `serve.go` (add one route to `Handler`)
- Test: `serve_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `var webFS embed.FS` — the embedded `web/templates` + `web/static` trees, rooted so paths start with `web/`.
  - `func staticHandler() http.Handler` — a ready-to-mount handler for `GET /static/`; it strips the `/static/` prefix itself, so register it directly with `mux.Handle("GET /static/", staticHandler())`.
  - `web/static/app.js` defines the global `copySid(btn)` and installs `htmx:afterSwap` / `htmx:afterRequest` listeners.

- [ ] **Step 1: Create the static directory and vendor htmx**

```bash
mkdir -p web/static web/templates
curl -fsSL https://cdn.jsdelivr.net/npm/htmx.org@2.0.4/dist/htmx.min.js -o web/static/htmx.min.js
curl -fsSL https://cdn.jsdelivr.net/npm/idiomorph@0.7.3/dist/idiomorph-ext.min.js -o web/static/idiomorph-ext.min.js
wc -c web/static/*.js
```

Expected: both files non-empty (htmx ~50 KB, the extension ~10 KB). If either 404s, retry with the newest `2.x` / `0.7.x` version and note the version used in Step 3's comment header.

- [ ] **Step 2: Sanity-check what was downloaded**

```bash
grep -c "htmx" web/static/htmx.min.js
grep -c "Idiomorph" web/static/idiomorph-ext.min.js
```

Expected: both print a number greater than 0. If a file contains HTML (a CDN error page), delete it and re-download.

- [ ] **Step 3: Write `web/static/app.js`**

This is the final content; the listeners are inert until Task 3 wires htmx into the page.

```javascript
// app.js — the dashboard's residual client policy.
//
// Everything the old hand-rolled reconciler did about node identity (disclosure
// state, scroll offsets, focus, no whole-screen fadein replay) is now idiomorph's
// job, driven by hx-swap="morph:innerHTML". What is left here is policy morphing
// cannot express.
//
// Vendored versions: htmx 2.0.4, idiomorph-ext 0.7.3.

// copySid copies a full session id to the clipboard, with a textarea fallback for
// non-secure contexts. Called from an inline onclick, so it must stay global.
function copySid(btn) {
  if (btn.dataset.copying) return;
  var id = btn.getAttribute('data-sid') || '';
  var orig = btn.textContent;
  var done = function () {
    btn.dataset.copying = '1';
    btn.textContent = 'copied';
    setTimeout(function () { delete btn.dataset.copying; btn.textContent = orig; }, 1200);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(id).then(done, done);
  } else {
    var ta = document.createElement('textarea');
    ta.value = id;
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
    done();
  }
}

// The "live · Ns ago" ticker. #ago is re-created by every out-of-band statbar
// swap, so the element is looked up on each tick rather than cached.
var since = 0;
setInterval(function () {
  since++;
  var el = document.getElementById('ago');
  if (el) el.textContent = since + 's';
}, 1000);

document.body.addEventListener('htmx:afterRequest', function () {
  since = 0;
  var el = document.getElementById('ago');
  if (el) el.textContent = '0s';
});

// A transcript feed the user had pinned to the bottom keeps following new lines.
// One they scrolled up in keeps its offset for free — idiomorph never re-creates
// the node — so this only re-pins the feeds that were already at the bottom.
var pinned = {};
document.body.addEventListener('htmx:beforeSwap', function (e) {
  e.target.querySelectorAll('.txfeed').forEach(function (f) {
    pinned[f.getAttribute('data-seq')] = f.scrollHeight - f.scrollTop - f.clientHeight < 8;
  });
});
document.body.addEventListener('htmx:afterSwap', function (e) {
  e.target.querySelectorAll('.txfeed').forEach(function (f) {
    var k = f.getAttribute('data-seq');
    if (pinned[k] === undefined || pinned[k]) f.scrollTop = f.scrollHeight;
  });
});

// The disclosure rule: a running step's fragment always ships <details open>, so
// re-applying the server's `open` attribute would re-open a disclosure the user
// just closed three seconds ago. Newly appearing step cards still auto-open,
// because idiomorph inserts new nodes wholesale rather than morphing them.
if (window.Idiomorph) {
  window.Idiomorph.defaults.callbacks.beforeAttributeUpdated = function (name, node) {
    if (name === 'open' && node.hasAttribute && node.hasAttribute('data-disc')) return false;
  };
}
```

- [ ] **Step 4: Write the failing test**

Append to `serve_test.go`:

```go
func TestStaticAssetsServed(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, tc := range []struct{ path, ctPart string }{
		{"/static/htmx.min.js", "javascript"},
		{"/static/idiomorph-ext.min.js", "javascript"},
		{"/static/app.js", "javascript"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: empty body", tc.path)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.ctPart) {
			t.Fatalf("%s: content-type = %q, want it to contain %q", tc.path, ct, tc.ctPart)
		}
	}
}

func TestStaticUnknownPath404(t *testing.T) {
	h := newTestServer(t).Handler()
	code, _ := get(t, h, "/static/nope.js")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}
```

- [ ] **Step 5: Run the tests to verify they fail**

Run: `go test ./... -run 'TestStatic' -v`
Expected: FAIL — both return 200/404 mismatches because `GET /` currently matches `/static/...` and renders the dashboard page (`TestStaticUnknownPath404` fails with status 200).

- [ ] **Step 6: Create `web.go`**

```go
package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// webFS carries the dashboard's templates and static assets into the binary, so
// a release stays a single file and the assets cannot be missing at runtime.
// Templates are parsed from it at startup; web/static is served over HTTP.
//
//go:embed web/templates web/static
var webFS embed.FS

// staticSub is the web/static subtree rooted at its own directory, so an HTTP
// path of "app.js" maps to the file "web/static/app.js".
func staticSub() fs.FS {
	sub, err := fs.Sub(webFS, "web/static")
	if err != nil {
		// Unreachable: the path is a compile-time constant that go:embed verified.
		panic(err)
	}
	return sub
}

// staticHandler serves the embedded static assets under /static/. An unknown
// path 404s, and Content-Type comes from the file extension.
func staticHandler() http.Handler {
	return http.StripPrefix("/static/", http.FileServerFS(staticSub()))
}
```

- [ ] **Step 7: Register the route**

In `serve.go`, `Handler()` becomes:

```go
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /rail", s.handleRail)
	mux.HandleFunc("GET /detail", s.handleDetail)
	mux.Handle("GET /static/", staticHandler())
	return mux
}
```

Also update the doc comment above it to mention `GET /static/` (embedded assets).

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./... -run 'TestStatic' -v`
Expected: PASS for both.

- [ ] **Step 9: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS — nothing else changed yet.

- [ ] **Step 10: Commit**

```bash
git add web.go web/static serve.go serve_test.go
git commit -m "feat(serve): embed and serve vendored web assets"
```

---

## Task 2: Move templates into `web/templates/` and parse with `ParseFS`

A pure move: the four template constants become four files, the three `*template.Template` fields collapse to one, and `ui.go`/`ui_client_test.go` are deleted with the inline script replaced by a `<script src="/static/app.js">` tag. Markup is otherwise byte-identical, and the Tailwind CDN stays for now. The existing `serve_test.go` assertions are the proof this changed nothing.

**Files:**
- Create: `web/templates/page.html`, `web/templates/rail.html`, `web/templates/detail.html`, `web/templates/stepcard.html`
- Modify: `serve.go` (delete `pageTmpl`/`pageHead`/`pageTail`/`railTmpl`/`detailTmpl`/`stepcardTmpl`; `Server` fields; `NewServer`; the three handlers)
- Delete: `ui.go`, `ui_client_test.go`

**Interfaces:**
- Consumes: `webFS` from Task 1; `/static/app.js` from Task 1.
- Produces:
  - `Server.tmpl *template.Template` — one parsed set containing every define. The `page`, `rail`, `detail` fields are gone.
  - Template names available to later tasks: `"page"`, `"rail"`, `"detail"`, `"stepcard"`.

- [ ] **Step 1: Create `web/templates/stepcard.html`**

Copy the *contents* of the `stepcardTmpl` constant from `serve.go` verbatim — everything between the opening backtick and the closing backtick, i.e. starting at `{{define "stepcard"}}` and ending at `{{end}}`. Add a trailing newline. Do not reformat, re-indent, or re-wrap: any whitespace change risks a text-node diff.

- [ ] **Step 2: Create `web/templates/detail.html`**

Same procedure with the `detailTmpl` constant: starts `{{define "detail"}}`, ends `{{end}}`.

- [ ] **Step 3: Create `web/templates/rail.html`**

Same procedure with the `railTmpl` constant: starts `{{define "rail"}}`, ends `{{end}}`. Keep the `#railmeta` div for now — Task 4 replaces it.

- [ ] **Step 4: Create `web/templates/page.html`**

Copy the `pageHead` constant's contents, then the `pageTail` constant's contents, but replace the inline script. Concretely, `pageHead` ends with:

```html
</div>
<script>
```

and `pageTail` begins with:

```html
</script>
</body></html>{{end}}
```

In `page.html` those two lines become one:

```html
<script src="/static/app.js"></script>
</body></html>{{end}}
```

Everything above (`<!doctype html>` through the closing `</div>` of the flex column) is copied verbatim, including the Tailwind CDN `<script>`, the inline `tailwind.config`, the `<style>` block, and the Google Fonts links. Add a trailing newline.

- [ ] **Step 5: Delete the constants and the old client**

In `serve.go`, delete the entire `// ── templates ──` section: `pageTmpl`, `pageHead`, `pageTail`, `railTmpl`, `detailTmpl`, `stepcardTmpl`.

```bash
git rm ui.go ui_client_test.go
```

- [ ] **Step 6: Collapse the template fields**

In `serve.go`, replace these three fields on `Server`:

```go
	page   *template.Template
	rail   *template.Template
	detail *template.Template
```

with one:

```go
	tmpl *template.Template
```

- [ ] **Step 7: Parse once with `ParseFS`**

In `NewServer`, replace the three `template.New(...).Parse(...)` blocks and the return statement with:

```go
	tmpl, err := template.New("dashboard").Funcs(funcs).ParseFS(webFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{runner: r, cfg: cfg, gh: NewGitHub(r, cfg), tmpl: tmpl, ttl: defaultGHTTL, now: time.Now, prTried: map[int]bool{}}, nil
```

Update `NewServer`'s doc comment to say the templates are parsed from the embedded FS.

- [ ] **Step 8: Point the handlers at the single set**

```go
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.tmpl, "page", v)
}

func (s *Server) handleRail(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.tmpl, "rail", v)
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.tmpl, "detail", v)
}
```

- [ ] **Step 9: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS, with **zero changes to existing assertions**. `TestServeRailFragment` still passes (a fragment, not a full page) and `TestStepcardRendersTranscript` still passes.

If a test fails on missing whitespace or a missing string, the transcription in Steps 1–4 drifted — fix the template file, never the test.

- [ ] **Step 10: Verify the binary is self-contained**

```bash
go build -o /tmp/loope-embedcheck . && ls -l /tmp/loope-embedcheck && rm /tmp/loope-embedcheck
```

Expected: builds clean. (`go:embed` fails the build outright if `web/templates` or `web/static` is missing or empty, so a successful build is the proof.)

- [ ] **Step 11: Commit**

```bash
git add web/templates serve.go
git commit -m "refactor(serve): move dashboard templates into web/templates"
```

---

## Task 3: HTMX polling wiring

Replaces the deleted `setInterval`/`fetch` loop with declarative htmx attributes on the rail and main panes, loading htmx and the morph extension from `/static/`.

**Files:**
- Modify: `web/templates/page.html`
- Test: `serve_test.go` (append)

**Interfaces:**
- Consumes: `/static/htmx.min.js`, `/static/idiomorph-ext.min.js`, `/static/app.js` (Task 1); `web/templates/page.html` (Task 2).
- Produces: `<body hx-ext="morph">`; `nav#rail` and `main#main` carrying `hx-get`, `hx-trigger="every 3s"`, `hx-swap="morph:innerHTML"`.

- [ ] **Step 1: Write the failing tests**

Append to `serve_test.go`:

```go
func TestPageWiresHTMXPolling(t *testing.T) {
	h := newTestServer(t).Handler()
	code, body := get(t, h, "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		`src="/static/htmx.min.js"`,
		`src="/static/idiomorph-ext.min.js"`,
		`src="/static/app.js"`,
		`hx-ext="morph"`,
		`hx-get="/rail?issue=142"`,
		`hx-get="/detail?issue=142"`,
		`hx-trigger="every 3s"`,
		`hx-swap="morph:innerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}
}

// The rail's ticket rows and the pipeline's step items must stay keyed, so
// idiomorph matches them by identity instead of rebuilding them on every poll.
func TestPollFragmentsAreKeyed(t *testing.T) {
	h := newTestServer(t).Handler()
	_, rail := get(t, h, "/rail?issue=142")
	if !strings.Contains(rail, `data-k="t142"`) {
		t.Fatalf("rail row not keyed: %s", rail)
	}
	_, detail := get(t, h, "/detail?issue=142")
	if !strings.Contains(detail, `data-k="s1"`) {
		t.Fatalf("step item not keyed: %s", detail)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestPageWiresHTMXPolling|TestPollFragmentsAreKeyed' -v`
Expected: `TestPageWiresHTMXPolling` FAILs on `src="/static/htmx.min.js"`. `TestPollFragmentsAreKeyed` should already PASS — the keys survive from the current markup; it is a regression guard, not new behaviour.

- [ ] **Step 3: Load htmx in `page.html`**

In the `<head>`, immediately after the existing `<script>` block that sets `tailwind.config` (and before `<style>`), add:

```html
<script src="/static/htmx.min.js"></script>
<script src="/static/idiomorph-ext.min.js"></script>
```

- [ ] **Step 4: Enable the extension on `<body>`**

```html
<body class="font-sans text-text antialiased" hx-ext="morph">
```

- [ ] **Step 5: Add the polling attributes to the two panes**

Replace the rail/main container line:

```html
 <div class="flex min-h-0 flex-1">
  <nav id="rail" class="scroll w-[320px] shrink-0 overflow-y-auto border-r border-line bg-panel"
       hx-get="/rail?issue={{if .Selected}}{{.Selected.Number}}{{end}}" hx-trigger="every 3s" hx-swap="morph:innerHTML">{{template "rail" .}}</nav>
  <main id="main" class="scroll min-w-0 flex-1 overflow-y-auto"
        hx-get="/detail?issue={{if .Selected}}{{.Selected.Number}}{{end}}" hx-trigger="every 3s" hx-swap="morph:innerHTML">{{template "detail" .}}</main>
 </div>
```

The selected issue is baked into the URL server-side because ticket selection is still a full-page `<a href="/?issue=N">` link — the selection cannot change for the life of the page. With no tickets, this renders `/rail?issue=`, which `load` treats exactly like an absent query.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./... -run 'TestPageWiresHTMXPolling|TestPollFragmentsAreKeyed' -v`
Expected: both PASS.

- [ ] **Step 7: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 8: Verify the idiomorph callback actually fires**

This is the one genuine unknown in the design: whether the htmx morph extension honours a callback installed globally on `Idiomorph.defaults.callbacks`, or overrides it with its own config object. **Verify; do not assume.**

Start the dashboard against real logs and open it:

```bash
go run . -serve -config loope.json -addr localhost:8080
```

In the browser console on the loaded page, run:

```js
Idiomorph.defaults.callbacks.beforeAttributeUpdated
```

Expected: prints the function from `app.js`, not `undefined`.

Then, on a *running* step, close the transcript disclosure and watch it across at least three 3-second polls. Expected: it stays closed.

- [ ] **Step 9: If — and only if — the disclosure re-opens, install the fallback swap**

The extension is overriding the global callbacks. Replace the `if (window.Idiomorph) { ... }` block at the bottom of `web/static/app.js` with a custom htmx extension that calls `Idiomorph.morph` directly with our config:

```javascript
// The htmx morph extension ignores Idiomorph.defaults.callbacks, so the swap is
// registered here instead and passes the config explicitly. Same behaviour,
// about ten more lines.
htmx.defineExtension('morphpolicy', {
  isInlineSwap: function (swapStyle) { return swapStyle === 'morph:innerHTML'; },
  handleSwap: function (swapStyle, target, fragment) {
    if (swapStyle !== 'morph:innerHTML') return false;
    return Idiomorph.morph(target, fragment.children, {
      morphStyle: 'innerHTML',
      callbacks: {
        beforeAttributeUpdated: function (name, node) {
          if (name === 'open' && node.hasAttribute && node.hasAttribute('data-disc')) return false;
        },
      },
    });
  },
});
```

and change `page.html`'s body attribute to `hx-ext="morph,morphpolicy"`. Re-run Step 8's verification; the disclosure must now stay closed. Update `TestPageWiresHTMXPolling`'s `hx-ext="morph"` expectation to `hx-ext="morph,morphpolicy"` if you take this branch.

- [ ] **Step 10: Commit**

```bash
git add web/templates/page.html web/static/app.js serve_test.go
git commit -m "feat(serve): poll the rail and detail panes with htmx morph swaps"
```

---

## Task 4: Out-of-band header stats

Deletes the hidden `#railmeta` element and lets htmx relocate the header counters itself, so `app.js` needs no stat-patching code at all.

**Files:**
- Modify: `web/templates/rail.html`, `web/templates/page.html`, `serve.go` (`handleRail` renders `"railpoll"`)
- Test: `serve_test.go` (append)

**Interfaces:**
- Consumes: the `view` payload (`.Stats.Tickets`, `.Stats.Running`, `.Stats.Spend`) and the `dollars` func.
- Produces: templates `"statbar"`, `"statbar-oob"`, `"railpoll"`. `handleRail` renders `"railpoll"`; `handleIndex` still renders `"page"`.

- [ ] **Step 1: Write the failing test**

Append to `serve_test.go`:

```go
func TestRailFragmentCarriesOOBStatbar(t *testing.T) {
	h := newTestServer(t).Handler()
	code, body := get(t, h, "/rail?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Fatalf("rail fragment carries no out-of-band statbar: %s", body)
	}
	if !strings.Contains(body, `id="statbar"`) {
		t.Fatalf("out-of-band statbar has no id to swap into: %s", body)
	}
	// The stats the header shows must be in the fragment: one ticket, none
	// running (the seeded step is settled), $0.51 spent.
	for _, want := range []string{`id="stat-tickets" class="text-base font-semibold tabular-nums text-text">1<`, `id="stat-running">0<`, `>$0.51<`} {
		if !strings.Contains(body, want) {
			t.Fatalf("statbar missing %q: %s", want, body)
		}
	}
	// The dead patching hook is gone.
	if strings.Contains(body, "railmeta") {
		t.Fatalf("rail still emits the obsolete #railmeta element")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./... -run TestRailFragmentCarriesOOBStatbar -v`
Expected: FAIL on `hx-swap-oob="true"`.

- [ ] **Step 3: Extract the statbar defines into `rail.html`**

Append to `web/templates/rail.html` (after the existing `{{define "rail"}}...{{end}}`):

```html
{{define "statbar"}}
   <div class="hidden items-baseline gap-1.5 md:flex"><span id="stat-tickets" class="text-base font-semibold tabular-nums text-text">{{.Stats.Tickets}}</span><span class="text-faint">tickets</span></div>
   <div class="hidden h-4 w-px bg-line2 md:block"></div>
   <div class="hidden items-baseline gap-1.5 md:flex"><span class="flex items-center gap-1.5 text-base font-semibold tabular-nums text-live"><span class="hb inline-block h-1.5 w-1.5 rounded-full bg-live"></span><span id="stat-running">{{.Stats.Running}}</span></span><span class="text-faint">running</span></div>
   <div class="hidden h-4 w-px bg-line2 md:block"></div>
   <div class="flex items-baseline gap-1.5"><span id="stat-spend" class="text-base font-semibold tabular-nums text-text">{{dollars .Stats.Spend}}</span><span class="text-faint">spend</span></div>
   <div class="h-4 w-px bg-line2"></div>
   <div class="flex items-center gap-1.5 text-faint"><span class="hb inline-block h-1.5 w-1.5 rounded-full bg-live"></span><span>live · <span id="ago" class="tabular-nums text-muted">0s</span> ago</span></div>
{{end}}

{{define "statbar-oob"}}<div id="statbar" hx-swap-oob="true" class="flex items-center gap-4 font-mono text-xs sm:gap-5">{{template "statbar" .}}</div>{{end}}

{{define "railpoll"}}{{template "rail" .}}{{template "statbar-oob" .}}{{end}}
```

`statbar-oob` is only ever rendered as part of `railpoll`, never inline in the page — an OOB element in the initial document would render as a visible duplicate.

- [ ] **Step 4: Delete `#railmeta` from the rail define**

Remove this line from the top of `{{define "rail"}}` in `web/templates/rail.html`:

```html
 <div id="railmeta" hidden data-tickets="{{.Stats.Tickets}}" data-running="{{.Stats.Running}}" data-spend="{{dollars .Stats.Spend}}"></div>
```

- [ ] **Step 5: Render the statbar through the shared define in `page.html`**

In the header, replace the whole stats block:

```html
  <div class="flex items-center gap-4 font-mono text-xs sm:gap-5">
   <div class="hidden items-baseline gap-1.5 md:flex"><span id="stat-tickets" ...
   ... through ...
   <div class="flex items-center gap-1.5 text-faint">...live · <span id="ago" ...</div>
  </div>
```

with:

```html
  <div id="statbar" class="flex items-center gap-4 font-mono text-xs sm:gap-5">{{template "statbar" .}}</div>
```

The wrapper's classes are unchanged from today's `<div class="flex items-center gap-4 font-mono text-xs sm:gap-5">`, and `statbar-oob` repeats them exactly — `hx-swap-oob="true"` is an `outerHTML` swap, so a class mismatch would silently change the header layout on the first poll.

- [ ] **Step 6: Render `railpoll` from `handleRail`**

In `serve.go`:

```go
// handleRail renders the left-rail poll fragment plus the out-of-band header
// statbar, which htmx relocates into the page header itself.
func (s *Server) handleRail(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.tmpl, "railpoll", v)
}
```

- [ ] **Step 7: Run the test to verify it passes**

Run: `go test ./... -run TestRailFragmentCarriesOOBStatbar -v`
Expected: PASS.

- [ ] **Step 8: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS, including the untouched `TestServeRailFragment`.

- [ ] **Step 9: Commit**

```bash
git add web/templates serve.go serve_test.go
git commit -m "feat(serve): swap header stats out-of-band from the rail poll"
```

---

## Task 5: Vendored Tailwind build

Replaces the browser-JIT CDN with a committed, prebuilt `app.css`, and guards the manual regeneration step with a test.

**Files:**
- Create: `web/tailwind.css`, `web/static/app.css` (generated)
- Modify: `web/templates/page.html`
- Test: `serve_test.go` (append)

**Interfaces:**
- Consumes: `web/templates/*.html` and `*.go` as Tailwind class sources; `staticHandler()` from Task 1.
- Produces: `/static/app.css`, containing every utility used by the templates *and* by the Go class helpers.

- [ ] **Step 1: Install the Tailwind v4 standalone CLI**

A single binary — no npm, no `package.json`, no Node. On Apple Silicon:

```bash
curl -fsSL -o /tmp/tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/latest/download/tailwindcss-macos-arm64
chmod +x /tmp/tailwindcss
/tmp/tailwindcss --help | head -5
```

Expected: prints the CLI usage banner. (Use `tailwindcss-macos-x64` or `tailwindcss-linux-x64` as appropriate.) Do **not** commit the binary.

- [ ] **Step 2: Write `web/tailwind.css`**

```css
/* Tailwind source for the loope dashboard.
 *
 * web/static/app.css is generated from this file and committed. Regenerate it
 * after changing any Tailwind class — in a template OR in a Go helper:
 *
 *   tailwindcss -i web/tailwind.css -o web/static/app.css --minify
 */
@import "tailwindcss";

/* Classes are constructed in Go as well as in templates (stateKind, stripeClass,
 * nodeClass, cardClass, divClass, statusChip), so both trees must be scanned or
 * the Go-only classes get tree-shaken out and the dashboard renders half-styled. */
@source "./templates/*.html";
@source "../*.go";

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

- [ ] **Step 3: Generate `app.css`**

```bash
/tmp/tailwindcss -i web/tailwind.css -o web/static/app.css --minify
wc -c web/static/app.css
```

Expected: the CLI reports "Done in Nms" and the file is tens of KB, not a few hundred bytes. A tiny file means the `@source` globs matched nothing — check that the command ran from the repo root.

- [ ] **Step 4: Confirm both source globs were scanned, and capture the sentinel spellings**

```bash
grep -o 'line-clamp-2' web/static/app.css | head -1
grep -o 'bg-ok[^{,: ]*' web/static/app.css | sort -u | head
```

Expected: `line-clamp-2` (used only in `web/templates/rail.html`) prints. The second command lists the `bg-ok` selectors, including the escaped form of `bg-ok/50` — which exists only because `stripeClass` in `serve.go` produces it. Note the exact escaped text (likely `bg-ok\/50`); Step 5's test asserts on it.

If `bg-ok/50` is absent, `@source "../*.go"` did not take effect — fix that before continuing, because the whole point of the guard is that Go-only classes survive.

- [ ] **Step 5: Write the failing stale-CSS guard test**

Append to `serve_test.go`, substituting the exact escaped selector observed in Step 4 for `bg-ok\/50` if it differs:

```go
// TestAppCSSCoversBothClassSources is the guard against the manual Tailwind
// regeneration step being skipped. Half the dashboard's classes exist only in
// templates and half only in Go helpers, so app.css is checked for one sentinel
// from each source. A miss means someone changed classes without re-running:
//
//	tailwindcss -i web/tailwind.css -o web/static/app.css --minify
func TestAppCSSCoversBothClassSources(t *testing.T) {
	css, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if len(css) < 4096 {
		t.Fatalf("app.css is only %d bytes — the Tailwind build produced nothing useful", len(css))
	}
	for _, want := range []string{
		`line-clamp-2`, // template-only: web/templates/rail.html
		`bg-ok\/50`,    // Go-only: stripeClass in render.go
	} {
		if !strings.Contains(string(css), want) {
			t.Fatalf("app.css missing %q — regenerate it: tailwindcss -i web/tailwind.css -o web/static/app.css --minify", want)
		}
	}
}

func TestStaticCSSServed(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("empty body")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("content-type = %q, want text/css", ct)
	}
}

func TestPageLinksVendoredCSSNotCDN(t *testing.T) {
	h := newTestServer(t).Handler()
	_, body := get(t, h, "/")
	if !strings.Contains(body, `href="/static/app.css"`) {
		t.Fatalf("page does not link the vendored stylesheet")
	}
	if strings.Contains(body, "cdn.tailwindcss.com") {
		t.Fatalf("page still loads the Tailwind browser CDN")
	}
}
```

- [ ] **Step 6: Run the tests to verify the right ones fail**

Run: `go test ./... -run 'TestAppCSS|TestStaticCSSServed|TestPageLinksVendoredCSSNotCDN' -v`
Expected: `TestAppCSSCoversBothClassSources` and `TestStaticCSSServed` PASS (the file exists from Step 3); `TestPageLinksVendoredCSSNotCDN` FAILs — the page still uses the CDN.

- [ ] **Step 7: Swap the CDN for the vendored stylesheet**

In `web/templates/page.html`, delete these three things from the `<head>`:

1. `<script src="https://cdn.tailwindcss.com"></script>`
2. the `<script>tailwind.config={...}</script>` block
3. the entire `<style>...</style>` block (its contents now live in `web/tailwind.css`)

and put in their place, immediately after the Google Fonts `<link>`:

```html
<link rel="stylesheet" href="/static/app.css">
```

The Google Fonts `<link>` and `<preconnect>` tags stay exactly as they are — an offline dashboard falls back to `system-ui` / `ui-monospace`, which is expected and acceptable.

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./... -run 'TestAppCSS|TestStaticCSSServed|TestPageLinksVendoredCSSNotCDN' -v`
Expected: all three PASS.

- [ ] **Step 9: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 10: Eyeball the rendered page against today's dashboard**

```bash
go run . -serve -config loope.json -addr localhost:8080
```

Open it and compare against the pre-change dashboard (`git stash` a copy or a screenshot taken before starting). The requirement is pixel-identical: same palette, same fonts, same spacing, same chips, same spine, same scrollbar styling. Investigate any difference — a missing utility means a class the extractor did not find.

- [ ] **Step 11: Commit**

```bash
git add web/tailwind.css web/static/app.css web/templates/page.html serve_test.go
git commit -m "build(web): vendor a prebuilt Tailwind stylesheet"
```

---

## Task 6: Split the template helpers into `render.go`

A pure move with no behaviour change: `serve.go` is left with HTTP concerns only. Its own tests are the existing `serve_test.go` helper tests (`TestTokensHumanize`, `TestDurationFormat`, `TestCtxTokensAndHasUsage`, `TestTxLineEscapesHTML`), which must keep passing untouched.

**Files:**
- Create: `render.go`
- Modify: `serve.go`

**Interfaces:**
- Consumes: `Config`, `Ticket`, `Step`, `StepStatus`, `TranscriptEvent` (unchanged types); `hasAnswerer` and `pipelineRows` stay in `tracker.go`.
- Produces: `func templateFuncs(cfg *Config) template.FuncMap` — the single entry point `NewServer` uses. Every helper listed below keeps its exact current name and signature.

- [ ] **Step 1: Create `render.go` with the helpers**

Move these declarations out of `serve.go`'s `// ── template helpers ──` section into a new `render.go`, **verbatim, comments included**, in this order: `hasRunning`, `errCount`, `money`, `dollars`, `tokens`, `duration`, `ctxTokens`, `hasUsage`, `txLine`, `short`, `shortid`, `stateKind`, `stripeClass`, `nodeClass`, `cardClass`, `divClass`, `statusChip`.

The file starts:

```go
package main

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
)

// This file holds the dashboard's presentation layer: the pure formatters and
// class-pickers the templates call, plus the FuncMap that binds them. serve.go
// is left with HTTP concerns only.
```

`hasAnswerer` and `pipelineRows` are *not* moved — they live in `tracker.go` and stay there.

- [ ] **Step 2: Move the FuncMap**

Cut the `funcs := template.FuncMap{...}` literal out of `NewServer` and add it to `render.go` as:

```go
// templateFuncs binds the presentation helpers for one Server. The closures
// capture cfg because label semantics and the issue URL are per-repository.
func templateFuncs(cfg *Config) template.FuncMap {
	return template.FuncMap{
		"money":        money,
		"dollars":      dollars,
		"short":        short,
		"shortid":      shortid,
		"hasRunning":   hasRunning,
		"errCount":     errCount,
		"statusChip":   statusChip,
		"nodeClass":    nodeClass,
		"cardClass":    cardClass,
		"divClass":     divClass,
		"stateKind":    func(label string) string { return stateKind(cfg, label) },
		"issueURL":     func(n int) string { return "https://github.com/" + cfg.RepoSlug + "/issues/" + strconv.Itoa(n) },
		"stripeClass":  func(label string) string { return stripeClass(cfg, label) },
		"tokens":       tokens,
		"duration":     duration,
		"ctxTokens":    ctxTokens,
		"hasUsage":     hasUsage,
		"hasAnswerer":  hasAnswerer,
		"pipelineRows": pipelineRows,
		"txLine":       txLine,
	}
}
```

- [ ] **Step 3: Call it from `NewServer`**

```go
func NewServer(r Runner, cfg *Config) (*Server, error) {
	tmpl, err := template.New("dashboard").Funcs(templateFuncs(cfg)).ParseFS(webFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{runner: r, cfg: cfg, gh: NewGitHub(r, cfg), tmpl: tmpl, ttl: defaultGHTTL, now: time.Now, prTried: map[int]bool{}}, nil
}
```

- [ ] **Step 4: Prune `serve.go`'s imports**

Drop any import `serve.go` no longer uses (`math` certainly; check `strconv` — still needed by `load`'s `strconv.Atoi`).

Run: `go build ./...`
Expected: clean build. `go vet` catches an unused import as a compile error, so a failure here names the exact line.

- [ ] **Step 5: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS with no test changes at all.

- [ ] **Step 6: Confirm `serve.go` actually shrank**

```bash
wc -l serve.go render.go
```

Expected: `serve.go` is roughly 250–350 lines (from 682), and nothing was lost — `render.go` holds the difference.

- [ ] **Step 7: Commit**

```bash
git add serve.go render.go
git commit -m "refactor(serve): split template helpers into render.go"
```

---

## Task 7: Documentation and manual verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the web assets in the README**

In the `## Progress dashboard (`loope -serve`)` section, after the paragraph ending "…both stop together on a signal. If the dashboard listener fails, the worker keeps running; the error is only logged.", add:

````markdown
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
````

- [ ] **Step 2: Verify the README renders**

```bash
grep -n "tailwindcss -i web/tailwind.css" README.md
```

Expected: one hit, inside the dashboard section.

- [ ] **Step 3: Run the manual verification checklist**

Start the dashboard against a work directory with real logs, ideally with a pipeline actually running:

```bash
go run . -serve -config loope.json -addr localhost:8080
```

Check every item — these are the behaviours the Go tests cannot prove:

- [ ] Open a transcript disclosure; it stays open across at least three polls (≥ 10s).
- [ ] Close a **running** step's disclosure; it stays closed across at least three polls.
- [ ] Scroll a transcript up; the offset holds, while a separate bottom-pinned feed keeps following new lines.
- [ ] No whole-screen fadein flash on poll — only genuinely new content animates.
- [ ] Header counters (tickets / running / spend) update on poll, and "live · Ns ago" resets to `0s` each poll and counts up between them.
- [ ] With the network offline (DevTools → Network → Offline, then reload from cache, or unplug and restart), the page still renders fully styled. System fonts substituting for IBM Plex is expected and acceptable.
- [ ] Ticket selection still works as a full-page navigation, and the newly selected ticket keeps polling.
- [ ] The "GitHub unreachable" banner still appears when `gh` is unavailable (temporarily point `repoSlug` at a nonexistent repo).
- [ ] The copy-session-id button copies the full id and flashes "copied".
- [ ] The two-column architect/answerer layout renders for a feature pipeline, and the single-column spine for a bug pipeline.

- [ ] **Step 4: Confirm the old world is gone**

```bash
git ls-files | grep -E '^(ui\.go|ui_client_test\.go)$' ; echo "exit=$?"
grep -rn "pageTmpl\|railTmpl\|detailTmpl\|stepcardTmpl\|uiJS\|railmeta" --include='*.go' . ; echo "exit=$?"
```

Expected: both print `exit=1` with no matches — the constants, the client script, and its node-based test are all deleted.

- [ ] **Step 5: Final full verification**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "docs: document the embedded web assets and Tailwind regeneration"
```
