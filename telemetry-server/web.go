package main

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
	"sort"
	"time"
)

//go:embed templates
var telemetryFS embed.FS

// telemetryWorkerView is one worker's pre-formatted render data.
type telemetryWorkerView struct {
	MachineID     string
	Hostname      string
	WorkDir       string
	Version       string
	Online        bool
	LastPushAt    time.Time
	UsageUnknown  bool
	FiveHourPct   float64
	FiveHourReset time.Time
	SevenDayPct   float64
	SevenDayReset time.Time
	Logs          []string
}

// telemetryGroup is one repoSlug's workers, sorted by hostname.
type telemetryGroup struct {
	RepoSlug string
	Workers  []telemetryWorkerView
}

// telemetryView is the template payload for one render.
type telemetryView struct {
	Groups   []telemetryGroup
	Selected *telemetryWorkerView
}

// buildTelemetryWorkerView converts one worker's server-side state into the
// template's flat, pre-formatted shape as of now.
func buildTelemetryWorkerView(ws *WorkerState, now time.Time) telemetryWorkerView {
	v := telemetryWorkerView{
		MachineID:  ws.Resource.MachineID,
		Hostname:   ws.Resource.Hostname,
		WorkDir:    ws.Resource.WorkDir,
		Version:    ws.Resource.Version,
		Online:     ws.online(now),
		LastPushAt: ws.LastPushAt,
		Logs:       ws.Logs.Lines(),
	}
	if u := ws.usableUsage(now); u != nil {
		v.FiveHourPct, v.FiveHourReset = u.FiveHourUsedPct, u.FiveHourResetAt
		v.SevenDayPct, v.SevenDayReset = u.SevenDayUsedPct, u.SevenDayResetAt
	} else {
		v.UsageUnknown = true
	}
	return v
}

// buildTelemetryView groups all workers by RepoSlug (sorted), sorts each
// group's workers by hostname, and selects the worker named by selectedID —
// or the first worker of the first group when selectedID is empty or not
// found, or nil when there are no workers at all.
func (s *TelemetryServer) buildTelemetryView(selectedID string) telemetryView {
	now := s.now()
	s.mu.Lock()
	byRepo := map[string][]telemetryWorkerView{}
	for _, ws := range s.workers {
		byRepo[ws.Resource.RepoSlug] = append(byRepo[ws.Resource.RepoSlug], buildTelemetryWorkerView(ws, now))
	}
	s.mu.Unlock()

	var slugs []string
	for slug := range byRepo {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	view := telemetryView{}
	for _, slug := range slugs {
		workers := byRepo[slug]
		sort.Slice(workers, func(i, j int) bool { return workers[i].Hostname < workers[j].Hostname })
		view.Groups = append(view.Groups, telemetryGroup{RepoSlug: slug, Workers: workers})
		for i := range workers {
			if workers[i].MachineID == selectedID {
				view.Selected = &view.Groups[len(view.Groups)-1].Workers[i]
			}
		}
	}
	if view.Selected == nil && len(view.Groups) > 0 && len(view.Groups[0].Workers) > 0 {
		view.Selected = &view.Groups[0].Workers[0]
	}
	return view
}

// telemetryIndexView is the template payload for GET /.
type telemetryIndexView struct {
	Groups []telemetryGroup
}

// buildTelemetryIndexView groups all workers by RepoSlug (sorted), and
// within each group sorts online workers before offline, both buckets by
// hostname.
func (s *TelemetryServer) buildTelemetryIndexView() telemetryIndexView {
	now := s.now()
	s.mu.Lock()
	byRepo := map[string][]telemetryWorkerView{}
	for _, ws := range s.workers {
		byRepo[ws.Resource.RepoSlug] = append(byRepo[ws.Resource.RepoSlug], buildTelemetryWorkerView(ws, now))
	}
	s.mu.Unlock()

	var slugs []string
	for slug := range byRepo {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	view := telemetryIndexView{}
	for _, slug := range slugs {
		workers := byRepo[slug]
		sort.Slice(workers, func(i, j int) bool {
			if workers[i].Online != workers[j].Online {
				return workers[i].Online // online sorts before offline
			}
			return workers[i].Hostname < workers[j].Hostname
		})
		view.Groups = append(view.Groups, telemetryGroup{RepoSlug: slug, Workers: workers})
	}
	return view
}

// registerWebHandlers wires the dashboard routes onto mux: GET / (index
// page), GET /detail (htmx poll fragment), and the embedded static assets
// (staticHandler, static.go).
func (s *TelemetryServer) registerWebHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.handleTelemetryIndex)
	mux.HandleFunc("GET /detail", s.handleTelemetryDetail) // Task 5 removes this
	mux.Handle("GET /static/", staticHandler())
}

// handleTelemetryIndex serves the full-width index page. A poll from the
// page's own #content element (identified by the HX-Request header htmx
// sets) gets just the grid fragment; any other request gets the full
// document.
func (s *TelemetryServer) handleTelemetryIndex(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryIndexView()
	if r.Header.Get("HX-Request") == "true" {
		renderTelemetryHTML(w, s.tmpl, "tindex", v)
		return
	}
	renderTelemetryHTML(w, s.tmpl, "tindexPage", v)
}

func (s *TelemetryServer) handleTelemetryDetail(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryView(r.URL.Query().Get("worker"))
	renderTelemetryHTML(w, s.tmpl, "tdetail", v)
}

// renderTelemetryHTML mirrors serve.go's renderHTML: it buffers the render
// so a template error still yields a clean 500, and swallows client-
// disconnect write errors from the 3s poll instead of logging them as noise.
func renderTelemetryHTML(w http.ResponseWriter, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil && !isClientDisconnect(err) {
		_ = err // best-effort write; a disconnect is expected under the 3s poll
	}
}
