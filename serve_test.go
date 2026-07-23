package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "architect", "design the thing", "the design output",
		`{"result":"the design output","session_id":"a3f9","is_error":false,"total_cost_usd":0.51}`)
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: `[{"number":142,"title":"Add OAuth login","labels":[{"name":"ai-wip"}]}]`}}}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestServeIndexRendersSelectedTicket(t *testing.T) {
	h := newTestServer(t).Handler()
	code, body := get(t, h, "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{"Add OAuth login", "architect", "the design output", "a3f9"} {
		if !strings.Contains(body, want) {
			t.Fatalf("index body missing %q", want)
		}
	}
}

func TestServeRailFragment(t *testing.T) {
	h := newTestServer(t).Handler()
	code, body := get(t, h, "/rail")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "#142") {
		t.Fatalf("rail missing ticket #142: %s", body)
	}
	// The rail is a fragment, not a full document.
	if strings.Contains(body, "<html") {
		t.Fatalf("rail should be a fragment, got full page")
	}
}

func TestDetailRouteRendersFragment(t *testing.T) {
	cfg := &Config{RepoPath: "/tmp", RepoSlug: "o/r", WorkDir: t.TempDir(), StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	s, err := NewServer(&fakeRunner{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/detail", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Error("/detail must be a fragment, not the full page")
	}
}

// TestServeEmptyQueueRenders covers the no-tickets branch: an empty logs dir
// and a gh call that returns no issues must still render a 200 with the empty
// rail + detail placeholder copy, not a template error.
func TestServeEmptyQueueRenders(t *testing.T) {
	cfg := &Config{WorkDir: t.TempDir(), RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: "[]"}}}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	code, body := get(t, s.Handler(), "/")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{"No tickets in flight", "Select a ticket"} {
		if !strings.Contains(body, want) {
			t.Fatalf("empty-queue body missing %q", want)
		}
	}
}

// TestServeGitHubUnreachableRenders covers the degraded branch: when gh fails,
// the page must render 200 from logs alone with the unreachable banner and the
// missing-title fallback rather than 500.
func TestServeGitHubUnreachableRenders(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "architect", "design the thing", "the design output",
		`{"result":"the design output","session_id":"a3f9","is_error":false,"total_cost_usd":0.51}`)
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{err: errors.New("could not connect")}}}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	code, body := get(t, s.Handler(), "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{"GitHub unreachable", "awaiting GitHub title", "architect"} {
		if !strings.Contains(body, want) {
			t.Fatalf("gh-unreachable body missing %q", want)
		}
	}
}

// newTestServerWithRunner is like newTestServer but hands back the
// fakeRunner too, and only queues a single response, so tests can assert
// on how many gh calls the cache allowed through.
func newTestServerWithRunner(t *testing.T) (*Server, *fakeRunner) {
	t.Helper()
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "architect", "design the thing", "the design output",
		`{"result":"the design output","session_id":"a3f9","is_error":false,"total_cost_usd":0.51}`)
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: `[{"number":142,"title":"Add OAuth login","labels":[{"name":"ai-wip"}]}]`}}}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

// TestServeCachesGitHubWithinTTL asserts the dashboard shells `gh` a single
// time for polls that fall inside the TTL window: the GitHub title/label data
// is fetched once and reused until the TTL elapses. The clock is pinned so all
// five polls land in the same window; only one response is queued, so a
// regression that re-fetched early would exhaust the queue — the calls count is
// the reliable signal.
//
// Ticket #142 has no PRURL, so the first poll also triggers backfillPR's
// one-time (per process) PR-by-branch lookup; with an empty queue it fails
// gracefully (empty gh output) and is never retried on the later polls, adding
// exactly 1 to the expected call count below.
func TestServeCachesGitHubWithinTTL(t *testing.T) {
	s, r := newTestServerWithRunner(t)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	h := s.Handler()

	for i := 0; i < 5; i++ {
		if code, _ := get(t, h, "/rail"); code != http.StatusOK {
			t.Fatalf("poll %d status = %d", i, code)
		}
	}
	if len(r.calls) != 2 {
		t.Fatalf("want exactly 2 gh calls across 5 polls within the TTL (1 issue list + 1 one-time PR backfill), got %d", len(r.calls))
	}
}

// TestServeRepollsGitHubAfterTTL is the regression guard for the "new label
// doesn't appear until restart" bug: once the TTL elapses, the next poll must
// re-query GitHub and surface an issue labeled after startup. A fetch-once
// dashboard would never make the second call and #200 would never show.
func TestServeRepollsGitHubAfterTTL(t *testing.T) {
	s, r := newTestServerWithRunner(t)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	r.queue = []rresp{
		{stdout: `[{"number":142,"title":"Add OAuth login","labels":[{"name":"ai-wip"}]}]`},
		// Ticket #142 has no PRURL, so this first poll also fires backfillPR's
		// one-time PR-by-branch lookup; this response answers that call ("no
		// PR for this branch") so it doesn't steal the post-TTL refetch below.
		{stdout: `{"url":""}`},
		{stdout: `[{"number":142,"title":"Add OAuth login","labels":[{"name":"ai-wip"}]},{"number":200,"title":"Second issue","labels":[{"name":"ai-agent"}]}]`},
	}
	h := s.Handler()

	if _, body := get(t, h, "/rail"); !strings.Contains(body, "Add OAuth login") {
		t.Fatal("first poll should show the initial issue")
	}
	// Still inside the TTL: no re-fetch, the newly labeled issue stays hidden.
	if _, body := get(t, h, "/rail"); strings.Contains(body, "#200") {
		t.Fatal("within the TTL the dashboard should serve the cached list")
	}
	if len(r.calls) != 2 {
		t.Fatalf("want 2 gh calls within the TTL (1 issue list + 1 one-time PR backfill), got %d", len(r.calls))
	}
	// Advance past the TTL: the next poll must re-query and reveal #200.
	now = now.Add(s.ttl + time.Second)
	if _, body := get(t, h, "/rail"); !strings.Contains(body, "#200") {
		t.Fatal("after the TTL the dashboard should re-poll and show the new issue")
	}
	if len(r.calls) != 3 {
		t.Fatalf("want 3 gh calls after the TTL elapsed (plus the one-time PR backfill), got %d", len(r.calls))
	}
}

// TestServeRetriesGitHubUntilSuccess covers the startup-blip case: if the first
// gh fetch fails, the dashboard keeps trying on later polls and stops once one
// succeeds — after which no further gh calls are made.
func TestServeRetriesGitHubUntilSuccess(t *testing.T) {
	s, r := newTestServerWithRunner(t)
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	// First poll fails, second succeeds, then a fifth response would only be
	// consumed by an (incorrect) re-fetch. Two per-ticket backfills also run
	// once per selected ticket regardless of gh success/failure, and both fire
	// on this very first poll (ticket #142 has neither a PRURL nor — while the
	// list is failing — a title): responses 2 and 3 answer them permanently
	// ("no PR for this branch", "no such issue"), so they are memoized and
	// don't steal the real issue-list response meant for the second poll.
	r.queue = []rresp{
		{err: errors.New("could not connect")},
		{stdout: `{"url":""}`},
		{err: errors.New("could not resolve to an issue")},
		{stdout: `[{"number":142,"title":"Add OAuth login","labels":[{"name":"ai-wip"}]}]`},
		{stdout: "[]"},
	}
	h := s.Handler()

	if _, body := get(t, h, "/?issue=142"); !strings.Contains(body, "GitHub unreachable") {
		t.Fatalf("first poll should show the unreachable banner")
	}
	if _, body := get(t, h, "/?issue=142"); !strings.Contains(body, "Add OAuth login") {
		t.Fatalf("second poll should show the recovered GitHub title")
	}
	if _, body := get(t, h, "/?issue=142"); !strings.Contains(body, "Add OAuth login") {
		t.Fatalf("third poll should reuse the memoized title")
	}
	if len(r.calls) != 4 {
		t.Fatalf("want 4 gh calls (one failed list, one succeeded, plus the one-time PR and title backfills), got %d", len(r.calls))
	}
}

// newFeatureServer builds a server whose issue has a brainstorm/answer pipeline
// with usage data, so the two-column layout and token surfaces are exercised.
func newFeatureServer(t *testing.T) *Server {
	t.Helper()
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "brainstorm-0", "design it", "the design output",
		`{"result":"the design output","session_id":"a3f9","is_error":false,"total_cost_usd":2.63,
		 "num_turns":23,"duration_ms":206302,
		 "usage":{"input_tokens":18934,"cache_creation_input_tokens":161846,
		 "cache_read_input_tokens":1280292,"output_tokens":10933}}`)
	writeStep(t, dir, 2, "answer-1", "answer it", "the answer",
		`{"result":"the answer","session_id":"b0","is_error":false,"total_cost_usd":0.14,
		 "num_turns":2,"duration_ms":9000,
		 "usage":{"input_tokens":100,"cache_creation_input_tokens":0,
		 "cache_read_input_tokens":900,"output_tokens":50}}`)
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: `[{"number":142,"title":"Add OAuth login","labels":[{"name":"ai-wip"}]}]`}}}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestServeRendersTokenSurfaces(t *testing.T) {
	code, body := get(t, newFeatureServer(t).Handler(), "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		"brainstorm-0", "answer-1", "the design output", "the answer",
		"tokens", // the ticket-level tile label
		"1.46M",  // step-0 context total 1,461,072 humanized
		"11k",    // step-0 output 10,933 humanized
		"usage",  // the per-step usage disclosure
		"3m26s",  // step-0 duration in the usage block
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("feature detail body missing %q", want)
		}
	}
}

func TestServeSingleColumnFallbackNoAnswerer(t *testing.T) {
	// The default test server's only step is "architect" (no answerer), so the
	// detail must not render the two-column grid marker.
	code, body := get(t, newTestServer(t).Handler(), "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if strings.Contains(body, `data-layout="two-col"`) {
		t.Fatal("all-architect ticket should use the single-column fallback")
	}
}

// TestDetailShowsGitHubLinksAndSession covers the always-on issue link, the
// conditional PR link (only when the ticket has a PRURL), and the copy button
// that carries the full session id in data-sid regardless of the shortened
// display text.
func TestDetailShowsGitHubLinksAndSession(t *testing.T) {
	cfg := &Config{RepoPath: "/tmp", RepoSlug: "o/r", WorkDir: t.TempDir(), StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	s, err := NewServer(&fakeRunner{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	v := view{Tickets: []Ticket{{Number: 7, Title: "T", SessionID: "abc-123-def", PRURL: "https://github.com/o/r/pull/9", Steps: []Step{{Seq: 1, Label: "execute", Status: StatusRunning}}}}}
	v.Selected = &v.Tickets[0]
	var b strings.Builder
	if err := s.tmpl.ExecuteTemplate(&b, "detail", v); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{
		"https://github.com/o/r/issues/7",
		"https://github.com/o/r/pull/9",
		`data-sid="abc-123-def"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("detail missing %q", want)
		}
	}
}

// TestBackfillPRCachesForSelectedTicket covers the legacy-ticket backfill: a
// ticket with no PRURL gets one looked up by branch, cached to its pr file, and
// set in memory — and a second attempt for the same issue is short-circuited by
// prTried rather than re-querying gh.
func TestBackfillPRCachesForSelectedTicket(t *testing.T) {
	work := t.TempDir()
	cfg := &Config{RepoPath: "/tmp", RepoSlug: "o/r", WorkDir: work, StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	f := &fakeRunner{queue: []rresp{{stdout: `{"url":"https://github.com/o/r/pull/5"}`}}}
	s, err := NewServer(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tk := &Ticket{Number: 5}
	s.backfillPR(context.Background(), tk)
	if tk.PRURL != "https://github.com/o/r/pull/5" {
		t.Errorf("PRURL = %q", tk.PRURL)
	}
	b, err := os.ReadFile(filepath.Join(work, "logs", "issue-5", "pr"))
	if err != nil {
		t.Fatalf("pr file not written: %v", err)
	}
	if string(b) != "https://github.com/o/r/pull/5" {
		t.Errorf("pr file = %q", b)
	}
	// Second attempt for the same issue is short-circuited by prTried, so the
	// empty queue is never consulted (no error, no change) and no additional
	// subprocess call is made.
	callsBefore := len(f.calls)
	tk2 := &Ticket{Number: 5}
	s.backfillPR(context.Background(), tk2)
	if tk2.PRURL != "" {
		t.Errorf("re-query happened for a tried issue: %q", tk2.PRURL)
	}
	if len(f.calls) != callsBefore {
		t.Errorf("gh was called again for a tried issue: %d calls, want %d", len(f.calls), callsBefore)
	}
}

// TestBackfillPRRetriesAfterTransientError verifies a transient gh outage is NOT
// memoized: a blip on one poll must not permanently suppress the PR link, so a
// later poll retries and finds it. (A permanent "no PR" error IS memoized — the
// at-most-once behavior the TTL tests pin.)
func TestBackfillPRRetriesAfterTransientError(t *testing.T) {
	work := t.TempDir()
	cfg := &Config{RepoPath: "/tmp", RepoSlug: "o/r", WorkDir: work, StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	// Three transient failures (exhausting the bounded retry) then a success.
	f := &fakeRunner{queue: []rresp{
		{stderr: "could not resolve host github.com", err: errors.New("exit 1")},
		{stderr: "could not resolve host github.com", err: errors.New("exit 1")},
		{stderr: "could not resolve host github.com", err: errors.New("exit 1")},
		{stdout: `{"url":"https://github.com/o/r/pull/5"}`},
	}}
	s, err := NewServer(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.gh.retry = testRetry // bounded so the transient error surfaces instead of looping
	// First poll: gh outage exhausts retries, nothing cached, prTried stays unset.
	tk := &Ticket{Number: 5}
	s.backfillPR(context.Background(), tk)
	if tk.PRURL != "" {
		t.Errorf("transient outage should not set a PRURL, got %q", tk.PRURL)
	}
	// Second poll: not short-circuited (prTried unset) — retries and finds the PR.
	tk2 := &Ticket{Number: 5}
	s.backfillPR(context.Background(), tk2)
	if tk2.PRURL != "https://github.com/o/r/pull/5" {
		t.Errorf("later poll should find the PR after a transient outage, got %q", tk2.PRURL)
	}
}

// TestBackfillPRSkipsWhenAlreadySet covers the common case on every later poll:
// a ticket that already has a PRURL (from ship-time recordPR or an earlier
// backfill) must not trigger a gh call at all.
func TestBackfillPRSkipsWhenAlreadySet(t *testing.T) {
	cfg := &Config{RepoPath: "/tmp", RepoSlug: "o/r", WorkDir: t.TempDir(), StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	f := &fakeRunner{}
	s, err := NewServer(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tk := &Ticket{Number: 7, PRURL: "https://github.com/o/r/pull/7"}
	s.backfillPR(context.Background(), tk)
	if tk.PRURL != "https://github.com/o/r/pull/7" {
		t.Errorf("PRURL mutated = %q", tk.PRURL)
	}
	if len(f.calls) != 0 {
		t.Errorf("gh was called for an already-set ticket: %d calls", len(f.calls))
	}
}

func TestTokensHumanize(t *testing.T) {
	cases := map[int]string{
		0: "0", 27: "27", 999: "999",
		1000: "1k", 10933: "11k", 161846: "162k",
		1000000: "1.00M", 1461072: "1.46M", 1280292: "1.28M",
	}
	for in, want := range cases {
		if got := tokens(in); got != want {
			t.Errorf("tokens(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDurationFormat(t *testing.T) {
	cases := map[int]string{
		0: "—", 45000: "45s", 206302: "3m26s", 3_720_000: "1h02m",
	}
	for in, want := range cases {
		if got := duration(in); got != want {
			t.Errorf("duration(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCtxTokensAndHasUsage(t *testing.T) {
	s := Step{InputTokens: 100, CacheCreationTokens: 20, CacheReadTokens: 900, OutputTokens: 50}
	if ctxTokens(s) != 1020 {
		t.Fatalf("ctxTokens = %d, want 1020", ctxTokens(s))
	}
	if !hasUsage(s) {
		t.Fatal("hasUsage should be true when tokens present")
	}
	if hasUsage(Step{}) {
		t.Fatal("hasUsage should be false for a step with no usage")
	}
	if !hasUsage(Step{NumTurns: 1}) {
		t.Fatal("hasUsage should be true when only NumTurns present")
	}
}

func TestStepcardRendersTranscript(t *testing.T) {
	cfg := &Config{RepoPath: "/tmp", RepoSlug: "o/r", WorkDir: t.TempDir(), StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	s, err := NewServer(&fakeRunner{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	step := Step{Seq: 4, Label: "execute", Status: StatusRunning, Transcript: []TranscriptEvent{
		{Kind: "text", Text: "Editing now"},
		{Kind: "tool", Tool: "Edit", Detail: "serve.go"},
		{Kind: "tool_result", IsError: false},
	}}
	var b strings.Builder
	if err := s.tmpl.ExecuteTemplate(&b, "stepcard", step); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{`class="txfeed`, `data-seq="4"`, "Editing now", "Edit", "serve.go"} {
		if !strings.Contains(html, want) {
			t.Errorf("stepcard missing %q", want)
		}
	}
}

// TestTxLineEscapesHTML guards the escaping boundary in txLine: a future
// accidental removal of esc(...) would let a tool name, tool detail, or
// assistant text containing HTML-significant characters break out of the
// fixed markup.
func TestTxLineEscapesHTML(t *testing.T) {
	textOut := string(txLine(TranscriptEvent{Kind: "text", Text: "<script>alert(1)</script>"}))
	if !strings.Contains(textOut, "&lt;script&gt;") {
		t.Errorf("text event not escaped: %q", textOut)
	}
	if strings.Contains(textOut, "<script>") {
		t.Errorf("text event leaked literal <script>: %q", textOut)
	}

	toolOut := string(txLine(TranscriptEvent{Kind: "tool", Tool: "Ed<it", Detail: "a & b"}))
	if !strings.Contains(toolOut, "Ed&lt;it") {
		t.Errorf("tool name not escaped: %q", toolOut)
	}
	if !strings.Contains(toolOut, "a &amp; b") {
		t.Errorf("tool detail not escaped: %q", toolOut)
	}
}

// railTitleEnv builds a dashboard over a single issue-<n> log dir (one step, no
// title file) plus a gh handler that mimics the reported bug: the issue no
// longer carries the eligible label, so the label-scoped `gh issue list` never
// returns it — its title is only reachable via a per-issue `gh issue view`.
func railTitleEnv(t *testing.T, num int, title string) (*Server, *fakeRunner) {
	t.Helper()
	work := t.TempDir()
	dir := filepath.Join(work, "logs", fmt.Sprintf("issue-%d", num))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "architect", "design the thing", "the design output",
		`{"result":"the design output","session_id":"a3f9","is_error":false,"total_cost_usd":0.51}`)
	mustWrite(t, filepath.Join(dir, stateFile), "ai-done")
	cfg := &Config{WorkDir: work, RepoPath: "/clone", RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{handler: func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		switch {
		case strings.HasPrefix(joined, "issue list"):
			return "[]", "", nil
		case strings.HasPrefix(joined, "issue view"):
			return fmt.Sprintf(`{"title":%q}`, title), "", nil
		}
		return "", "", nil
	}}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

// TestServeBackfillsTitleForUnlabeledIssue reproduces issue #16: a finished
// ticket whose issue lost the eligible label drops out of the label-scoped
// issue list, leaving the card stuck on the "awaiting GitHub title" placeholder
// forever. The dashboard must fall back to a per-issue title lookup.
func TestServeBackfillsTitleForUnlabeledIssue(t *testing.T) {
	s, _ := railTitleEnv(t, 3, "Enhance: add Stop/Continue")
	code, body := get(t, s.Handler(), "/?issue=3")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "Enhance: add Stop/Continue") {
		t.Fatalf("body missing backfilled title: %s", body)
	}
	if strings.Contains(body, "awaiting GitHub title") {
		t.Fatalf("body still shows the placeholder: %s", body)
	}
}

// TestServeTitleBackfillPersistsAndIsNotRepeated asserts the backfill is cached
// like the PR backfill: the title is written to the issue's log dir (so it
// survives a restart with GitHub unreachable) and `gh issue view` is not
// re-issued on every 3s poll.
func TestServeTitleBackfillPersistsAndIsNotRepeated(t *testing.T) {
	s, r := railTitleEnv(t, 3, "Enhance: add Stop/Continue")
	for i := 0; i < 3; i++ {
		get(t, s.Handler(), "/?issue=3")
	}
	views := 0
	for _, c := range r.calls {
		if c.name == "gh" && strings.HasPrefix(strings.Join(c.args, " "), "issue view") {
			views++
		}
	}
	if views != 1 {
		t.Fatalf("gh issue view called %d times, want 1", views)
	}
	body, err := os.ReadFile(filepath.Join(s.cfg.WorkDir, "logs", "issue-3", titleFile))
	if err != nil {
		t.Fatalf("title not persisted: %v", err)
	}
	if strings.TrimSpace(string(body)) != "Enhance: add Stop/Continue" {
		t.Fatalf("persisted title = %q", body)
	}
}

// TestServeUsesPersistedTitleWhenGitHubUnreachable is the restart case from the
// issue: the process comes up with GitHub down, so nothing can be fetched — the
// title recorded on disk during the run must still render.
func TestServeUsesPersistedTitleWhenGitHubUnreachable(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStep(t, dir, 1, "architect", "design the thing", "the design output",
		`{"result":"the design output","session_id":"a3f9","is_error":false,"total_cost_usd":0.51}`)
	mustWrite(t, filepath.Join(dir, titleFile), "Add OAuth login")
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{handler: func(rcall) (string, string, error) { return "", "", errors.New("could not connect") }}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	code, body := get(t, s.Handler(), "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "Add OAuth login") {
		t.Fatalf("persisted title missing while gh is down: %s", body)
	}
	if strings.Contains(body, "awaiting GitHub title") {
		t.Fatalf("placeholder shown despite persisted title: %s", body)
	}
}

// TestServePersistsFetchedTitles covers the cheap path: when the issue list does
// return the ticket, its title is mirrored to the log dir so the next restart
// needs no gh call at all.
func TestServePersistsFetchedTitles(t *testing.T) {
	s := newTestServer(t)
	get(t, s.Handler(), "/?issue=142")
	body, err := os.ReadFile(filepath.Join(s.cfg.WorkDir, "logs", "issue-142", titleFile))
	if err != nil {
		t.Fatalf("fetched title not persisted: %v", err)
	}
	if strings.TrimSpace(string(body)) != "Add OAuth login" {
		t.Fatalf("persisted title = %q", body)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, tc := range []struct{ path, ctPart string }{
		{"/static/htmx.min.js", "javascript"},
		{"/static/idiomorph-ext.min.js", "javascript"},
		{"/static/app.js", "javascript"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: empty body", tc.path)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.ctPart) {
			t.Fatalf("%s: content-type = %q, want it to contain %q", tc.path, ct, tc.ctPart)
		}
	}
}

func TestStaticUnknownPath404(t *testing.T) {
	h := newTestServer(t).Handler()
	code, _ := get(t, h, "/static/nope.js")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
}

// The asset tree is an implementation detail: net/http's file server would
// happily render a directory index for it, which is an endpoint the dashboard
// never had and never wants.
func TestStaticDirectoryListingIs404(t *testing.T) {
	h := newTestServer(t).Handler()
	// "/static/." is not covered here: ServeMux normalizes it to "/static/"
	// with a 301 before any handler sees it.
	code, body := get(t, h, "/static/")
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
	if strings.Contains(body, "app.js") {
		t.Errorf("body listed the asset directory: %q", body)
	}
}

func TestPageWiresHTMXPolling(t *testing.T) {
	h := newTestServer(t).Handler()
	code, body := get(t, h, "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	for _, want := range []string{
		`src="/static/htmx.min.js"`,
		`src="/static/idiomorph-ext.min.js"`,
		`src="/static/app.js"`,
		`hx-ext="morph"`,
		`hx-get="/rail?issue=142"`,
		`hx-get="/detail?issue=142"`,
		`hx-trigger="every 3s"`,
		`hx-swap="morph:innerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page missing %q", want)
		}
	}
}

// The rail's ticket rows and the pipeline's step cards must stay keyed, so
// idiomorph matches them by identity instead of rebuilding them on every poll.
// The key has to be `id`: idiomorph builds its match sets from id attributes
// only, so a bespoke data-* key would look keyed while morphing positionally.
func TestPollFragmentsAreKeyed(t *testing.T) {
	h := newTestServer(t).Handler()
	_, rail := get(t, h, "/rail?issue=142")
	if !strings.Contains(rail, `id="t142"`) {
		t.Fatalf("rail row not keyed: %s", rail)
	}
	_, detail := get(t, h, "/detail?issue=142")
	if !strings.Contains(detail, `id="s1"`) {
		t.Fatalf("step card not keyed: %s", detail)
	}
}

func TestRailFragmentCarriesOOBStatbar(t *testing.T) {
	h := newTestServer(t).Handler()
	code, body := get(t, h, "/rail?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Fatalf("rail fragment carries no out-of-band statbar: %s", body)
	}
	if !strings.Contains(body, `id="statbar"`) {
		t.Fatalf("out-of-band statbar has no id to swap into: %s", body)
	}
	// The stats the header shows must be in the fragment: one ticket, none
	// running (the seeded step is settled), $0.51 spent.
	for _, want := range []string{`id="stat-tickets" class="text-base font-semibold tabular-nums text-text">1<`, `id="stat-running">0<`, `>$0.51<`} {
		if !strings.Contains(body, want) {
			t.Fatalf("statbar missing %q: %s", want, body)
		}
	}
	// The dead patching hook is gone.
	if strings.Contains(body, "railmeta") {
		t.Fatalf("rail still emits the obsolete #railmeta element")
	}
}

// TestAppCSSCoversBothClassSources is the guard against the manual Tailwind
// regeneration step being skipped. Half the dashboard's classes exist only in
// templates and half only in Go helpers, so app.css is checked for one sentinel
// from each source. A miss means someone changed classes without re-running:
//
//	tailwindcss -i web/tailwind.css -o web/static/app.css --minify
func TestAppCSSCoversBothClassSources(t *testing.T) {
	css, err := webFS.ReadFile("web/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if len(css) < 4096 {
		t.Fatalf("app.css is only %d bytes — the Tailwind build produced nothing useful", len(css))
	}
	for _, want := range []string{
		`line-clamp-2`, // template-only: web/templates/rail.html
		`bg-ok\/50`,    // Go-only: stripeClass in render.go
	} {
		if !strings.Contains(string(css), want) {
			t.Fatalf("app.css missing %q — regenerate it: tailwindcss -i web/tailwind.css -o web/static/app.css --minify", want)
		}
	}
}

func TestStaticCSSServed(t *testing.T) {
	h := newTestServer(t).Handler()
	req := httptest.NewRequest(http.MethodGet, "/static/app.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("empty body")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("content-type = %q, want text/css", ct)
	}
}

func TestPageLinksVendoredCSSNotCDN(t *testing.T) {
	h := newTestServer(t).Handler()
	_, body := get(t, h, "/")
	if !strings.Contains(body, `href="/static/app.css"`) {
		t.Fatalf("page does not link the vendored stylesheet")
	}
	if strings.Contains(body, "cdn.tailwindcss.com") {
		t.Fatalf("page still loads the Tailwind browser CDN")
	}
}

func post(t *testing.T, h http.Handler, target string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, nil).WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// serverWithOrch builds a Server and a real Orchestrator sharing one fake runner
// and workDir, with issue 7's log dir seeded to the given state.
func serverWithOrch(t *testing.T, state string, session bool) (*Server, *fakeEnv) {
	t.Helper()
	env := newFakeEnv(t)
	cfg := &Config{
		RepoPath: "/clone", RepoSlug: "org/repo", EligibleLabel: "ai-agent",
		WorkDir: env.wtDir, MaxQARounds: 3, StateLabels: defaultStateLabels(),
		Models: Models{Architect: ModelConfig{Model: "opus"}, Triage: ModelConfig{Model: "sonnet"}},
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if session {
		if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(`{"sessionId":"s1","kind":"bug"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recordState(logDir, state)
	o := &Orchestrator{cfg: cfg, runner: env.f, gh: NewGitHub(env.f, cfg), wt: &Worktree{runner: env.f, repoPath: cfg.RepoPath}}
	o.gh.retry = testRetry
	s, err := NewServer(env.f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.orch = o
	return s, env
}

func TestContinueRouteQueuesReworkAndRendersFragment(t *testing.T) {
	s, env := serverWithOrch(t, "ai-stopped", true /* session */)
	code, body := post(t, s.Handler(), "/continue?issue=7")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if strings.Contains(body, "<html") {
		t.Fatal("continue should return the detail fragment, not a full page")
	}
	swap := env.callsMatching("gh", "--remove-label ai-stopped")
	if len(swap) != 1 || !strings.Contains(swap[0], "--add-label ai-rework") {
		t.Fatalf("continue did not queue rework, got %v", swap)
	}
}

func TestStopRouteInvokesOrchestratorAndRenders(t *testing.T) {
	s, _ := serverWithOrch(t, "ai-wip", true)
	// Nothing is in flight, so Stop returns errNotRunning and we render an inline
	// notice — not a 5xx.
	code, body := post(t, s.Handler(), "/stop?issue=7")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (sentinel is non-fatal)", code)
	}
	if !strings.Contains(body, "not running") {
		t.Fatalf("expected an inline not-running notice, got: %s", body)
	}
}

func TestMutateRouteRejectsBadIssue(t *testing.T) {
	s, _ := serverWithOrch(t, "ai-wip", true)
	code, _ := post(t, s.Handler(), "/stop?issue=abc")
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a non-numeric issue", code)
	}
}

func TestMutationRoutesDisabledOnReadOnlyServer(t *testing.T) {
	// A server built without an orchestrator (orch == nil) is read-only. The
	// mutation routes must not be exposed at all — POSTing to one has to be refused
	// cleanly, never dereference the nil orchestrator and panic.
	h := newTestServer(t).Handler() // no orch wired
	for _, route := range []string{"/stop?issue=142", "/continue?issue=142"} {
		code, _ := post(t, h, route)
		if code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s on a read-only server = %d, want 405 (route not exposed)", route, code)
		}
	}
}

func TestStateKindMapsStopped(t *testing.T) {
	cfg := &Config{StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	if got := stateKind(cfg, "ai-stopped"); got != "stopped" {
		t.Fatalf("stateKind(ai-stopped) = %q, want stopped", got)
	}
	if got := stripeClass(cfg, "ai-stopped"); got == "bg-line2" || got == "" {
		t.Fatalf("stripeClass(ai-stopped) = %q, want a distinct stopped tone", got)
	}
}

func TestPickStateLabelPrefersStoppedOverEligible(t *testing.T) {
	cfg := &Config{StateLabels: defaultStateLabels(), EligibleLabel: "ai-agent"}
	labels := []Label{{Name: "ai-agent"}, {Name: "ai-stopped"}}
	if got := pickStateLabel(labels, cfg); got != "ai-stopped" {
		t.Fatalf("pickStateLabel = %q, want ai-stopped (not the eligible label)", got)
	}
}

func TestDetailRendersStopButtonForWip(t *testing.T) {
	// newTestServer seeds issue 142 labeled ai-wip.
	code, body := get(t, newTestServer(t).Handler(), "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, `hx-post="/stop?issue=142"`) {
		t.Fatalf("wip detail should render a Stop button, got: %s", body)
	}
	if strings.Contains(body, `hx-post="/continue?issue=142"`) {
		t.Fatal("wip detail must not render a Continue button")
	}
}

func TestDetailRendersContinueButtonForStopped(t *testing.T) {
	work := t.TempDir()
	dir := filepath.Join(work, "logs", "issue-142")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordState(dir, "ai-stopped")
	cfg := &Config{WorkDir: work, RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()}
	r := &fakeRunner{queue: []rresp{{stdout: `[{"number":142,"title":"Add OAuth","labels":[{"name":"ai-agent"},{"name":"ai-stopped"}]}]`}}}
	s, err := NewServer(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	code, body := get(t, s.Handler(), "/?issue=142")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, `hx-post="/continue?issue=142"`) {
		t.Fatalf("stopped detail should render a Continue button, got: %s", body)
	}
}
