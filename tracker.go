package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type StepStatus string

const (
	StatusOK       StepStatus = "ok"
	StatusError    StepStatus = "error"
	StatusRunning  StepStatus = "running"
	StatusUnparsed StepStatus = "unparsed"
)

// Step is one Claude.Call within an issue's pipeline, reconstructed from the
// NNN-<label>.{prompt.md,json,output.md,stream.jsonl} artifacts.
type Step struct {
	Seq                 int
	Label               string
	Prompt              string
	Output              string
	SessionID           string
	IsError             bool
	Cost                float64
	Status              StepStatus
	NumTurns            int
	DurationMS          int
	InputTokens         int
	CacheCreationTokens int
	CacheReadTokens     int
	OutputTokens        int
	Transcript          []TranscriptEvent
}

// Ticket is one issue's view: GitHub-sourced Title/StateLabel (filled by the
// caller that merges gh data) plus the log-sourced Kind/SessionID/cost/steps/PRURL.
type Ticket struct {
	Number            int
	Title             string
	StateLabel        string
	Kind              string
	SessionID         string
	PRURL             string
	TotalCost         float64
	TotalInputTokens  int
	TotalOutputTokens int
	Steps             []Step
	LastActive        time.Time
}

// TranscriptEvent is one rendered line of a step's live transcript, decoded from
// a stream-json event. Kind is "text" (assistant prose), "thinking", "tool" (a
// tool_use, with Tool name and Detail = its file path or command), or
// "tool_result" (with IsError set from the result).
type TranscriptEvent struct {
	Kind    string
	Text    string
	Tool    string
	Detail  string
	IsError bool
}

// stepArtifact classifies a log filename into its (base, ext) where base is the
// "NNN-label" grouping key. ok is false for files that are not step artifacts.
func stepArtifact(name string) (base, ext string, ok bool) {
	switch {
	case strings.HasSuffix(name, ".prompt.md"):
		return strings.TrimSuffix(name, ".prompt.md"), "prompt", true
	case strings.HasSuffix(name, ".output.md"):
		return strings.TrimSuffix(name, ".output.md"), "output", true
	case strings.HasSuffix(name, ".stream.jsonl"):
		return strings.TrimSuffix(name, ".stream.jsonl"), "stream", true
	case strings.HasSuffix(name, ".json"):
		return strings.TrimSuffix(name, ".json"), "json", true
	}
	return "", "", false
}

// parseTranscript decodes stream-json NDJSON into ordered UI events plus the
// first session id it sees. Malformed or unknown lines are skipped, so a
// partially-written stream (a step still running) parses cleanly.
func parseTranscript(raw string) ([]TranscriptEvent, string) {
	var events []TranscriptEvent
	var sessionID string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
			Message   struct {
				Content []struct {
					Type     string         `json:"type"`
					Text     string         `json:"text"`
					Thinking string         `json:"thinking"`
					Name     string         `json:"name"`
					Input    map[string]any `json:"input"`
					IsError  bool           `json:"is_error"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if sessionID == "" && ev.SessionID != "" {
			sessionID = ev.SessionID
		}
		switch ev.Type {
		case "assistant":
			for _, b := range ev.Message.Content {
				switch b.Type {
				case "text":
					if b.Text != "" {
						events = append(events, TranscriptEvent{Kind: "text", Text: b.Text})
					}
				case "thinking":
					if b.Thinking != "" {
						events = append(events, TranscriptEvent{Kind: "thinking", Text: b.Thinking})
					}
				case "tool_use":
					events = append(events, TranscriptEvent{Kind: "tool", Tool: b.Name, Detail: toolDetail(b.Input)})
				}
			}
		case "user":
			for _, b := range ev.Message.Content {
				if b.Type == "tool_result" {
					events = append(events, TranscriptEvent{Kind: "tool_result", IsError: b.IsError})
				}
			}
		}
	}
	return events, sessionID
}

// toolDetail pulls the most useful one-line hint out of a tool_use input: the
// file path for edit/read/write tools, else the command for Bash (trimmed), else
// empty.
func toolDetail(input map[string]any) string {
	if fp, ok := input["file_path"].(string); ok && fp != "" {
		return fp
	}
	if cmd, ok := input["command"].(string); ok && cmd != "" {
		if len(cmd) > 80 {
			r := []rune(cmd)
			if len(r) > 80 {
				return string(r[:80]) + "…"
			}
		}
		return cmd
	}
	return ""
}

// splitBase parses "042-architect" into seq=42, label="architect".
func splitBase(base string) (seq int, label string, ok bool) {
	i := strings.IndexByte(base, '-')
	if i <= 0 {
		return 0, "", false
	}
	n, err := strconv.Atoi(base[:i])
	if err != nil {
		return 0, "", false
	}
	return n, base[i+1:], true
}

// stateFile is the local marker the orchestrator drops in an issue's log dir
// the instant it changes the issue's state label. It holds the raw label string
// (e.g. "ai-done"). The dashboard reads it on every disk scan so loop-driven
// transitions (picked up → WIP, shipped → Done, parked → Rework) show up live
// without re-polling GitHub, which is fetched only once.
const stateFile = "state"

// recordState writes the issue's current state label to <logDir>/state,
// creating the dir if needed. Best-effort, like the other log-writers: a no-op
// on empty inputs and errors are swallowed, since a failed state write must
// never derail the transition it is only mirroring.
func recordState(logDir, label string) {
	if logDir == "" || label == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, stateFile), []byte(label), 0o644)
}

// titleFile holds the issue's GitHub title, mirrored to disk the moment the
// loop (or the dashboard) learns it. Without it the title lives only in the
// label-scoped `gh issue list` the dashboard runs, so any issue that drops out
// of that query — a human editing its labels after it finished, a >100-result
// repo, GitHub unreachable on a fresh start — renders forever as the
// "awaiting GitHub title" placeholder even though the run is long done.
const titleFile = "title"

// recordTitle writes the issue's GitHub title to <logDir>/title. Best-effort,
// matching the other log-writers.
func recordTitle(logDir, title string) {
	if logDir == "" || title == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, titleFile), []byte(title), 0o644)
}

// recordPR writes the issue's PR URL to <logDir>/pr so the dashboard can link to
// it without a gh call. Best-effort, matching the other log-writers.
func recordPR(logDir, url string) {
	if logDir == "" || url == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, "pr"), []byte(url), 0o644)
}

// clearState removes the local state marker, returning the issue to whatever
// state GitHub reports (typically back to eligible). Used when the loop backs an
// issue out to be re-picked. Best-effort.
func clearState(logDir string) {
	if logDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(logDir, stateFile))
}

// parkCauseFile holds the failure text that parked the issue as ai-rework, so
// the auto-resume scan can decide whether the cause is transient (usage limit,
// budget ceiling, network outage) without re-deriving it from GitHub comments.
const parkCauseFile = "park-cause"

// recordParkCause writes the park cause to <logDir>/park-cause. Best-effort,
// like the other log-writers.
func recordParkCause(logDir, msg string) {
	if logDir == "" || msg == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, parkCauseFile), []byte(msg), 0o644)
}

// readParkCause returns the recorded park cause, or "" when none exists.
func readParkCause(logDir string) string {
	b, err := os.ReadFile(filepath.Join(logDir, parkCauseFile))
	if err != nil {
		return ""
	}
	return string(b)
}

// clearParkCause removes the park cause when the issue leaves the parked state.
func clearParkCause(logDir string) {
	if logDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(logDir, parkCauseFile))
}

// scanLogs reads workDir/logs and returns one Ticket per issue-<N> dir, steps
// ordered by seq and cost summed, sorted by LastActive descending. A missing
// logs dir yields an empty slice, not an error.
func scanLogs(workDir string) ([]Ticket, error) {
	logsDir := filepath.Join(workDir, "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Ticket{}, nil
		}
		return nil, err
	}
	var tickets []Ticket
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "issue-") {
			continue
		}
		num, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "issue-"))
		if err != nil {
			continue
		}
		tk, ok := scanIssueDir(filepath.Join(logsDir, e.Name()), num)
		if ok {
			tickets = append(tickets, tk)
		}
	}
	sortTickets(tickets)
	if tickets == nil {
		tickets = []Ticket{}
	}
	return tickets, nil
}

// sortTickets orders tickets by LastActive descending, breaking ties by
// ascending Number so tickets sharing a zero LastActive (label-only, no
// logs yet) don't jitter between refreshes.
func sortTickets(tickets []Ticket) {
	sort.SliceStable(tickets, func(i, j int) bool {
		if !tickets[i].LastActive.Equal(tickets[j].LastActive) {
			return tickets[i].LastActive.After(tickets[j].LastActive)
		}
		return tickets[i].Number < tickets[j].Number
	})
}

// scanIssueDir reconstructs one Ticket from a single issue-<N> log directory.
func scanIssueDir(dir string, num int) (Ticket, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Ticket{}, false
	}
	type raw struct {
		seq                   int
		label                 string
		prompt, output, jsonB string
		stream                string
		hasJSON               bool
	}
	byBase := map[string]*raw{}
	var order []string
	tk := Ticket{Number: num}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if info, ierr := e.Info(); ierr == nil {
			if info.ModTime().After(tk.LastActive) {
				tk.LastActive = info.ModTime()
			}
		}
		if name == "session" {
			if data, rerr := os.ReadFile(filepath.Join(dir, name)); rerr == nil {
				var si SessionInfo
				if json.Unmarshal(data, &si) == nil {
					tk.Kind = si.Kind
					tk.SessionID = si.SessionID
				}
			}
			continue
		}
		if name == stateFile {
			if data, rerr := os.ReadFile(filepath.Join(dir, name)); rerr == nil {
				tk.StateLabel = strings.TrimSpace(string(data))
			}
			continue
		}
		if name == titleFile {
			if data, rerr := os.ReadFile(filepath.Join(dir, name)); rerr == nil {
				tk.Title = strings.TrimSpace(string(data))
			}
			continue
		}
		if name == "pr" {
			if data, rerr := os.ReadFile(filepath.Join(dir, name)); rerr == nil {
				tk.PRURL = strings.TrimSpace(string(data))
			}
			continue
		}
		base, ext, ok := stepArtifact(name)
		if !ok {
			continue
		}
		seq, label, ok := splitBase(base)
		if !ok {
			continue
		}
		r := byBase[base]
		if r == nil {
			r = &raw{seq: seq, label: label}
			byBase[base] = r
			order = append(order, base)
		}
		body, _ := os.ReadFile(filepath.Join(dir, name))
		switch ext {
		case "prompt":
			r.prompt = string(body)
		case "output":
			r.output = string(body)
		case "json":
			r.jsonB = string(body)
			r.hasJSON = true
		case "stream":
			r.stream = string(body)
		}
	}

	for _, base := range order {
		r := byBase[base]
		st := Step{Seq: r.seq, Label: r.label, Prompt: r.prompt, Output: r.output}
		switch {
		case r.hasJSON:
			var cr ClaudeResult
			if json.Unmarshal([]byte(r.jsonB), &cr) != nil {
				st.Status = StatusUnparsed
			} else {
				st.Cost = cr.CostUSD
				st.SessionID = cr.SessionID
				st.IsError = cr.IsError
				st.NumTurns = cr.NumTurns
				st.DurationMS = cr.DurationMS
				st.InputTokens = cr.Usage.InputTokens
				st.CacheCreationTokens = cr.Usage.CacheCreationTokens
				st.CacheReadTokens = cr.Usage.CacheReadTokens
				st.OutputTokens = cr.Usage.OutputTokens
				if cr.IsError {
					st.Status = StatusError
				} else {
					st.Status = StatusOK
				}
				tk.TotalCost += cr.CostUSD
				tk.TotalInputTokens += cr.Usage.InputTokens + cr.Usage.CacheCreationTokens + cr.Usage.CacheReadTokens
				tk.TotalOutputTokens += cr.Usage.OutputTokens
			}
		case r.prompt != "":
			st.Status = StatusRunning
		default:
			st.Status = StatusOK
		}
		if r.stream != "" {
			events, sid := parseTranscript(r.stream)
			st.Transcript = events
			if st.SessionID == "" {
				st.SessionID = sid
			}
		}
		tk.Steps = append(tk.Steps, st)
	}
	sort.Slice(tk.Steps, func(i, j int) bool { return tk.Steps[i].Seq < tk.Steps[j].Seq })
	if tk.SessionID == "" {
		for i := len(tk.Steps) - 1; i >= 0; i-- {
			if tk.Steps[i].SessionID != "" {
				tk.SessionID = tk.Steps[i].SessionID
				break
			}
		}
	}
	return tk, true
}

// answererStep reports whether a step is a product-owner-proxy turn (the right
// lane of the two-column pipeline). The feature pipeline labels these answer-N
// and done-confirm-N; every other label (brainstorm-N, execute, and the
// single-session debug/rework/triage pipelines) is architect-side.
func answererStep(s Step) bool {
	return strings.HasPrefix(s.Label, "answer") || strings.HasPrefix(s.Label, "done-confirm")
}

// hasAnswerer reports whether any step is answerer-side. When false the detail
// view keeps its single-column spine instead of a two-column grid with a dead
// right lane.
func hasAnswerer(steps []Step) bool {
	for _, s := range steps {
		if answererStep(s) {
			return true
		}
	}
	return false
}

// PipeRow is one row of the two-column pipeline: an architect turn on the left
// and the answerer turn that responds to it on the right. Either cell may be
// absent (Has* false) — brainstorm-0 before its first answer, or a lone execute.
type PipeRow struct {
	Left, Right       Step
	HasLeft, HasRight bool
}

// pipelineRows groups seq-ordered steps into conversation rows: an architect
// step opens a new row in the left cell; an answerer step fills the current
// row's empty right cell, else opens its own row. Cells are Step values (not
// pointers) so the template can pass them to funcs without a type mismatch.
func pipelineRows(steps []Step) []PipeRow {
	var rows []PipeRow
	for _, s := range steps {
		if answererStep(s) {
			if n := len(rows); n > 0 && !rows[n-1].HasRight {
				rows[n-1].Right, rows[n-1].HasRight = s, true
				continue
			}
			rows = append(rows, PipeRow{Right: s, HasRight: true})
			continue
		}
		rows = append(rows, PipeRow{Left: s, HasLeft: true})
	}
	return rows
}

// trackedStateLabels returns the loop's board states (wip/rework/done). It is
// only the fallback search set for a config with no eligible label; the normal
// path scopes the fetch to the eligible label instead (see listTrackedIssues).
func trackedStateLabels(cfg *Config) []string {
	return []string{cfg.StateLabels.WIP, cfg.StateLabels.Done, cfg.StateLabels.Rework, cfg.StateLabels.Stopped}
}

// hasLabel reports whether labels contains name (name == "" is never a match).
func hasLabel(labels []Label, name string) bool {
	if name == "" {
		return false
	}
	for _, l := range labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// pickStateLabel returns the first tracked label present on the issue, in
// priority order WIP > Rework > Done > eligible, or "" if none.
func pickStateLabel(labels []Label, cfg *Config) string {
	has := func(name string) bool {
		for _, l := range labels {
			if l.Name == name {
				return true
			}
		}
		return false
	}
	for _, name := range []string{cfg.StateLabels.WIP, cfg.StateLabels.Rework, cfg.StateLabels.Stopped, cfg.StateLabels.Done, cfg.EligibleLabel} {
		if name != "" && has(name) {
			return name
		}
	}
	return ""
}

// listTrackedIssues shells `gh issue list` (through Runner) for this instance's
// issues, across open and closed state (done issues close).
//
// The fetch is scoped to the eligible label, not the state labels. State labels
// (ai-wip/ai-done/…) are shared by everyone running the tool against this repo,
// so a state-label search would pull in other users' tickets — and on a busy
// repo the many closed ai-done issues could crowd this user's tickets past the
// --limit. The eligible label rides along an issue for its whole lifecycle
// (queued → wip → rework → done, since only state labels are swapped), so it
// alone selects exactly this instance's tickets in every state. Only when no
// eligible label is configured does it fall back to the shared state labels.
func listTrackedIssues(ctx context.Context, r Runner, repoPath, slug, eligible string, stateLabels []string) ([]Issue, error) {
	labels := []string{eligible}
	if eligible == "" {
		labels = stateLabels
	}
	search := "label:" + strings.Join(labels, ",")
	stdout, stderr, err := r.Run(ctx, repoPath, nil, "", "gh",
		"issue", "list", "--repo", slug, "--search", search,
		"--state", "all", "--limit", "100", "--json", "number,title,labels")
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %w (stderr: %s)", err, tail(stderr, 300))
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(stdout), &issues); err != nil {
		return nil, fmt.Errorf("parse issue list: %w", err)
	}
	return issues, nil
}

// overlayIssues merges GitHub title/state-label data onto log-derived tickets,
// unioning by issue number: known tickets gain their Title/StateLabel, and
// label-only issues (no logs yet) are appended as step-less tickets. The result
// is re-sorted so the freshly appended tickets land in order.
func overlayIssues(tickets []Ticket, issues []Issue, cfg *Config) []Ticket {
	byNum := map[int]int{} // issue number -> index in tickets
	for i := range tickets {
		byNum[tickets[i].Number] = i
	}
	for _, is := range issues {
		state := pickStateLabel(is.Labels, cfg)
		if idx, ok := byNum[is.Number]; ok {
			// gh is authoritative for the title (the issue may have been
			// renamed), but a titleless entry must not wipe the one recovered
			// from the log dir.
			if is.Title != "" {
				tickets[idx].Title = is.Title
			}
			// A local state marker (written by the loop at the moment it changed
			// the label) is fresher than the once-fetched gh snapshot, so let it
			// stand and only fall back to gh's label when there is no local one.
			if tickets[idx].StateLabel == "" {
				tickets[idx].StateLabel = state
			}
			continue
		}
		// No local logs for this issue. The state labels (ai-wip/ai-done/…) are
		// shared across everyone running the tool against this repo, so a
		// label-only issue with no logs in *this* WorkDir belongs to another
		// user's loop — append it only when it carries our eligible label, i.e.
		// it is queued for this instance but not yet picked up. Anything we have
		// actually worked on has logs and took the branch above.
		if !hasLabel(is.Labels, cfg.EligibleLabel) {
			continue
		}
		tickets = append(tickets, Ticket{Number: is.Number, Title: is.Title, StateLabel: state, Steps: []Step{}})
	}
	sortTickets(tickets)
	return tickets
}

// BuildTickets scans logs and overlays GitHub label/title data, unioning by
// issue number. On a gh failure it returns the logs-only tickets plus ghErr so
// the caller can show a "GitHub unreachable" banner.
func BuildTickets(ctx context.Context, r Runner, cfg *Config) ([]Ticket, error) {
	tickets, err := scanLogs(cfg.WorkDir)
	if err != nil {
		return nil, err
	}
	issues, ghErr := listTrackedIssues(ctx, r, cfg.RepoPath, cfg.RepoSlug, cfg.EligibleLabel, trackedStateLabels(cfg))
	if ghErr != nil {
		return tickets, ghErr
	}
	return overlayIssues(tickets, issues, cfg), nil
}
