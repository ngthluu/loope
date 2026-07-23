package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
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

	mu         sync.Mutex
	ghIssues   []Issue      // last good gh result
	ghReady    bool         // true once a gh fetch has succeeded
	fetchedAt  time.Time    // when ghIssues was last refreshed
	prTried    map[int]bool // issues whose PR backfill was attempted (guarded by mu)
	titleTried map[int]bool // issues whose title backfill was attempted (guarded by mu)
}

// NewServer parses the dashboard templates from the embedded FS once and
// returns a Server that renders from the given Runner and Config. It errors if
// a template fails to parse.
func NewServer(r Runner, cfg *Config) (*Server, error) {
	tmpl, err := template.New("dashboard").Funcs(templateFuncs(cfg)).ParseFS(webFS, "web/templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{runner: r, cfg: cfg, gh: NewGitHub(r, cfg), tmpl: tmpl, ttl: defaultGHTTL, now: time.Now,
		prTried: map[int]bool{}, titleTried: map[int]bool{}}, nil
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
	tickets, err := scanLogs(s.cfg)
	if err != nil {
		log.Printf("serve: scan logs: %v", err)
	}
	// Titles already on disk, so persistTitles below can tell a freshly-fetched
	// title from one it has already mirrored.
	onDisk := map[int]string{}
	for i := range tickets {
		onDisk[tickets[i].Number] = tickets[i].Title
	}
	var ghErr error
	if issues, e := s.issues(ctx); e != nil {
		ghErr = e
	} else {
		tickets = overlayIssues(tickets, issues, s.cfg)
		s.persistTitles(tickets, onDisk)
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
	s.backfillTitle(ctx, v.Selected)
	return v
}

// issueLogDir is the log dir for one issue.
func (s *Server) issueLogDir(n int) string {
	return filepath.Join(s.cfg.WorkDir, "logs", fmt.Sprintf("issue-%d", n))
}

// persistTitles mirrors GitHub titles into the log dirs of tickets that came
// from the disk scan, so a later start finds them without GitHub. Only titles
// that actually changed are written, so the 3s poll doesn't rewrite the same
// file forever; label-only tickets (no log dir yet) are skipped — the loop
// records their title when it picks them up.
func (s *Server) persistTitles(tickets []Ticket, onDisk map[int]string) {
	for i := range tickets {
		tk := &tickets[i]
		prev, logged := onDisk[tk.Number]
		if !logged || tk.Title == "" || tk.Title == prev {
			continue
		}
		recordTitle(s.issueLogDir(tk.Number), tk.Title)
	}
}

// backfillTitle recovers the title of a ticket the issue-list query no longer
// returns — the issue's labels were edited after it finished, so it silently
// dropped out of the label-scoped search and the card is stuck on the
// "awaiting GitHub title" placeholder (issue #16).
//
// It mirrors backfillPR: only the selected ticket, at most once per issue per
// process for a settled answer (a transient outage is not memoized, so a later
// poll can retry), and a hit is written to the log dir so the rail — which
// renders from the disk scan — picks it up on the next poll without a gh call.
func (s *Server) backfillTitle(ctx context.Context, tk *Ticket) {
	if tk == nil || tk.Title != "" {
		return
	}
	s.mu.Lock()
	tried := s.titleTried[tk.Number]
	s.mu.Unlock()
	if tried {
		return
	}

	title, err := s.gh.IssueTitle(ctx, tk.Number)
	if err != nil {
		if isTransientGitHubError(err) {
			return
		}
		s.mu.Lock()
		s.titleTried[tk.Number] = true
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.titleTried[tk.Number] = true
	s.mu.Unlock()
	if title == "" {
		return
	}
	tk.Title = title
	recordTitle(s.issueLogDir(tk.Number), title)
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
	recordPR(s.issueLogDir(tk.Number), url)
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

// handleRail renders the left-rail poll fragment plus the out-of-band header
// statbar, which htmx relocates into the page header itself.
func (s *Server) handleRail(w http.ResponseWriter, r *http.Request) {
	v := s.load(r.Context(), r.URL.Query().Get("issue"))
	renderHTML(w, s.tmpl, "railpoll", v)
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
