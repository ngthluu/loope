# Open the Worktree in VS Code Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a chip to the dashboard's ticket detail page that opens that ticket's git worktree folder in VS Code via a `vscode://file/<path>` URI, disabled when the folder no longer exists.

**Architecture:** Purely client-side: the server computes the worktree path with the existing `worktreePath()` helper, `os.Stat`s it once per detail render for the selected ticket only, and puts both the path and a `template.URL`-typed `vscode://` URI on the `view` payload. `web/templates/detail.html` renders an `<a>` when the URI is set and an inert `<span>` with an explanatory `title` when it is not. No new HTTP route, no server-side process spawning.

**Tech Stack:** Go 1.x standard library (`net/url`, `html/template`, `os`), htmx-polled server-rendered fragments, Tailwind (pre-built, checked-in `web/static/app.css`), Go `testing` with `httptest`.

## Global Constraints

- Path source of truth: `worktreePath(cfg.WorkDir, issueNum)` in `worktree.go:26` — never re-derive `<workDir>/issue-<N>` by hand.
- URI form is exactly `vscode://file/<absolute-path>` (scheme `vscode`, host `file`). Built with `url.URL` so paths containing spaces are percent-encoded.
- `WorktreeURI` must be typed `template.URL`, not `string`. `html/template`'s URL filter allows only `http`, `https`, `mailto` and relative URLs; a plain string renders as `#ZgotmplZ` and the chip silently does nothing.
- VS Code only. No Cursor, no `editor` config key, no new config surface.
- Detail page only. `web/templates/rail.html`, `Ticket`, and `scanLogs` are untouched.
- The chip is never gated behind a control flag. The only disabled condition is a missing worktree directory.
- Exactly one `os.Stat` per detail render, and only for `v.Selected`.
- Chip label text, both states: `open in VS Code ↗`.
- Only Tailwind utility classes already present in `web/static/app.css` may be used. If any new class is introduced, regenerate with `tailwindcss -i web/tailwind.css -o web/static/app.css --minify` (guarded by `TestAppCSSCoversBothClassSources` in `serve_test.go:759`).
- No existing test may be modified; the added row does not change any asserted markup.

## Assumptions (headless calls made while writing this plan)

1. The spec says the `worktreeURI` unit test belongs to "`render.go`'s test coverage". This repo has no `render_test.go` — render helpers are tested from `tracker_test.go` and `serve_test.go`. All three new tests go in `serve_test.go`, which already holds the dashboard-rendering tests and the `newTestServer` helper. Same package, so nothing is lost.
2. The stat helper lives in `serve.go` next to `load()` rather than in `worktree.go`, because it is a server-render concern (`worktree.go` is the pipeline's git wrapper and has no dependency on the dashboard).
3. `WorktreePath` is always set for a selected ticket even when the directory is absent, because the disabled tooltip names the missing path.

---

## File Structure

- `render.go` — add `worktreeURI(path string) template.URL`, a pure presentation function, beside the other formatters. Not registered in `templateFuncs`: it is called from Go (`load()`), not from a template.
- `serve.go` — add `WorktreePath` / `WorktreeURI` to `view` (struct at `serve.go:91`); populate them in `load()` right after the `backfillPR` / `backfillTitle` calls.
- `web/templates/detail.html` — the chip's two variants, appended to the `issue ↗ / pull request ↗` row.
- `serve_test.go` — three tests: `worktreeURI` escaping, enabled chip, disabled chip.

---

### Task 1: The `worktreeURI` helper

**Files:**
- Modify: `render.go` (add function + `net/url` import)
- Test: `serve_test.go` (append at end of file)

**Interfaces:**
- Consumes: nothing.
- Produces: `func worktreeURI(path string) template.URL` — takes an absolute filesystem path, returns a `vscode://file/<path>` URI with the path percent-encoded.

- [ ] **Step 1: Write the failing test**

Append to `serve_test.go`:

```go
// TestWorktreeURIEscapesPath pins the exact scheme/host prefix the VS Code
// handler expects and proves url.URL does the percent-encoding — workDir on
// macOS frequently contains spaces, which manual string concatenation would
// emit raw and the browser would refuse.
func TestWorktreeURIEscapesPath(t *testing.T) {
	got := string(worktreeURI("/Users/me/my work/issue-7"))
	want := "vscode://file/Users/me/my%20work/issue-7"
	if got != want {
		t.Fatalf("worktreeURI = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "vscode://file/") {
		t.Fatalf("worktreeURI = %q, want vscode://file/ prefix", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestWorktreeURIEscapesPath`
Expected: FAIL — compile error `undefined: worktreeURI`.

- [ ] **Step 3: Write minimal implementation**

In `render.go`, add `"net/url"` to the import block (keeping the existing gofmt grouping — the block currently is `fmt`, `html/template`, `math`, `strconv`; `net/url` sorts after `math`), then append this function after `templateFuncs`:

```go
// worktreeURI is the vscode://file/<path> URI that asks the OS to open a
// worktree folder in VS Code. It returns template.URL because html/template
// rewrites href values whose scheme is not http/https/mailto to "#ZgotmplZ",
// which would leave the chip silently inert. url.URL does the escaping, so a
// workDir containing spaces still produces a URI the browser accepts.
func worktreeURI(path string) template.URL {
	u := url.URL{Scheme: "vscode", Host: "file", Path: path}
	return template.URL(u.String())
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./... -run TestWorktreeURIEscapesPath`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add render.go serve_test.go
git commit -m "feat: add worktreeURI helper for vscode://file links"
```

---

### Task 2: `view` fields populated in `load()`

**Files:**
- Modify: `serve.go` (the `view` struct at `serve.go:91`; `load()` just before its `return v`)
- Test: `serve_test.go` (append at end of file)

**Interfaces:**
- Consumes: `worktreeURI(path string) template.URL` from Task 1; `worktreePath(workDir string, issueNum int) string` from `worktree.go:26`.
- Produces: `view.WorktreePath string` (always set when `v.Selected != nil`) and `view.WorktreeURI template.URL` (set only when the directory exists). Both stay zero when `v.Selected` is nil.

- [ ] **Step 1: Write the failing test**

Append to `serve_test.go`:

```go
// TestLoadSetsWorktreeFieldsWhenDirExists covers both halves of the one stat
// load() runs for the selected ticket: the path is always reported so the
// disabled tooltip can name it, and the URI appears only while the folder is
// on disk. The second half also documents the live behaviour — the detail pane
// re-polls, so removing the worktree flips the chip off with no cache to bust.
func TestLoadSetsWorktreeFieldsWhenDirExists(t *testing.T) {
	s := newTestServer(t)
	wt := worktreePath(s.cfg.WorkDir, 142)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	v := s.load(context.Background(), "142")
	if v.WorktreePath != wt {
		t.Fatalf("WorktreePath = %q, want %q", v.WorktreePath, wt)
	}
	if string(v.WorktreeURI) != string(worktreeURI(wt)) {
		t.Fatalf("WorktreeURI = %q, want %q", v.WorktreeURI, worktreeURI(wt))
	}

	if err := os.Remove(wt); err != nil {
		t.Fatal(err)
	}
	v = s.load(context.Background(), "142")
	if v.WorktreePath != wt {
		t.Fatalf("WorktreePath = %q after removal, want %q", v.WorktreePath, wt)
	}
	if v.WorktreeURI != "" {
		t.Fatalf("WorktreeURI = %q after removal, want empty", v.WorktreeURI)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./... -run TestLoadSetsWorktreeFieldsWhenDirExists`
Expected: FAIL — compile error `v.WorktreePath undefined (type view has no field or method WorktreePath)`.

- [ ] **Step 3: Add the two `view` fields**

Replace the `view` struct in `serve.go` with:

```go
// view is the template payload for one render.
type view struct {
	Tickets  []Ticket
	Selected *Ticket
	GHError  string
	Notice   string
	Stats    stats
	// WorktreePath is where the selected ticket's worktree lives, set even
	// when the folder is gone so the disabled chip can name it.
	WorktreePath string
	// WorktreeURI is set only while that folder exists; an empty value is
	// what switches the detail chip to its disabled variant.
	WorktreeURI template.URL
}
```

Ensure `serve.go` imports `html/template` (it already parses templates; if the import is absent, add it in the stdlib group).

- [ ] **Step 4: Populate the fields in `load()`**

In `serve.go`, replace the tail of `load()`:

```go
	s.backfillPR(ctx, v.Selected)
	s.backfillTitle(ctx, v.Selected)
	return v
}
```

with:

```go
	s.backfillPR(ctx, v.Selected)
	s.backfillTitle(ctx, v.Selected)
	s.setWorktree(&v)
	return v
}

// setWorktree fills the selected ticket's worktree path and, when the folder is
// actually on disk, the vscode:// URI the detail chip links to. One stat per
// render, for the ticket on screen only — the rail has no use for it. A missing
// folder is the normal end state: the pipeline removes the worktree once the
// ticket is done, and the chip renders disabled from then on.
func (s *Server) setWorktree(v *view) {
	if v.Selected == nil {
		return
	}
	path := worktreePath(s.cfg.WorkDir, v.Selected.Number)
	v.WorktreePath = path
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		v.WorktreeURI = worktreeURI(path)
	}
}
```

Ensure `serve.go` imports `os` (add to the stdlib import group if missing).

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./... -run TestLoadSetsWorktreeFieldsWhenDirExists`
Expected: PASS.

- [ ] **Step 6: Run the full suite for regressions**

Run: `go test ./...`
Expected: PASS — no existing test asserts on `view`'s field count or on detail markup that changed.

- [ ] **Step 7: Commit**

```bash
git add serve.go serve_test.go
git commit -m "feat: expose the selected ticket's worktree path and URI on the view"
```

---

### Task 3: The detail-page chip

**Files:**
- Modify: `web/templates/detail.html` (the `mt-3 flex flex-wrap` link row, immediately after the `pull request ↗` anchor)
- Test: `serve_test.go` (append at end of file)

**Interfaces:**
- Consumes: `view.WorktreePath` and `view.WorktreeURI` from Task 2. The row sits inside `{{with .Selected}}`, so view-level fields are reached through `$`.
- Produces: rendered markup only — nothing downstream depends on it.

- [ ] **Step 1: Write the two failing tests**

Append to `serve_test.go`:

```go
// TestDetailShowsEnabledVSCodeChip asserts the live worktree renders a real
// vscode://file/ href. The ZgotmplZ check is the regression guard for the
// template.URL requirement: html/template blanks non-http schemes typed as
// plain strings, which would leave a chip that looks fine and does nothing.
func TestDetailShowsEnabledVSCodeChip(t *testing.T) {
	s := newTestServer(t)
	wt := worktreePath(s.cfg.WorkDir, 142)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	code, body := get(t, s.Handler(), "/detail?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	want := `href="` + string(worktreeURI(wt)) + `"`
	if !strings.Contains(body, want) {
		t.Fatalf("detail missing %q", want)
	}
	if !strings.Contains(body, "open in VS Code") {
		t.Fatalf("detail missing the chip label")
	}
	if strings.Contains(body, "ZgotmplZ") {
		t.Fatalf("detail sanitized the vscode:// href — WorktreeURI must be template.URL")
	}
}

// TestDetailShowsDisabledVSCodeChip covers the post-merge state: the worktree
// is gone, so the chip is still there (so the row does not jump) but inert,
// with a tooltip naming the path and explaining the absence.
func TestDetailShowsDisabledVSCodeChip(t *testing.T) {
	s := newTestServer(t)
	wt := worktreePath(s.cfg.WorkDir, 142)
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %q should not exist in a fresh test server", wt)
	}
	code, body := get(t, s.Handler(), "/detail?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "open in VS Code") {
		t.Fatalf("detail missing the chip label")
	}
	if !strings.Contains(body, "No worktree folder at "+wt) {
		t.Fatalf("detail missing the disabled tooltip naming %q", wt)
	}
	if strings.Contains(body, "vscode://") {
		t.Fatalf("detail still emits a vscode:// link for a missing worktree")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestDetailShows(Enabled|Disabled)VSCodeChip'`
Expected: FAIL — `detail missing "href=\"vscode://file/..."` and `detail missing the chip label`.

- [ ] **Step 3: Add the chip to the template**

In `web/templates/detail.html`, replace this line (the `pull request ↗` line inside the `mt-3 flex flex-wrap items-center gap-2 font-mono text-[11px]` div):

```
    {{if .PRURL}}<a href="{{.PRURL}}" target="_blank" rel="noopener" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/40">pull request ↗</a>{{end}}
```

with:

```
    {{if .PRURL}}<a href="{{.PRURL}}" target="_blank" rel="noopener" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/40">pull request ↗</a>{{end}}
    {{if $.WorktreeURI}}<a href="{{$.WorktreeURI}}" title="Open {{$.WorktreePath}} in VS Code" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/40">open in VS Code ↗</a>{{else}}<span title="No worktree folder at {{$.WorktreePath}} — it is removed once the ticket is done." class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-faint">open in VS Code ↗</span>{{end}}
```

The disabled state is a `<span>`, not a disabled `<a>`: an anchor without `href` is already unclickable and unfocusable, so no ARIA or `pointer-events` handling is needed, and `title` supplies the hover tooltip.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./... -run 'TestDetailShows(Enabled|Disabled)VSCodeChip'`
Expected: PASS.

- [ ] **Step 5: Confirm no new Tailwind classes were introduced**

Every class used above (`inline-flex`, `items-center`, `gap-1`, `rounded`, `border`, `border-line2`, `bg-panel`, `px-2`, `py-0.5`, `text-muted`, `text-faint`, `hover:text-text`, `hover:border-live/40`) already appears elsewhere in `web/templates/detail.html`, so the checked-in build stays valid.

Run: `go test ./... -run TestAppCSSCoversBothClassSources`
Expected: PASS. If it fails, a new class slipped in — regenerate:
`tailwindcss -i web/tailwind.css -o web/static/app.css --minify`

- [ ] **Step 6: Run the full suite**

Run: `go test ./...`
Expected: PASS, including `TestDetailShowsGitHubLinksAndSession` (it renders the `detail` template with a zero `WorktreeURI`, hitting the disabled `<span>` branch, and asserts nothing about this row).

- [ ] **Step 7: Commit**

```bash
git add web/templates/detail.html serve_test.go
git commit -m "feat: add 'open in VS Code' chip to the ticket detail page"
```

---

### Task 4: Manual verification and vet

**Files:**
- Modify: none (verification only)

**Interfaces:**
- Consumes: everything from Tasks 1–3.
- Produces: nothing.

- [ ] **Step 1: Vet and format**

Run: `gofmt -l . && go vet ./...`
Expected: no file names listed by `gofmt`, no vet diagnostics.

- [ ] **Step 2: Full suite once more**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 3: Eyeball the rendered chip**

Start the dashboard against a work dir that has at least one `issue-<N>` worktree, open a ticket's detail page, and confirm:
- With the worktree present: the chip is a link, hover shows `Open <path> in VS Code`, clicking opens the folder in VS Code (an OS "no application" prompt is an acceptable outcome on a machine without VS Code — the spec accepts that failure mode).
- With the worktree removed: the chip greys out on the next poll, hover shows `No worktree folder at <path> — it is removed once the ticket is done.`, and clicking does nothing.

- [ ] **Step 4: Commit any formatting fixes**

```bash
git add -A
git commit -m "chore: gofmt after VS Code chip"
```

(Skip this step if `git status` is clean.)

---

## Self-Review

**Spec coverage**

| Spec requirement | Task |
| --- | --- |
| `vscode://file/<path>` URI, no endpoint, no spawn | 1, 3 |
| VS Code only, no editor config key | 1 (helper hardcodes the scheme) |
| Detail page only, rail unchanged | 3 |
| Disabled chip with tooltip when folder is gone | 2 (empty URI), 3 (`<span>` branch) |
| Never gated behind a control flag | 2 (`setWorktree` has no flag check) |
| Reuse `worktreePath` | 2 |
| Two `view` fields, filled in `load()` after backfills | 2 |
| One `os.Stat`, selected ticket only | 2 |
| `Ticket` / `scanLogs` untouched | 2 (no changes there) |
| `nil` Selected leaves both fields zero | 2 (`setWorktree` early return) |
| `worktreeURI` helper in `render.go`, `template.URL`, `url.URL` escaping | 1 |
| Markup after `pull request ↗`, `$`-scoped, existing classes only | 3 |
| Live flip on re-poll, no cache | 2 (second half of the load test) |
| Test: enabled href + no `ZgotmplZ` | 3 |
| Test: disabled chip text + title, no `vscode://` | 3 |
| Test: space → `%20`, `vscode://file/` prefix | 1 |
| No existing tests changed | 3 Step 6 |

No gaps.

**Placeholder scan:** none — every step carries the literal code or command to run.

**Type consistency:** `worktreeURI(string) template.URL` is defined in Task 1 and used unchanged in Tasks 2 and 3. `view.WorktreePath` (string) and `view.WorktreeURI` (`template.URL`) are defined in Task 2 and referenced as `$.WorktreePath` / `$.WorktreeURI` in Task 3. `setWorktree(*view)` is defined and called only in Task 2. `worktreePath(string, int) string` matches `worktree.go:26`.
