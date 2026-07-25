# Open the worktree in VS Code (#37)

## Problem

The detail page links out to the GitHub issue and PR, but there is no way to get
from a ticket to the code the agent actually wrote. The worktree lives at a
deterministic path on the daemon host, and the person reading the dashboard is
usually sitting at that host — they currently have to copy the path by hand.

## Scope

Add one chip to the detail page's `issue ↗ / pull request ↗` row that opens the
ticket's worktree folder in VS Code.

Decided by the issue author:

- The button emits a `vscode://file/<path>` URI the browser hands to the OS. No
  new endpoint, no process spawned by the daemon.
- VS Code only. No Cursor, no `editor` config key.
- Detail page only. The rail rows are unchanged.
- When the worktree directory is gone (removed once a ticket merges), the chip
  is still rendered but disabled, with a hover tooltip explaining why.
- Never gated behind a control flag — the only reason it is ever disabled is a
  missing folder.

Out of scope: opening a specific file, editor choice, revealing the folder in
Finder, and any server-side execution.

## Design

### Path

`worktreePath(cfg.WorkDir, issueNum)` in `worktree.go:26` is already the single
definition of where a ticket's worktree lives (`<workDir>/issue-<N>`), shared by
`Worktree.Create` and the rework command. The server reuses that function so a
future layout change moves the link too.

### Plumbing

Two fields are added to `view` (`serve.go:91`):

```go
type view struct {
    // …
    WorktreePath string        // always set for the selected ticket
    WorktreeURI  template.URL  // set only when the directory exists
}
```

They are filled in `load()` immediately after `v.Selected` is chosen (next to
the existing `backfillPR` / `backfillTitle` calls), so exactly one `os.Stat` runs
per render and only for the ticket on screen. `Ticket` and `scanLogs` are
untouched: this is a property of the selected view, not of the log scan, and the
rail fragment has no use for it.

When `v.Selected` is nil (empty queue) both fields stay zero; the detail template
never reaches the row in that case.

### Building the URI

A small helper in `render.go`, beside the other presentation pure functions:

```go
// worktreeURI is the vscode://file/<path> URI that asks the OS to open a
// worktree folder in VS Code. It returns template.URL because html/template
// rewrites href values whose scheme is not http/https/mailto to "#ZgotmplZ".
func worktreeURI(path string) template.URL {
    u := url.URL{Scheme: "vscode", Host: "file", Path: path}
    return template.URL(u.String())
}
```

Two details this pins down:

- **The `template.URL` type is required, not stylistic.** `html/template`'s URL
  filter permits only `http`, `https`, `mailto`, and relative URLs; a plain
  string `vscode://…` renders as `#ZgotmplZ` and the chip silently does nothing.
- **`url.URL` does the escaping.** `workDir` may contain spaces (macOS paths
  frequently do); `u.String()` percent-encodes the path correctly, which manual
  concatenation would not.

`Host: "file"` plus an absolute `Path` produces `vscode://file/Users/you/…`,
which is the form VS Code documents for opening a file *or folder*. `WorkDir` is
already absolute (`config.go:132`).

### Markup

In `web/templates/detail.html`, after the `pull request ↗` link (line 17), inside
the same `flex flex-wrap` row. The template is inside `{{with .Selected}}`, so
the view fields are reached through `$`:

```
{{if $.WorktreeURI}}
  <a href="{{$.WorktreeURI}}" title="Open {{$.WorktreePath}} in VS Code"
     class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/40">open in VS Code ↗</a>
{{else}}
  <span title="No worktree folder at {{$.WorktreePath}} — it is removed once the ticket is done."
        class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-faint">open in VS Code ↗</span>
{{end}}
```

The disabled state is a `<span>`, not a disabled `<a>`: an anchor without `href`
is already unclickable and unfocusable, so no ARIA or pointer-event handling is
needed, and the `title` attribute gives the required hover tooltip.

Every utility class used here (`text-faint`, `border-line2`, `bg-panel`,
`inline-flex`, `gap-1`, …) already appears in `web/static/app.css`, so the
checked-in Tailwind build stays valid. If a class outside that set is introduced
during implementation, `web/static/app.css` must be regenerated with
`tailwindcss -i web/tailwind.css -o web/static/app.css --minify`, which
`TestAppCSSCoversBothClassSources` guards.

### Behaviour when the folder disappears mid-session

The detail pane re-polls every few seconds, so the chip flips from enabled to
disabled on its own once the pipeline removes the worktree. No cache, no
invalidation.

### Failure modes

- **VS Code not installed / scheme unregistered** — the browser or OS ignores the
  click, or shows its own "no application" prompt. Not detectable from the page
  and deliberately not worked around.
- **Remote browser, local daemon** — the URI resolves against the *browser's*
  machine, where the path likely does not exist. Accepted: the dashboard is a
  localhost tool, and the alternative (server-side exec) was explicitly rejected
  in the issue.
- **Directory exists but is not a worktree** — treated as present; VS Code opens
  whatever is there. Stat-only is enough.

## Testing

New tests in `serve_test.go`, following the existing `newTestServer` /
`TestDetailShowsGitHubLinksAndSession` pattern:

1. **Enabled** — with `<workDir>/issue-<N>` created, `GET /detail?issue=N`
   contains `href="vscode://file/` + the worktree path, and does **not** contain
   `ZgotmplZ` (the regression guard for the `template.URL` requirement).
2. **Disabled** — with the directory absent, the fragment contains the chip text
   `open in VS Code` and a `title=` mentioning the path, but no `vscode://` href.
3. **Escaping** — a unit test on `worktreeURI` in `render.go`'s test coverage:
   a path containing a space yields `%20`, and the scheme/host prefix is exactly
   `vscode://file/`.

No changes to existing tests are expected; the added row does not alter any
asserted markup.

## Files touched

- `serve.go` — two `view` fields, populated in `load()`.
- `render.go` — `worktreeURI` helper.
- `web/templates/detail.html` — the chip, enabled and disabled variants.
- `serve_test.go` — the three tests above.
