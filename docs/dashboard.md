# Progress dashboard

The daemon always serves a live web dashboard from the same process, so one
command both picks up labeled issues and shows every issue the loop has touched,
its live state, and a full per-issue pipeline timeline. The listen address is the
`addr` config field (default `localhost:8080`):

```bash
./loope --config loope.json                    # dashboard on http://localhost:8080
```

Point it elsewhere by setting `"addr": "localhost:9000"` in the config.

The dashboard rebuilds the view from two sources: the `logs/issue-<N>/` artifacts
on disk and current issue label/title state from `gh` (TTL-cached for a few
seconds so labels added after startup appear without a restart). A master-detail
page lists tickets in the left rail (auto-refreshing every few seconds);
selecting one shows its steps with expandable prompt and output, per-step cost
and Claude session id, and totals. The worker side is the same poll loop as the
plain daemon, so it swaps labels, opens PRs, and writes under `logs/` exactly as
`./loope` does — both stop together on a signal. If the dashboard listener fails,
the worker keeps running; the error is only logged.

If `gh` is unreachable, the page still renders from local logs and shows a
"GitHub unreachable" banner. The server shuts down cleanly on Ctrl-C / SIGTERM.
Bind stays on `localhost` by default since the dashboard exposes prompt/output
content.

## Web assets

The dashboard's front end lives in `web/`: Go templates in `web/templates/`, and
htmx, the idiomorph morph extension, `app.js` and the compiled stylesheet in
`web/static/`. Everything there is embedded with `go:embed`, so the release
binary stays self-contained and `go build` remains the only build command —
there is no Node, npm, or asset pipeline in CI. Editing a template or script
needs a rebuild, which takes about a second.

The one exception is webfonts: the page links IBM Plex from Google Fonts, so an
offline or air-gapped host falls back to system fonts. Everything else — markup,
behavior, styling — is served from the binary.

Styling is Tailwind CSS v4, compiled ahead of time with the [standalone
CLI](https://tailwindcss.com/blog/standalone-cli) (a single binary — no npm) and
**committed** as `web/static/app.css`. The source is `web/tailwind.css`.
Regenerate after changing any Tailwind class:

```bash
tailwindcss -i web/tailwind.css -o web/static/app.css --minify
```

⚠️ This step is manual, and it matters for **both** class sources: the templates
*and* the Go helpers (`stripeClass`, `nodeClass`, `cardClass`, `divClass`,
`statusChip`) that build class strings in `render.go`. Skipping it ships a
half-styled dashboard. `TestAppCSSCoversBothClassSources` in `serve_test.go`
fails if the regeneration was forgotten.
