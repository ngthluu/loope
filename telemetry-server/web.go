package main

import (
	"github.com/ngthluu/loope/shared"

	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
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
	IssueLogs     []issueLogDirView
}

// issueLogFileView is one file entry in the persisted-logs tree.
type issueLogFileView struct {
	Name string
}

// issueLogDirView is one directory entry in the persisted-logs tree,
// pre-sorted and carrying its display label.
type issueLogDirView struct {
	Name        string
	DisplayName string
	Files       []issueLogFileView
}

// issueLogDisplayName renders "issue-42" as "Issue 42"; anything else
// (namely "triage") displays as-is.
func issueLogDisplayName(name string) string {
	if n, ok := shared.ParseIssueDirName(name); ok {
		return fmt.Sprintf("Issue %d", n)
	}
	return name
}

// buildIssueLogDirViews converts a worker's raw IssueLogs into the sorted
// tree the template renders: numeric "issue-N" dirs (newest-modified first)
// before the shared "triage" dir, each with its files listed by name.
func buildIssueLogDirViews(dirs map[string]shared.IssueLogDir) []issueLogDirView {
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		_, iIsIssue := shared.ParseIssueDirName(names[i])
		_, jIsIssue := shared.ParseIssueDirName(names[j])
		if iIsIssue != jIsIssue {
			return iIsIssue // numeric issue dirs sort before non-numeric ("triage")
		}
		return dirModTime(dirs[names[i]]).After(dirModTime(dirs[names[j]]))
	})
	views := make([]issueLogDirView, 0, len(names))
	for _, name := range names {
		d := dirs[name]
		files := make([]issueLogFileView, 0, len(d.Files))
		for _, f := range d.Files {
			files = append(files, issueLogFileView{Name: f.Name})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		views = append(views, issueLogDirView{Name: name, DisplayName: issueLogDisplayName(name), Files: files})
	}
	return views
}

// findIssueLogFileContent looks up one file's raw content within dirs.
func findIssueLogFileContent(dirs map[string]shared.IssueLogDir, dirName, fileName string) (string, bool) {
	d, ok := dirs[dirName]
	if !ok {
		return "", false
	}
	for _, f := range d.Files {
		if f.Name == fileName {
			return f.Content, true
		}
	}
	return "", false
}

// formatIssueLogFileContent pretty-prints .json files server-side; everything
// else renders as-is — syntax highlighting is explicitly out of scope.
func formatIssueLogFileContent(name, content string) string {
	if !strings.HasSuffix(name, ".json") {
		return content
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(content), "", "  "); err != nil {
		return content
	}
	return pretty.String()
}

// telemetryGroup is one repoSlug's workers, sorted by hostname.
type telemetryGroup struct {
	RepoSlug string
	Workers  []telemetryWorkerView
}

// buildTelemetryWorkerView converts one worker's server-side state into the
// template's flat, pre-formatted shape as of now. Logs and IssueLogs are
// built only when withDetails is set: the index page renders neither, and it
// rebuilds every worker's view under s.mu on every 3s poll — the log tree's
// sort (whose comparator rescans file mod times) is detail-page work the
// index must not pay per card.
func buildTelemetryWorkerView(ws *WorkerState, now time.Time, withDetails bool) telemetryWorkerView {
	v := telemetryWorkerView{
		MachineID:  ws.Resource.MachineID,
		Hostname:   ws.Resource.Hostname,
		WorkDir:    ws.Resource.WorkDir,
		Version:    ws.Resource.Version,
		Online:     ws.online(now),
		LastPushAt: ws.LastPushAt,
	}
	if withDetails {
		v.Logs = ws.Logs.Lines()
		v.IssueLogs = buildIssueLogDirViews(ws.IssueLogs)
	}
	if u := ws.usableUsage(now); u != nil {
		v.FiveHourPct, v.FiveHourReset = u.FiveHourUsedPct, u.FiveHourResetAt
		v.SevenDayPct, v.SevenDayReset = u.SevenDayUsedPct, u.SevenDayResetAt
	} else {
		v.UsageUnknown = true
	}
	return v
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
		byRepo[ws.Resource.RepoSlug] = append(byRepo[ws.Resource.RepoSlug], buildTelemetryWorkerView(ws, now, false))
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
// page), GET /workers/{machineID} (worker detail page), and the embedded
// static assets (staticHandler, static.go).
func (s *TelemetryServer) registerWebHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.handleTelemetryIndex)
	mux.HandleFunc("GET /workers/{machineID}", s.handleTelemetryWorker)
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

// telemetryWorkerPageView is the template payload for GET /workers/{id}.
// Worker is nil when machineID matches no known worker. SelectedFile is
// non-empty when a specific log file is being viewed; FileNotFound is set
// when the requested dir/file no longer exists on the worker's latest push.
type telemetryWorkerPageView struct {
	MachineID    string
	PollURL      string
	Worker       *telemetryWorkerView
	SelectedDir  string
	SelectedFile string
	FileContent  string
	FileNotFound bool
}

// buildTelemetryWorkerPageView looks up machineID and renders its current
// state, optionally resolving the file named by dir/file for the log
// viewer pane. dir and file are both empty when no file is selected.
func (s *TelemetryServer) buildTelemetryWorkerPageView(machineID, dir, file string) telemetryWorkerPageView {
	now := s.now()
	s.mu.Lock()
	ws := s.workers[machineID]
	if ws == nil {
		s.mu.Unlock()
		return telemetryWorkerPageView{MachineID: machineID, PollURL: workerPollURL(machineID, dir, file)}
	}
	wv := buildTelemetryWorkerView(ws, now, true)
	var content string
	var found bool
	if dir != "" && file != "" {
		content, found = findIssueLogFileContent(ws.IssueLogs, dir, file)
	}
	s.mu.Unlock()

	v := telemetryWorkerPageView{MachineID: machineID, PollURL: workerPollURL(machineID, dir, file), Worker: &wv, SelectedDir: dir, SelectedFile: file}
	if dir != "" && file != "" {
		if !found {
			v.FileNotFound = true
		} else {
			v.FileContent = formatIssueLogFileContent(file, content)
		}
	}
	return v
}

// workerPollURL builds the worker page's self-poll URL, preserving the
// selected dir/file so a poll re-renders the same file rather than resetting
// the viewer pane.
func workerPollURL(machineID, dir, file string) string {
	u := "/workers/" + url.PathEscape(machineID)
	if dir != "" && file != "" {
		u += "?" + url.Values{"dir": {dir}, "file": {file}}.Encode()
	}
	return u
}

// handleTelemetryWorker serves the worker detail page. Like the index page,
// a poll from the page's own #content element gets just the content
// fragment; a plain navigation gets the full document.
func (s *TelemetryServer) handleTelemetryWorker(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryWorkerPageView(r.PathValue("machineID"), r.URL.Query().Get("dir"), r.URL.Query().Get("file"))
	if r.Header.Get("HX-Request") == "true" {
		renderTelemetryHTML(w, s.tmpl, "tdetail", v)
		return
	}
	renderTelemetryHTML(w, s.tmpl, "tworkerPage", v)
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
