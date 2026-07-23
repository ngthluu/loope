package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// defaultGHTTL is how long a successful `gh` issue-list result is reused before
// the next poll re-queries GitHub. It matches the rail's 3-second client poll,
// so a label added after startup surfaces within one refresh instead of only on
// a restart.
const defaultGHTTL = 3 * time.Second

// Server renders the read-only progress dashboard from logs + gh state.
//
// The two data sources are cached differently. Disk logs (steps, cost, session)
// change every poll, so they are re-scanned on every request. GitHub data
// (titles, state labels) is TTL-cached: a successful fetch is reused for
// defaultGHTTL, after which the next poll re-queries `gh` so newly labeled
// issues appear without a restart. Before the first success (GitHub briefly
// unreachable) every poll retries; after a success, a transient failure serves
// the last good list rather than flashing the "unreachable" banner.
// Controller is the mutating surface the dashboard exposes. Orchestrator
// implements it (via the adapter in control.go). A nil Controller — a dashboard
// with no daemon behind it — hides the buttons and makes the routes return 503.
type Controller interface {
	Stop(n int) error
	Continue(n int) error
}

type Server struct {
	runner Runner
	cfg    *Config
	ctl    Controller
	gh     *GitHub
	page   *template.Template
	rail   *template.Template
	detail *template.Template

	ttl time.Duration
	now func() time.Time

	mu        sync.Mutex
	ghIssues  []Issue      // last good gh result
	ghReady   bool         // true once a gh fetch has succeeded
	fetchedAt time.Time    // when ghIssues was last refreshed
	prTried   map[int]bool // issues whose PR backfill was attempted (guarded by mu)
}

// NewServer parses the dashboard templates once and returns a Server that
// renders from the given Runner and Config. ctl, when non-nil, enables the
// mutating stop/continue routes and their buttons; pass nil for a strictly
// read-only dashboard. It errors if a template fails to parse.
func NewServer(r Runner, cfg *Config, ctl Controller) (*Server, error) {
	funcs := template.FuncMap{
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
		"canAct":       func() bool { return ctl != nil },
	}
	page, err := template.New("page").Funcs(funcs).Parse(pageTmpl + railTmpl + detailTmpl + stepcardTmpl)
	if err != nil {
		return nil, err
	}
	rail, err := template.New("rail").Funcs(funcs).Parse(railTmpl)
	if err != nil {
		return nil, err
	}
	detail, err := template.New("detail").Funcs(funcs).Parse(detailTmpl + stepcardTmpl)
	if err != nil {
		return nil, err
	}
	return &Server{runner: r, cfg: cfg, ctl: ctl, gh: NewGitHub(r, cfg), page: page, rail: rail, detail: detail, ttl: defaultGHTTL, now: time.Now, prTried: map[int]bool{}}, nil
}

// Handler returns the dashboard's HTTP routes: GET / (full page), GET /rail and
// GET /detail (the poll fragments), and the mutating /stop and /continue, which
// accept POST only so a link or a crawler cannot trigger either, and only from
// the dashboard's own origin so another page the operator is browsing cannot
// either (see sameOrigin).
//
// The mutating routes are registered method-less and check the method in act:
// a method-scoped "POST /stop" would leave GET /stop to be swallowed by the
// GET / catch-all and render the dashboard instead of refusing.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// "GET /{$}" (exact) rather than "GET /" (subtree): the subtree form would
	// both swallow GET /stop instead of refusing it and conflict with the
	// method-less /stop registration below.
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /rail", s.handleRail)
	mux.HandleFunc("GET /detail", s.handleDetail)
	mux.HandleFunc("/stop", s.handleStop)
	mux.HandleFunc("/continue", s.handleContinue)
	return mux
}

// handleStop halts work on ?issue=<N>. Stop is fast, so it runs inline.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func(n int) error { return s.ctl.Stop(n) })
}

// handleContinue resumes a stopped ?issue=<N>. The controller validates
// synchronously and runs the multi-minute resume in the background, so this
// returns as soon as the transition is real.
func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func(n int) error { return s.ctl.Continue(n) })
}

// act is the shared shape of the mutating routes: 405 on anything but POST,
// 503 with no controller, 400
// on a bad issue number, 409 with the controller's plain-text reason on a
// refusal, 204 on success.
func (s *Server) act(w http.ResponseWriter, r *http.Request, fn func(int) error) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if s.ctl == nil {
		http.Error(w, "no daemon behind this dashboard", http.StatusServiceUnavailable)
		return
	}
	n, err := strconv.Atoi(r.URL.Query().Get("issue"))
	if err != nil || n <= 0 {
		http.Error(w, "issue must be a positive number", http.StatusBadRequest)
		return
	}
	if err := fn(n); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sameOrigin reports whether a mutating request came from the dashboard itself.
//
// POST alone does not protect these routes: a form post is a CORS-simple
// request, so any page the operator happens to be browsing can submit one at
// localhost:8080 and halt or resume a ticket, and the attacker never needs to
// read the (opaque) response. The dashboard has no login and therefore no
// session token to bind a CSRF token to, so the check is the header pair every
// current browser attaches and no cross-site page can forge: Sec-Fetch-Site,
// falling back to Origin against the request's own Host.
//
// A client that sends neither (curl, a script, the CLI) is allowed through:
// those are not the confused deputy this is defending against, and the routes
// are deliberately usable without a browser.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
		// No Fetch Metadata: fall through to the Origin check below.
	default:
		// "cross-site" or "same-site" — a different origin either way.
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// stats is the fleet-wide telemetry shown in the command bar: how many tickets
// the loop is tracking, how many have a step in flight, and the summed spend.
type stats struct {
	Tickets int
	Running int
	Spend   float64
}

// view is the template payload for one render.
type view struct {
	Tickets  []Ticket
	Selected *Ticket
	GHError  string
	Stats    stats
}

// issues returns the tracked-issue list from GitHub. A successful fetch is
// reused for the TTL; the first poll past it re-queries `gh` so labels added
// after startup show up. Before the first success every poll retries; after a
// success a transient failure serves the last good list so a blip doesn't flash
// the unreachable banner. Safe for concurrent use by multiple HTTP handlers.
func (s *Server) issues(ctx context.Context) ([]Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ghReady && s.now().Sub(s.fetchedAt) < s.ttl {
		return s.ghIssues, nil
	}
	issues, err := listTrackedIssues(ctx, s.runner, s.cfg.RepoPath, s.cfg.RepoSlug, s.cfg.EligibleLabel, trackedStateLabels(s.cfg))
	if err != nil {
		if s.ghReady {
			return s.ghIssues, nil
		}
		return nil, err
	}
	s.ghIssues, s.ghReady, s.fetchedAt = issues, true, s.now()
	return issues, nil
}

// load builds the render payload: it re-scans the disk logs on every request so
// steps and cost stay live, then overlays the once-fetched GitHub titles/labels.
func (s *Server) load(ctx context.Context, selWanted string) view {
	tickets, err := scanLogs(s.cfg.WorkDir)
	if err != nil {
		log.Printf("serve: scan logs: %v", err)
	}
	var ghErr error
	if issues, e := s.issues(ctx); e != nil {
		ghErr = e
	} else {
		tickets = overlayIssues(tickets, issues, s.cfg)
	}
	v := view{Tickets: tickets, Stats: summarize(tickets)}
	if ghErr != nil {
		v.GHError = ghErr.Error()
	}
	if len(tickets) == 0 {
		return v
	}
	v.Selected = &tickets[0]
	if selWanted != "" {
		if n, err := strconv.Atoi(selWanted); err == nil {
			for i := range tickets {
				if tickets[i].Number == n {
					v.Selected = &tickets[i]
					break
				}
			}
		}
	}
	s.backfillPR(ctx, v.Selected)
	return v
}

// backfillPR lazily fetches and caches the PR URL for a ticket shipped before
// ship-time persistence existed (Task 5). It runs only for the selected ticket
// — the PR link lives only in the detail header — and at most once per issue per
// process for a SETTLED answer: a hit or a permanent "no PR" is remembered in
// prTried so it isn't re-queried on every 3s poll, but a transient gh outage is
// not memoized so a later poll can still find the PR. On a hit it writes the
// issue's pr file so later scans read the URL without a gh call, and sets
// tk.PRURL for this render.
func (s *Server) backfillPR(ctx context.Context, tk *Ticket) {
	if tk == nil || tk.PRURL != "" {
		return
	}
	s.mu.Lock()
	tried := s.prTried[tk.Number]
	s.mu.Unlock()
	if tried {
		return
	}

	url, err := s.gh.PRURLForBranch(ctx, branchName(tk.Number))
	if err != nil {
		if isTransientGitHubError(err) {
			// A transient outage (internal retries already exhausted): do NOT
			// memoize, so a later poll retries rather than permanently suppressing
			// the PR link for the life of the process. `gh pr view` for a branch with
			// no PR fails non-transiently and still memoizes below.
			return
		}
		// Permanent answer — this branch genuinely has no PR. Remember it so the
		// lookup doesn't repeat on every poll.
		s.mu.Lock()
		s.prTried[tk.Number] = true
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.prTried[tk.Number] = true
	s.mu.Unlock()
	tk.PRURL = url
	recordPR(filepath.Join(s.cfg.WorkDir, "logs", fmt.Sprintf("issue-%d", tk.Number)), url)
}

// summarize rolls the ticket list up into the command-bar telemetry.
func summarize(tickets []Ticket) stats {
	st := stats{Tickets: len(tickets)}
	for i := range tickets {
		st.Spend += tickets[i].TotalCost
		if hasRunning(tickets[i]) {
			st.Running++
		}
	}
	return st
}

// handleIndex renders the full master-detail page for the selected ticket.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.page, "page", v)
}

// handleRail renders only the left-rail fragment for the poll refresh.
func (s *Server) handleRail(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.rail, "rail", v)
}

// handleDetail renders only the detail-pane fragment for the live poll refresh.
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.detail, "detail", v)
}

// renderHTML executes a template into a buffer before touching the
// ResponseWriter. Rendering straight to w wrote a 200 and a partial body, and if
// the template then errored — or the client had already disconnected — the
// follow-up http.Error tried to WriteHeader again, producing the "superfluous
// WriteHeader" warning atop the broken-pipe line. Buffering means a template
// error still yields a clean 500 (nothing written yet), and a write failure is
// just the client going away mid-poll: expected with the 3s refresh, so the
// disconnect errors are swallowed rather than logged as noise.
func renderHTML(w http.ResponseWriter, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("serve: render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil && !isClientDisconnect(err) {
		log.Printf("serve: write %s: %v", name, err)
	}
}

// isClientDisconnect reports whether a write failed because the client hung up
// (broken pipe / connection reset) — a mid-response poll cancellation, not a
// server fault, so it is not worth logging.
func isClientDisconnect(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// ── template helpers ────────────────────────────────────────────────────────

// hasRunning reports whether any of the ticket's steps is still in flight.
func hasRunning(t Ticket) bool {
	for _, s := range t.Steps {
		if s.Status == StatusRunning {
			return true
		}
	}
	return false
}

// errCount is how many of the ticket's steps ended in error.
func errCount(t Ticket) int {
	n := 0
	for _, s := range t.Steps {
		if s.Status == StatusError {
			n++
		}
	}
	return n
}

// money formats a per-step cost, using an em dash for zero/unknown (a running
// step has no settled cost yet).
func money(c float64) string {
	if c == 0 {
		return "—"
	}
	return "$" + strconv.FormatFloat(c, 'f', 2, 64)
}

// dollars formats an aggregate figure that should always read as money,
// including a genuine zero.
func dollars(c float64) string {
	return fmt.Sprintf("$%.2f", c)
}

// tokens humanizes a token count: exact under 1k, rounded "11k" under 1M, and
// two-decimal "1.46M" beyond.
func tokens(n int) string {
	switch {
	case n < 1000:
		if n < 0 {
			return "0"
		}
		return strconv.Itoa(n)
	case n < 1_000_000:
		return strconv.Itoa(int(math.Round(float64(n)/1000))) + "k"
	default:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 2, 64) + "M"
	}
}

// duration renders a millisecond span compactly: "45s", "3m26s", "1h02m". Zero
// or negative reads as an em dash.
func duration(ms int) string {
	if ms <= 0 {
		return "—"
	}
	s := ms / 1000
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	m := s / 60
	s %= 60
	if m < 60 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := m / 60
	m %= 60
	return fmt.Sprintf("%dh%02dm", h, m)
}

// ctxTokens is a step's prompt-side (context) token total: fresh input plus both
// cache tiers.
func ctxTokens(s Step) int {
	return s.InputTokens + s.CacheCreationTokens + s.CacheReadTokens
}

// hasUsage reports whether a step carries any token accounting worth showing.
func hasUsage(s Step) bool {
	return s.NumTurns > 0 || s.OutputTokens > 0 || ctxTokens(s) > 0
}

// txLine renders one transcript event as an inline HTML row: a glyph plus its
// text/tool detail. Text is escaped; only the fixed markup is trusted.
func txLine(e TranscriptEvent) template.HTML {
	esc := template.HTMLEscapeString
	switch e.Kind {
	case "text":
		return template.HTML(`<div class="text-text/85">` + esc(e.Text) + `</div>`)
	case "thinking":
		return template.HTML(`<div class="italic text-faint">` + esc(e.Text) + `</div>`)
	case "tool":
		d := ""
		if e.Detail != "" {
			d = ` <span class="text-faint">` + esc(e.Detail) + `</span>`
		}
		return template.HTML(`<div class="text-live">⚙ <span class="text-text/85">` + esc(e.Tool) + `</span>` + d + `</div>`)
	default: // tool_result
		if e.IsError {
			return template.HTML(`<div class="text-err/80">↳ error</div>`)
		}
		return template.HTML(`<div class="text-ok/80">↳ ok</div>`)
	}
}

// short returns the first n runes of s (for a compact session id column).
func short(n int, s string) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// shortid abbreviates a full session id as head…tail so it fits one cell
// without losing the recognizable ends.
func shortid(s string) string {
	r := []rune(s)
	if len(r) <= 16 {
		return s
	}
	return string(r[:8]) + "…" + string(r[len(r)-6:])
}

// stateKind maps a GitHub state label to a semantic bucket the templates key
// their color and copy on, so the palette stays correct even when a project
// renames its labels.
func stateKind(cfg *Config, label string) string {
	switch label {
	case cfg.StateLabels.Done:
		return "done"
	case cfg.StateLabels.WIP:
		return "wip"
	case cfg.StateLabels.Rework:
		return "rework"
	case cfg.StateLabels.Stopped:
		return "stopped"
	case cfg.StateLabels.Failed:
		return "failed"
	case cfg.EligibleLabel:
		return "queued"
	default:
		return ""
	}
}

// stripeClass is the rail row's left accent-bar color for a state.
func stripeClass(cfg *Config, label string) string {
	switch stateKind(cfg, label) {
	case "done":
		return "bg-ok/50"
	case "wip":
		return "bg-live"
	case "rework":
		return "bg-warn/80"
	case "failed":
		return "bg-err/70"
	case "stopped":
		return "bg-muted/40"
	default:
		return "bg-line2"
	}
}

// nodeClass is the pipeline-spine node's fill + glow for a step status.
func nodeClass(st StepStatus) string {
	switch st {
	case StatusOK:
		return "bg-ok node-ok"
	case StatusError:
		return "bg-err node-err"
	case StatusRunning:
		return "bg-live node-live"
	default:
		return "bg-faint"
	}
}

// cardClass is the step card's surface + ring for a status: quiet for settled
// steps, tinted and outlined for the error and in-flight states.
func cardClass(st StepStatus) string {
	switch st {
	case StatusError:
		return "bg-err/[0.055] ring-1 ring-err/35"
	case StatusRunning:
		return "bg-live/[0.045] ring-1 ring-live/40"
	default:
		return "bg-panel ring-1 ring-line"
	}
}

// divClass is the divider color between a step's header and its disclosures.
func divClass(st StepStatus) string {
	switch st {
	case StatusError:
		return "border-err/25"
	case StatusRunning:
		return "border-live/25"
	default:
		return "border-line/60"
	}
}

// statusChip renders the inline status marker (icon + word) for a step.
func statusChip(st StepStatus) template.HTML {
	switch st {
	case StatusOK:
		return `<span class="inline-flex items-center gap-1 font-mono text-[10px] font-semibold uppercase tracking-wide text-ok"><svg class="h-3 w-3" viewBox="0 0 12 12" fill="none"><path d="M2.5 6.2l2.2 2.2 4.8-5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>ok</span>`
	case StatusError:
		return `<span class="inline-flex items-center gap-1 font-mono text-[10px] font-semibold uppercase tracking-wide text-err"><svg class="h-3 w-3" viewBox="0 0 12 12" fill="none"><path d="M6 2v4.5M6 9v.01" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>error</span>`
	case StatusRunning:
		return `<span class="inline-flex items-center gap-1.5 font-mono text-[10px] font-semibold uppercase tracking-wide text-live"><span class="hb inline-block h-1.5 w-1.5 rounded-full bg-live"></span>running</span>`
	default:
		return `<span class="font-mono text-[10px] font-semibold uppercase tracking-wide text-faint">unparsed</span>`
	}
}

// ── templates ───────────────────────────────────────────────────────────────

// pageTmpl is split around uiJS so the client script stays a plain .go string
// with no template actions in it (see ui.go).
const pageTmpl = pageHead + uiJS + pageTail

const pageHead = `{{define "page"}}<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>loop // telemetry</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&family=IBM+Plex+Sans:wght@400;500;600;700&display=swap" rel="stylesheet">
<script src="https://cdn.tailwindcss.com"></script>
<script>
tailwind.config={theme:{extend:{
 colors:{ink:'#F3F5F8',panel:'#FFFFFF',panel2:'#EAEEF3',line:'#E4E8ED',line2:'#D2D9E1',text:'#16202B',muted:'#55636F',faint:'#6E7A87',ok:'#0B7D43',err:'#C42B1C',warn:'#B45309',live:'#0A7E95'},
 fontFamily:{sans:['"IBM Plex Sans"','system-ui','sans-serif'],mono:['"IBM Plex Mono"','ui-monospace','monospace']}}}}
</script>
<style>
 :root{color-scheme:light} body{background:#F3F5F8}
 @keyframes hb{0%,100%{opacity:.35;transform:scale(.8)}50%{opacity:1;transform:scale(1)}}
 @keyframes ring{0%{box-shadow:0 0 0 0 rgba(10,126,149,.45)}70%{box-shadow:0 0 0 8px rgba(10,126,149,0)}100%{box-shadow:0 0 0 0 rgba(10,126,149,0)}}
 @keyframes fadein{from{opacity:0;transform:translateY(3px)}to{opacity:1;transform:none}}
 .hb{animation:hb 1.6s ease-in-out infinite}.ring{animation:ring 1.8s ease-out infinite}.fadein{animation:fadein .35s ease both}
 .node-ok{box-shadow:0 0 0 3px rgba(11,125,67,.16)}.node-err{box-shadow:0 0 0 3px rgba(196,43,28,.16)}.node-live{box-shadow:0 0 0 3px rgba(10,126,149,.2)}
 details>summary{list-style:none}details>summary::-webkit-details-marker{display:none}details[open] .chev{transform:rotate(90deg)}
 .scroll::-webkit-scrollbar{width:10px;height:10px}.scroll::-webkit-scrollbar-thumb{background:#D2D9E1;border-radius:6px;border:2px solid #F3F5F8}.scroll::-webkit-scrollbar-track{background:transparent}
 @media (prefers-reduced-motion:reduce){.hb,.ring,.fadein{animation:none!important}}
</style></head>
<body class="font-sans text-text antialiased">
<div class="flex h-screen flex-col">
 <header class="flex shrink-0 items-center justify-between gap-4 border-b border-line bg-panel px-5 py-3">
  <div class="flex items-center gap-3 min-w-0">
   <div class="flex items-center gap-2">
    <span class="relative flex h-2.5 w-2.5"><span class="ring absolute inline-flex h-2.5 w-2.5 rounded-full"></span><span class="relative inline-flex h-2.5 w-2.5 rounded-full bg-live"></span></span>
    <span class="font-mono text-[15px] font-semibold tracking-tight text-text">loop<span class="text-live">·</span>telemetry</span>
   </div>
   <span class="hidden truncate font-mono text-xs text-faint sm:inline">reading {{.Stats.Tickets}} tracked issues</span>
  </div>
  <div class="flex items-center gap-4 font-mono text-xs sm:gap-5">
   <div class="hidden items-baseline gap-1.5 md:flex"><span id="stat-tickets" class="text-base font-semibold tabular-nums text-text">{{.Stats.Tickets}}</span><span class="text-faint">tickets</span></div>
   <div class="hidden h-4 w-px bg-line2 md:block"></div>
   <div class="hidden items-baseline gap-1.5 md:flex"><span class="flex items-center gap-1.5 text-base font-semibold tabular-nums text-live"><span class="hb inline-block h-1.5 w-1.5 rounded-full bg-live"></span><span id="stat-running">{{.Stats.Running}}</span></span><span class="text-faint">running</span></div>
   <div class="hidden h-4 w-px bg-line2 md:block"></div>
   <div class="flex items-baseline gap-1.5"><span id="stat-spend" class="text-base font-semibold tabular-nums text-text">{{dollars .Stats.Spend}}</span><span class="text-faint">spend</span></div>
   <div class="h-4 w-px bg-line2"></div>
   <div class="flex items-center gap-1.5 text-faint"><span class="hb inline-block h-1.5 w-1.5 rounded-full bg-live"></span><span>live · <span id="ago" class="tabular-nums text-muted">0s</span> ago</span></div>
  </div>
 </header>
 <div class="flex min-h-0 flex-1">
  <nav id="rail" class="scroll w-[320px] shrink-0 overflow-y-auto border-r border-line bg-panel">{{template "rail" .}}</nav>
  <main id="main" class="scroll min-w-0 flex-1 overflow-y-auto">{{template "detail" .}}</main>
 </div>
</div>
<script>`

const pageTail = `</script>
</body></html>{{end}}`

const railTmpl = `{{define "rail"}}
 <div id="railmeta" hidden data-tickets="{{.Stats.Tickets}}" data-running="{{.Stats.Running}}" data-spend="{{dollars .Stats.Spend}}"></div>
 <div class="sticky top-0 z-10 flex items-center justify-between border-b border-line bg-panel px-4 py-2.5">
  <span class="font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-muted">queue · {{.Stats.Tickets}}</span>
  <span class="font-mono text-[10px] uppercase tracking-[0.14em] text-faint">cost</span>
 </div>
 {{if .GHError}}<div class="border-b border-warn/25 bg-warn/[0.07] px-4 py-2 font-mono text-[11px] leading-snug text-warn/90">GitHub unreachable — showing local logs only.</div>{{end}}
 {{range .Tickets}}
  {{$sel := and $.Selected (eq .Number $.Selected.Number)}}
  {{$k := stateKind .StateLabel}}
  <a href="/?issue={{.Number}}" data-k="t{{.Number}}" class="group relative block border-b border-line/60 pl-4 pr-3.5 py-3 {{if $sel}}bg-panel2{{else}}hover:bg-panel2/50{{end}}">
   <span class="absolute inset-y-0 left-0 w-[3px] {{stripeClass .StateLabel}}"></span>
   <div class="flex items-start justify-between gap-3">
    <div class="min-w-0">
     <div class="flex items-center gap-2">
      <span class="font-mono text-[11px] font-semibold {{if $sel}}text-live{{else}}text-muted{{end}}">#{{.Number}}</span>
      {{if $k}}<span class="inline-flex items-center gap-1 rounded-sm border px-1.5 py-px font-mono text-[9px] font-semibold uppercase tracking-wide {{if eq $k "done"}}border-ok/25 bg-ok/[0.13] text-ok{{else if eq $k "wip"}}border-live/30 bg-live/10 text-live{{else if eq $k "rework"}}border-warn/30 bg-warn/10 text-warn{{else if eq $k "failed"}}border-err/30 bg-err/10 text-err{{else if eq $k "stopped"}}border-line2 bg-panel2 text-muted{{else}}border-line2 bg-panel2 text-muted{{end}}">{{if eq $k "wip"}}<span class="hb inline-block h-1 w-1 rounded-full bg-live"></span>{{end}}{{$k}}</span>{{end}}
     </div>
     <div class="mt-1.5 line-clamp-2 min-h-[34px] text-[13px] font-medium leading-[17px] text-text/90">{{if .Title}}{{.Title}}{{else}}#{{.Number}} · awaiting GitHub title{{end}}</div>
     <div class="mt-1.5 font-mono text-[10px] uppercase tracking-wide text-faint">{{if .Kind}}{{.Kind}} · {{end}}{{len .Steps}} step{{if ne (len .Steps) 1}}s{{end}}</div>
    </div>
    <span class="shrink-0 font-mono text-[13px] tabular-nums {{if eq $k "done"}}font-semibold text-ok{{else if $sel}}font-semibold text-text{{else}}text-muted{{end}}">{{money .TotalCost}}</span>
   </div>
  </a>
 {{else}}<div class="px-4 py-8 text-center font-mono text-[11px] leading-relaxed text-faint">No tickets in flight.<br>The loop hasn't picked up any labeled issues yet.</div>{{end}}
{{end}}`

const detailTmpl = `{{define "detail"}}<div class="max-w-[1160px] px-10 py-7">
 {{if .GHError}}<div class="mb-5 flex items-start gap-2 rounded-md border border-warn/30 bg-warn/[0.06] px-4 py-3 font-mono text-[12px] leading-relaxed text-warn/90"><span class="mt-px">⚠</span><span>GitHub unreachable — showing local logs only. Titles and states may be missing.<br><span class="text-warn/60">{{.GHError}}</span></span></div>{{end}}
 {{with .Selected}}
  {{$k := stateKind .StateLabel}}
  <div class="mb-6">
   <div class="mb-2.5 flex flex-wrap items-center gap-2">
    <span class="font-mono text-sm font-semibold text-live">#{{.Number}}</span>
    {{if $k}}<span class="inline-flex items-center gap-1.5 rounded border px-2 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-widest {{if eq $k "done"}}border-ok/30 bg-ok/10 text-ok{{else if eq $k "wip"}}border-live/30 bg-live/10 text-live{{else if eq $k "rework"}}border-warn/30 bg-warn/10 text-warn{{else if eq $k "failed"}}border-err/30 bg-err/10 text-err{{else if eq $k "stopped"}}border-line2 bg-panel2 text-muted{{else}}border-line2 bg-panel2 text-muted{{end}}">{{if eq $k "wip"}}<span class="hb inline-block h-1.5 w-1.5 rounded-full bg-live"></span>in progress{{else if eq $k "done"}}done{{else if eq $k "stopped"}}stopped{{else}}{{$k}}{{end}}</span>{{end}}
    {{if .Kind}}<span class="rounded border border-line2 bg-panel2 px-2 py-0.5 font-mono text-[10px] font-semibold uppercase tracking-widest text-muted">{{.Kind}}</span>{{end}}
   </div>
   <h1 class="text-[26px] font-semibold leading-tight tracking-tight text-text">{{if .Title}}{{.Title}}{{else}}Issue #{{.Number}}{{end}}</h1>
   <div class="mt-3 flex flex-wrap items-center gap-2 font-mono text-[11px]">
    <a href="{{issueURL .Number}}" target="_blank" rel="noopener" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/40">issue ↗</a>
    {{if .PRURL}}<a href="{{.PRURL}}" target="_blank" rel="noopener" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/40">pull request ↗</a>{{end}}
    {{if canAct}}
     {{/* data-k keys the buttons for morph(): stop and continue are distinct
          nodes, so swapping one for the other inserts a fresh element instead
          of re-labelling the old one, which would inherit its disabled state. */}}
     {{if or (eq $k "wip") (eq $k "rework") (eq $k "queued")}}<button type="button" data-k="act-stop" data-act="stop" data-issue="{{.Number}}" onclick="act(this)" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-warn/50">stop</button>{{end}}
     {{if eq $k "stopped"}}<button type="button" data-k="act-continue" data-act="continue" data-issue="{{.Number}}" onclick="act(this)" class="inline-flex items-center gap-1 rounded border border-line2 bg-panel px-2 py-0.5 text-muted hover:text-text hover:border-live/50">continue</button>{{end}}
    {{end}}
   </div>
   <div id="acterr" class="mt-2 hidden font-mono text-[11px] text-err"></div>
   <dl class="mt-5 grid grid-cols-2 gap-px overflow-hidden rounded-md border border-line bg-line sm:grid-cols-5">
    <div class="bg-panel px-4 py-3"><dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">total spend</dt><dd class="mt-1.5 font-mono text-xl font-semibold tabular-nums {{if eq $k "done"}}text-ok{{else}}text-text{{end}}">{{dollars .TotalCost}}</dd></div>
    <div class="bg-panel px-4 py-3"><dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">tokens</dt><dd class="mt-1.5 font-mono text-sm font-semibold tabular-nums text-text" title="context in · output out">&darr;{{tokens .TotalInputTokens}} &middot; &uarr;{{tokens .TotalOutputTokens}}</dd></div>
    <div class="bg-panel px-4 py-3"><dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">steps</dt><dd class="mt-1.5 font-mono text-xl font-semibold tabular-nums text-text">{{len .Steps}}</dd></div>
    <div class="bg-panel px-4 py-3"><dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">errors</dt><dd class="mt-1.5 font-mono text-xl font-semibold tabular-nums {{if errCount .}}text-err{{else}}text-faint{{end}}">{{errCount .}}</dd></div>
    <div class="bg-panel px-4 py-3"><dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">session</dt>
     <dd class="mt-1.5 flex items-center gap-1.5">
      <span class="truncate font-mono text-[13px] text-muted" title="{{.SessionID}}">{{if .SessionID}}{{shortid .SessionID}}{{else}}—{{end}}</span>
      {{if .SessionID}}<button type="button" data-sid="{{.SessionID}}" onclick="copySid(this)" class="copy shrink-0 rounded border border-line2 bg-panel px-1.5 py-0.5 font-mono text-[10px] text-faint hover:text-text hover:border-live/40" title="copy full session id">copy</button>{{end}}
     </dd>
    </div>
   </dl>
  </div>

  <div class="sticky top-0 z-10 -mx-10 mb-2 flex items-center gap-3 border-y border-line bg-ink/95 px-10 py-2 backdrop-blur">
   <span class="font-mono text-[10px] font-semibold uppercase tracking-[0.18em] text-muted">pipeline</span>
   <span class="font-mono text-[10px] uppercase tracking-wide text-faint">{{len .Steps}} step{{if ne (len .Steps) 1}}s{{end}}{{if errCount .}} · {{errCount .}} error{{if ne (errCount .) 1}}s{{end}}{{end}}</span>
   <span class="ml-auto font-mono text-[10px] uppercase tracking-[0.14em] text-faint">cost</span>
  </div>

  {{if .Steps}}
   {{if hasAnswerer .Steps}}
   <div data-layout="two-col" class="pt-2">
    <div class="mb-2 grid grid-cols-2 gap-x-5 px-1 font-mono text-[10px] font-semibold uppercase tracking-[0.16em] text-faint">
     <span>architect</span><span>answerer</span>
    </div>
    <div class="grid grid-cols-2 gap-x-5 gap-y-2.5">
     {{range pipelineRows .Steps}}
      <div class="relative pl-7">{{if .HasLeft}}<span class="{{nodeClass .Left.Status}} absolute left-[4px] top-[14px] h-3 w-3 rounded-full border-2 border-ink" aria-hidden="true"></span>{{template "stepcard" .Left}}{{end}}</div>
      <div class="relative pl-7">{{if .HasRight}}<span class="{{nodeClass .Right.Status}} absolute left-[4px] top-[14px] h-3 w-3 rounded-full border-2 border-ink" aria-hidden="true"></span>{{template "stepcard" .Right}}{{end}}</div>
     {{end}}
    </div>
   </div>
   {{else}}
   <ol class="relative pt-2">
    <span class="absolute left-[12px] top-2 bottom-4 w-px bg-line2" aria-hidden="true"></span>
    {{range .Steps}}
    <li data-k="s{{.Seq}}" class="relative fadein pl-9 pb-2.5">
     <span class="{{nodeClass .Status}} absolute left-[6px] top-[14px] h-3.5 w-3.5 rounded-full border-2 border-ink" aria-hidden="true"></span>
     {{if eq .Status "running"}}<span class="ring absolute left-[6px] top-[14px] h-3.5 w-3.5 rounded-full" aria-hidden="true"></span>{{end}}
     {{template "stepcard" .}}
    </li>
    {{end}}
   </ol>
   {{end}}
  {{else}}<p class="pt-6 font-mono text-[12px] text-faint">No steps recorded yet — waiting for the first Claude call.</p>{{end}}
 {{else}}<div class="flex h-full items-center justify-center py-20 font-mono text-[12px] text-faint">Select a ticket from the queue.</div>{{end}}
</div>{{end}}`

const stepcardTmpl = `{{define "stepcard"}}<div class="fadein overflow-hidden rounded-md {{cardClass .Status}}">
 <div class="flex items-center gap-3 px-4 py-2.5">
  <span class="w-7 shrink-0 font-mono text-[11px] tabular-nums text-faint">{{printf "%03d" .Seq}}</span>
  <span class="truncate text-[13px] font-medium text-text">{{.Label}}</span>
  {{statusChip .Status}}
  <div class="ml-auto flex items-center gap-4">
   {{if hasUsage .}}<span class="hidden font-mono text-[11px] text-faint lg:inline" title="context in · output out · turns">&darr;{{tokens (ctxTokens .)}} &uarr;{{tokens .OutputTokens}} · {{.NumTurns}}t</span>{{end}}
   {{if .SessionID}}<span class="hidden font-mono text-[11px] {{if eq .Status "error"}}text-err/60{{else}}text-faint{{end}} md:inline">{{short 8 .SessionID}}</span>{{end}}
   <span class="w-14 text-right font-mono text-[13px] tabular-nums {{if eq .Status "error"}}text-err{{else}}text-muted{{end}}">{{money .Cost}}</span>
  </div>
 </div>
 {{if or .Prompt .Output (eq .Status "running") (hasUsage .) .Transcript}}
 <div class="border-t {{divClass .Status}} px-4">
  {{if .Transcript}}<details data-disc="{{.Seq}}-transcript" class="group"{{if eq .Status "running"}} open{{end}}>
   <summary class="flex cursor-pointer items-center gap-2 py-1.5 text-muted hover:text-text"><svg class="chev h-3 w-3 shrink-0 text-faint transition-transform" viewBox="0 0 12 12" fill="none"><path d="M4.5 3l3 3-3 3" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg><span class="font-mono text-[10px] uppercase tracking-wider">transcript</span></summary>
   <div class="txfeed scroll mb-2.5 max-h-72 space-y-1 overflow-auto rounded border border-line bg-ink px-3.5 py-3 font-mono text-[12px] leading-relaxed" data-seq="{{.Seq}}">
    {{range .Transcript}}{{txLine .}}{{end}}
   </div>
  </details>{{end}}
  {{if .Prompt}}<details data-disc="{{.Seq}}-prompt" class="group"{{if eq .Status "running"}} open{{end}}>
   <summary class="flex cursor-pointer items-center gap-2 py-1.5 text-muted hover:text-text"><svg class="chev h-3 w-3 shrink-0 text-faint transition-transform" viewBox="0 0 12 12" fill="none"><path d="M4.5 3l3 3-3 3" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg><span class="font-mono text-[10px] uppercase tracking-wider">prompt</span></summary>
   <pre class="scroll mb-2.5 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded border border-line bg-ink px-3.5 py-3 font-mono text-[12px] leading-relaxed text-muted">{{.Prompt}}</pre>
  </details>{{end}}
  {{if .Output}}<details data-disc="{{.Seq}}-output" class="group {{if .Prompt}}border-t border-line/50{{end}}"{{if eq .Status "error"}} open{{end}}>
   <summary class="flex cursor-pointer items-center gap-2 py-1.5 text-muted hover:text-text"><svg class="chev h-3 w-3 shrink-0 text-faint transition-transform" viewBox="0 0 12 12" fill="none"><path d="M4.5 3l3 3-3 3" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg><span class="font-mono text-[10px] uppercase tracking-wider">output</span></summary>
   <pre class="scroll mb-2.5 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded border {{if eq .Status "error"}}border-err/25 text-err/80{{else}}border-line text-muted{{end}} bg-ink px-3.5 py-3 font-mono text-[12px] leading-relaxed">{{.Output}}</pre>
  </details>{{end}}
  {{if hasUsage .}}<details data-disc="{{.Seq}}-usage" class="group {{if or .Prompt .Output}}border-t border-line/50{{end}}">
   <summary class="flex cursor-pointer items-center gap-2 py-1.5 text-muted hover:text-text"><svg class="chev h-3 w-3 shrink-0 text-faint transition-transform" viewBox="0 0 12 12" fill="none"><path d="M4.5 3l3 3-3 3" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round"/></svg><span class="font-mono text-[10px] uppercase tracking-wider">usage</span></summary>
   <dl class="mb-2.5 grid grid-cols-2 gap-x-6 gap-y-1 rounded border border-line bg-ink px-3.5 py-3 font-mono text-[11px] text-muted sm:grid-cols-3">
    <div class="flex justify-between gap-3"><dt>input</dt><dd class="tabular-nums text-text">{{tokens .InputTokens}}</dd></div>
    <div class="flex justify-between gap-3"><dt>cache-create</dt><dd class="tabular-nums text-text">{{tokens .CacheCreationTokens}}</dd></div>
    <div class="flex justify-between gap-3"><dt>cache-read</dt><dd class="tabular-nums text-text">{{tokens .CacheReadTokens}}</dd></div>
    <div class="flex justify-between gap-3"><dt>output</dt><dd class="tabular-nums text-text">{{tokens .OutputTokens}}</dd></div>
    <div class="flex justify-between gap-3"><dt>turns</dt><dd class="tabular-nums text-text">{{.NumTurns}}</dd></div>
    <div class="flex justify-between gap-3"><dt>duration</dt><dd class="tabular-nums text-text">{{duration .DurationMS}}</dd></div>
   </dl>
  </details>{{end}}
  {{if eq .Status "running"}}<div class="flex items-center gap-2 {{if or .Prompt .Output (hasUsage .)}}border-t border-live/20{{end}} py-2.5 font-mono text-[11px] text-faint"><span class="hb inline-block h-1 w-1 rounded-full bg-live"></span>waiting for Claude to finish this step…</div>{{end}}
 </div>{{end}}
</div>{{end}}`
