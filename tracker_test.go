package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// errFake is declared in images_test.go and reused here.

// writeStep writes the three artifact files Claude.Call would produce for one
// step. Pass jsonBody="" to simulate a call still in flight (no json yet).
func writeStep(t *testing.T, dir string, seq int, label, prompt, output, jsonBody string) {
	t.Helper()
	base := filepath.Join(dir, itoa3(seq)+"-"+label)
	if prompt != "" {
		mustWrite(t, base+".prompt.md", prompt)
	}
	if output != "" {
		mustWrite(t, base+".output.md", output)
	}
	if jsonBody != "" {
		mustWrite(t, base+".json", jsonBody)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// itoa3 mirrors the %03d sequence prefix Claude.writeLog uses.
func itoa3(n int) string {
	s := make([]byte, 0, 3)
	if n < 10 {
		s = append(s, '0', '0')
	} else if n < 100 {
		s = append(s, '0')
	}
	return string(append(s, []byte(itoa(n))...))
}

func itoa(n int) string { return fmtInt(n) }

func fmtInt(n int) string { return strconv.Itoa(n) }

func TestScanLogs(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// step 1: normal ok step with cost + session
	writeStep(t, dir, 1, "architect", "design it", "here is the design",
		`{"result":"here is the design","session_id":"a3f9","is_error":false,"total_cost_usd":0.51}`)
	// step 2: running (prompt, no json yet)
	writeStep(t, dir, 2, "execute", "build it", "", "")
	// session file recorded by RecordSession
	mustWrite(t, filepath.Join(dir, "session"), `{"sessionId":"a3f9","kind":"feature"}`)

	tickets, err := scanLogs(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 {
		t.Fatalf("want 1 ticket, got %d", len(tickets))
	}
	tk := tickets[0]
	if tk.Number != 142 || tk.Kind != "feature" || tk.SessionID != "a3f9" {
		t.Fatalf("ticket header wrong: %+v", tk)
	}
	if len(tk.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(tk.Steps))
	}
	if tk.Steps[0].Label != "architect" || tk.Steps[0].Status != StatusOK || tk.Steps[0].Cost != 0.51 {
		t.Fatalf("step 1 wrong: %+v", tk.Steps[0])
	}
	if tk.Steps[1].Status != StatusRunning {
		t.Fatalf("step 2 should be running, got %q", tk.Steps[1].Status)
	}
	if tk.TotalCost != 0.51 {
		t.Fatalf("total cost want 0.51, got %v", tk.TotalCost)
	}
}

func TestScanLogsUnparsedAndMissing(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "triage", "pick one", "", "{not valid json")

	tickets, err := scanLogs(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 || len(tickets[0].Steps) != 1 {
		t.Fatalf("unexpected: %+v", tickets)
	}
	if tickets[0].Steps[0].Status != StatusUnparsed {
		t.Fatalf("want unparsed, got %q", tickets[0].Steps[0].Status)
	}
}

func TestScanLogsStepError(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-77")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "execute", "do it", "it broke",
		`{"result":"it broke","session_id":"e1","is_error":true,"total_cost_usd":0.2}`)

	tickets, err := scanLogs(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 || len(tickets[0].Steps) != 1 {
		t.Fatalf("unexpected: %+v", tickets)
	}
	st := tickets[0].Steps[0]
	if st.Status != StatusError {
		t.Fatalf("want StatusError, got %q", st.Status)
	}
	if !st.IsError {
		t.Fatalf("want IsError true")
	}
}

func TestScanLogsNoDir(t *testing.T) {
	tickets, err := scanLogs(t.TempDir()) // no logs/ subdir
	if err != nil {
		t.Fatalf("missing logs dir should not error: %v", err)
	}
	if len(tickets) != 0 {
		t.Fatalf("want 0 tickets, got %d", len(tickets))
	}
}

func trackedListJSON() string {
	// gh issue list --json number,title,labels output. #142 is ours (has logs),
	// #7 is queued for us (our eligible label, no logs yet), and #9 carries only
	// a shared state label with no logs here — i.e. another user's in-flight
	// issue, which the dashboard must not surface.
	return `[
	  {"number":142,"title":"Add OAuth login","labels":[{"name":"ai-wip"}]},
	  {"number":7,"title":"Queued for me","labels":[{"name":"ai-agent"}]},
	  {"number":9,"title":"Another user's issue","labels":[{"name":"ai-done"}]}
	]`
}

func TestBuildTicketsMergesLabels(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "execute", "build it", "done",
		`{"result":"done","session_id":"z1","is_error":false,"total_cost_usd":1.0}`)

	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: trackedListJSON()}}}

	tickets, ghErr := BuildTickets(context.Background(), r, cfg)
	if ghErr != nil {
		t.Fatalf("unexpected gh error: %v", ghErr)
	}
	// #142 (ours, has logs) and #7 (queued under our eligible label) show; #9
	// carries only a shared state label with no logs here, so it belongs to
	// another user and must be dropped.
	if len(tickets) != 2 {
		t.Fatalf("want 2 tickets (ours + queued), got %d: %+v", len(tickets), tickets)
	}
	var t142, t7, t9 *Ticket
	for i := range tickets {
		switch tickets[i].Number {
		case 142:
			t142 = &tickets[i]
		case 7:
			t7 = &tickets[i]
		case 9:
			t9 = &tickets[i]
		}
	}
	if t142 == nil || t142.Title != "Add OAuth login" || t142.StateLabel != "ai-wip" || len(t142.Steps) != 1 {
		t.Fatalf("142 merge wrong: %+v", t142)
	}
	if t7 == nil || t7.Title != "Queued for me" || t7.StateLabel != "ai-agent" || len(t7.Steps) != 0 {
		t.Fatalf("7 (eligible, label-only) wrong: %+v", t7)
	}
	if t9 != nil {
		t.Fatalf("9 (another user's state-labeled issue, no logs) must not appear: %+v", t9)
	}
}

// TestLocalStateWinsOverGitHub covers the live-transition path: a local state
// marker (written by the loop the moment it relabels) must override the
// once-fetched gh snapshot, while a ticket with no marker still falls back to
// gh's label.
func TestLocalStateWinsOverGitHub(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "execute", "build it", "done",
		`{"result":"done","session_id":"z1","is_error":false,"total_cost_usd":1.0}`)
	// The loop has since shipped #142 and recorded Done locally, but gh's cached
	// snapshot below still reports it as ai-wip.
	recordState(dir, "ai-done")

	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: trackedListJSON()}}}

	tickets, ghErr := BuildTickets(context.Background(), r, cfg)
	if ghErr != nil {
		t.Fatalf("unexpected gh error: %v", ghErr)
	}
	var t142 *Ticket
	for i := range tickets {
		if tickets[i].Number == 142 {
			t142 = &tickets[i]
		}
	}
	if t142 == nil || t142.StateLabel != "ai-done" {
		t.Fatalf("142 should reflect local ai-done over gh ai-wip: %+v", t142)
	}

	// clearState returns the issue to gh's view.
	clearState(dir)
	tickets, _ = BuildTickets(context.Background(), &fakeRunner{queue: []rresp{{stdout: trackedListJSON()}}}, cfg)
	for i := range tickets {
		if tickets[i].Number == 142 && tickets[i].StateLabel != "ai-wip" {
			t.Fatalf("after clearState, 142 should fall back to gh ai-wip, got %q", tickets[i].StateLabel)
		}
	}
}

// TestBuildTicketsGHArgv locks down the read-only `gh issue list` invocation
// shape, the one integration seam fakeRunner otherwise leaves unchecked.
func TestBuildTicketsGHArgv(t *testing.T) {
	work := t.TempDir()
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: "[]"}}}

	if _, ghErr := BuildTickets(context.Background(), r, cfg); ghErr != nil {
		t.Fatalf("unexpected gh error: %v", ghErr)
	}
	if len(r.calls) != 1 {
		t.Fatalf("want 1 gh call, got %d", len(r.calls))
	}
	args := r.calls[0].args
	if !hasArg(args, "issue") || !hasArg(args, "list") {
		t.Fatalf("args missing issue list: %v", args)
	}
	if got := argAfter(args, "--repo"); got != "o/r" {
		t.Fatalf("--repo = %q, want o/r", got)
	}
	// The fetch is scoped to this instance's eligible label, not the shared
	// state labels, so a multi-user repo's other tickets never show up here (and
	// can't crowd ours past the fetch limit).
	if got := argAfter(args, "--search"); got != "label:ai-agent" {
		t.Fatalf("--search = %q, want label:ai-agent (eligible-scoped)", got)
	}
	if got := argAfter(args, "--state"); got != "all" {
		t.Fatalf("--state = %q, want all", got)
	}
	if !hasArg(args, "--limit") {
		t.Fatalf("args missing --limit: %v", args)
	}
	if got := argAfter(args, "--json"); got != "number,title,labels" {
		t.Fatalf("--json = %q, want number,title,labels", got)
	}
}

// With no eligible label configured, the dashboard falls back to searching the
// shared state labels so a single-user / legacy config still shows a board.
func TestBuildTicketsGHArgvNoEligibleLabel(t *testing.T) {
	work := t.TempDir()
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: "[]"}}}

	if _, ghErr := BuildTickets(context.Background(), r, cfg); ghErr != nil {
		t.Fatalf("unexpected gh error: %v", ghErr)
	}
	if got := argAfter(r.calls[0].args, "--search"); got != "label:ai-wip,ai-done,ai-rework,ai-stopped" {
		t.Fatalf("--search = %q, want the shared state-label fallback", got)
	}
}

func TestBuildTicketsGHFallback(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-3")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "triage", "x", "y",
		`{"result":"y","session_id":"s","is_error":false,"total_cost_usd":0.1}`)

	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stderr: "gh: offline", err: errFake}}}

	tickets, ghErr := BuildTickets(context.Background(), r, cfg)
	if ghErr == nil {
		t.Fatal("want a gh error for the banner")
	}
	if len(tickets) != 1 || tickets[0].Number != 3 {
		t.Fatalf("fallback should still show logs-only ticket, got %+v", tickets)
	}
}

func TestScanLogsCarriesUsage(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-9")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "brainstorm-0", "p", "o",
		`{"result":"o","session_id":"a","is_error":false,"total_cost_usd":2.63,
		 "num_turns":23,"duration_ms":206302,
		 "usage":{"input_tokens":18934,"cache_creation_input_tokens":161846,
		 "cache_read_input_tokens":1280292,"output_tokens":10933}}`)
	writeStep(t, dir, 2, "answer-1", "p", "o",
		`{"result":"o","session_id":"b","is_error":false,"total_cost_usd":0.14,
		 "num_turns":2,"duration_ms":9000,
		 "usage":{"input_tokens":100,"cache_creation_input_tokens":0,
		 "cache_read_input_tokens":900,"output_tokens":50}}`)

	tickets, err := scanLogs(work)
	if err != nil {
		t.Fatal(err)
	}
	tk := tickets[0]
	s0 := tk.Steps[0]
	if s0.NumTurns != 23 || s0.DurationMS != 206302 ||
		s0.InputTokens != 18934 || s0.CacheCreationTokens != 161846 ||
		s0.CacheReadTokens != 1280292 || s0.OutputTokens != 10933 {
		t.Fatalf("step 0 usage wrong: %+v", s0)
	}
	// ticket context total = sum of (input+create+read); output total summed too.
	wantIn := (18934 + 161846 + 1280292) + (100 + 0 + 900)
	wantOut := 10933 + 50
	if tk.TotalInputTokens != wantIn || tk.TotalOutputTokens != wantOut {
		t.Fatalf("ticket totals wrong: in=%d(want %d) out=%d(want %d)",
			tk.TotalInputTokens, wantIn, tk.TotalOutputTokens, wantOut)
	}
}

func steps(labels ...string) []Step {
	s := make([]Step, len(labels))
	for i, l := range labels {
		s[i] = Step{Seq: i + 1, Label: l}
	}
	return s
}

func TestPipelineRowsPairsConversation(t *testing.T) {
	rows := pipelineRows(steps("brainstorm-0", "answer-1", "brainstorm-1", "answer-2", "execute"))
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d: %+v", len(rows), rows)
	}
	if !rows[0].HasLeft || rows[0].Left.Label != "brainstorm-0" ||
		!rows[0].HasRight || rows[0].Right.Label != "answer-1" {
		t.Fatalf("row 0 wrong: %+v", rows[0])
	}
	if rows[1].Left.Label != "brainstorm-1" || rows[1].Right.Label != "answer-2" {
		t.Fatalf("row 1 wrong: %+v", rows[1])
	}
	// execute is architect-side with no answer: left cell only.
	if !rows[2].HasLeft || rows[2].Left.Label != "execute" || rows[2].HasRight {
		t.Fatalf("row 2 (execute alone) wrong: %+v", rows[2])
	}
}

func TestPipelineRowsDoneConfirmIsAnswerer(t *testing.T) {
	rows := pipelineRows(steps("brainstorm-0", "done-confirm-1"))
	if len(rows) != 1 || !rows[0].HasLeft || !rows[0].HasRight ||
		rows[0].Right.Label != "done-confirm-1" {
		t.Fatalf("done-confirm should fill the right cell: %+v", rows)
	}
}

func TestPipelineRowsAllArchitectFallback(t *testing.T) {
	labels := []string{"debug", "execute"}
	if hasAnswerer(steps(labels...)) {
		t.Fatal("debug/execute pipeline should have no answerer")
	}
	rows := pipelineRows(steps(labels...))
	if len(rows) != 2 || rows[0].HasRight || rows[1].HasRight {
		t.Fatalf("all-architect should be one left cell per row: %+v", rows)
	}
}

func TestPipelineRowsEmpty(t *testing.T) {
	if got := pipelineRows(nil); len(got) != 0 {
		t.Fatalf("empty steps -> empty rows, got %+v", got)
	}
}

func TestScanReadsTranscriptPRAndRunningSession(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A running step: prompt + stream, no .json yet.
	os.WriteFile(filepath.Join(dir, "001-execute.prompt.md"), []byte("go"), 0o644)
	os.WriteFile(filepath.Join(dir, "001-execute.stream.jsonl"), []byte(
		`{"type":"system","subtype":"init","session_id":"sess-run"}`+"\n"+
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"x.go"}}]}}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "pr"), []byte("https://github.com/o/r/pull/9\n"), 0o644)

	tickets, err := scanLogs(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 1 {
		t.Fatalf("got %d tickets", len(tickets))
	}
	tk := tickets[0]
	if tk.PRURL != "https://github.com/o/r/pull/9" {
		t.Errorf("PRURL = %q", tk.PRURL)
	}
	if tk.SessionID != "sess-run" {
		t.Errorf("running SessionID = %q, want sess-run", tk.SessionID)
	}
	if len(tk.Steps) != 1 || tk.Steps[0].Status != StatusRunning {
		t.Fatalf("steps = %+v", tk.Steps)
	}
	if n := len(tk.Steps[0].Transcript); n != 1 || tk.Steps[0].Transcript[0].Tool != "Edit" {
		t.Errorf("transcript = %+v", tk.Steps[0].Transcript)
	}
}

func TestRecordPRWritesFile(t *testing.T) {
	dir := t.TempDir()
	recordPR(dir, "https://github.com/o/r/pull/3")
	b, err := os.ReadFile(filepath.Join(dir, "pr"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "https://github.com/o/r/pull/3" {
		t.Errorf("pr file = %q", b)
	}
	// Empty inputs are no-ops.
	recordPR("", "x")
	recordPR(dir, "")
}

func TestParkCauseRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "issue-7")
	if got := readParkCause(dir); got != "" {
		t.Errorf("missing file should read empty, got %q", got)
	}
	recordParkCause(dir, "claude execute: terminated: max_turns")
	if got := readParkCause(dir); got != "claude execute: terminated: max_turns" {
		t.Errorf("readParkCause = %q", got)
	}
	clearParkCause(dir)
	if got := readParkCause(dir); got != "" {
		t.Errorf("cleared cause should read empty, got %q", got)
	}
	// Best-effort like the other writers: no panic on empty inputs.
	recordParkCause("", "x")
	recordParkCause(dir, "")
	clearParkCause("")
}

func TestParseTranscript(t *testing.T) {
	raw := `{"type":"system","subtype":"init","session_id":"sess-1"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"Editing now"},{"type":"tool_use","name":"Edit","input":{"file_path":"serve.go"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","is_error":false}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./"}}]}}
{"type":"result","is_error":false,"result":"done"}` + "\n"
	events, sid := parseTranscript(raw)
	if sid != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", sid)
	}
	want := []TranscriptEvent{
		{Kind: "thinking", Text: "pondering"},
		{Kind: "text", Text: "Editing now"},
		{Kind: "tool", Tool: "Edit", Detail: "serve.go"},
		{Kind: "tool_result", IsError: false},
		{Kind: "tool", Tool: "Bash", Detail: "go test ./"},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event[%d] = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestPickStateLabelStoppedBeatsReworkAndDone(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	labels := []Label{{Name: "ai-agent"}, {Name: "ai-rework"}, {Name: "ai-stopped"}}
	if got := pickStateLabel(labels, cfg); got != "ai-stopped" {
		t.Fatalf("pickStateLabel = %q, want ai-stopped", got)
	}
}

func TestPickStateLabelWIPBeatsStopped(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	labels := []Label{{Name: "ai-stopped"}, {Name: "ai-wip"}}
	if got := pickStateLabel(labels, cfg); got != "ai-wip" {
		t.Fatalf("pickStateLabel = %q, want ai-wip", got)
	}
}

func TestTrackedStateLabelsIncludesStopped(t *testing.T) {
	cfg := &Config{StateLabels: defaultStateLabels()}
	found := false
	for _, l := range trackedStateLabels(cfg) {
		if l == "ai-stopped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("trackedStateLabels = %v, want it to include ai-stopped", trackedStateLabels(cfg))
	}
}

func TestStateKindAndStripeForStopped(t *testing.T) {
	cfg := &Config{EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	if got := stateKind(cfg, "ai-stopped"); got != "stopped" {
		t.Fatalf("stateKind = %q, want stopped", got)
	}
	if got := stripeClass(cfg, "ai-stopped"); got != "bg-muted/40" {
		t.Fatalf("stripeClass = %q, want bg-muted/40", got)
	}
}

func TestStopMarkerLifecycle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "issue-7")
	if stopRequested(dir) {
		t.Fatal("no marker written yet, stopRequested should be false")
	}
	recordStopRequest(dir)
	if !stopRequested(dir) {
		t.Fatal("after recordStopRequest, stopRequested should be true")
	}
	b, err := os.ReadFile(filepath.Join(dir, stopFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err != nil {
		t.Fatalf("marker content %q is not an RFC3339 timestamp: %v", b, err)
	}
	clearStopRequest(dir)
	if stopRequested(dir) {
		t.Fatal("after clearStopRequest, stopRequested should be false")
	}
}

func TestStopMarkerEmptyDirIsNoOp(t *testing.T) {
	recordStopRequest("")
	clearStopRequest("")
	if stopRequested("") {
		t.Fatal("empty logDir must never report a stop")
	}
}
