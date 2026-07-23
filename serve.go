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
type Server struct {
	runner Runner
	cfg    *Config
	gh     *GitHub
	tmpl   *template.Template

	ttl time.Duration
	now func() time.Time

	mu        sync.Mutex
	ghIssues  []Issue      // last good gh result
	ghReady   bool         // true once a gh fetch has succeeded
	fetchedAt time.Time    // when ghIssues was last refreshed
	prTried   map[int]bool // issues whose PR backfill was attempted (guarded by mu)
}

// NewServer parses the dashboard templates from the embedded FS once and
// returns a Server that renders from the given Runner and Config. It errors if
// a template fails to parse.
func NewServer(r Runner, cfg *Config) (*Server, error) {
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
	}
	tmpl, err := template.New("dashboard").Funcs(funcs).ParseFS(webFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{runner: r, cfg: cfg, gh: NewGitHub(r, cfg), tmpl: tmpl, ttl: defaultGHTTL, now: time.Now, prTried: map[int]bool{}}, nil
}

// Handler returns the dashboard's HTTP routes: GET / (full page), GET /rail
// (the rail poll fragment), GET /detail (the detail-pane poll fragment), and
// GET /static/ (the embedded JS/CSS assets).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /rail", s.handleRail)
	mux.HandleFunc("GET /detail", s.handleDetail)
	mux.Handle("GET /static/", staticHandler())
	return mux
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
	renderHTML(w, s.tmpl, "page", v)
}

// handleRail renders only the left-rail fragment for the poll refresh.
func (s *Server) handleRail(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.tmpl, "rail", v)
}

// handleDetail renders only the detail-pane fragment for the live poll refresh.
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.tmpl, "detail", v)
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
