package main

import (
	"github.com/ngthluu/loope/shared"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestTelemetryServer(t *testing.T) *TelemetryServer {
	t.Helper()
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pushWorker(t *testing.T, s *TelemetryServer, req shared.PushRequest) {
	t.Helper()
	rec := doPush(t, s.Handler(), "secret", req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("push status = %d", rec.Code)
	}
}

func TestTelemetryIndexGroupsByRepoSlugAndShowsWorkers(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15}})
	pushWorker(t, s, shared.PushRequest{Resource: shared.Resource{MachineID: "m2", RepoSlug: "o/other", Hostname: "host2", PushIntervalSec: 15}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"o/r", "o/other", "m1", "m2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("index body missing %q:\n%s", want, body)
		}
	}
}

func TestTelemetryIndexOrdersOnlineBeforeOfflineWithinRepoSlug(t *testing.T) {
	s := newTestTelemetryServer(t)
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)

	s.now = func() time.Time { return base }
	pushWorker(t, s, shared.PushRequest{Resource: shared.Resource{MachineID: "m-aaa", RepoSlug: "o/r", Hostname: "aaa-host", PushIntervalSec: 15}})

	later := base.Add(60 * time.Second) // > 3*15s online window, so m-aaa is now offline
	s.now = func() time.Time { return later }
	pushWorker(t, s, shared.PushRequest{Resource: shared.Resource{MachineID: "m-zzz", RepoSlug: "o/r", Hostname: "zzz-host", PushIntervalSec: 15}})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	aIdx, zIdx := strings.Index(body, "m-aaa"), strings.Index(body, "m-zzz")
	if aIdx == -1 || zIdx == -1 {
		t.Fatalf("index body missing a worker card:\n%s", body)
	}
	if zIdx > aIdx {
		t.Fatalf("expected online m-zzz before offline m-aaa (alphabetically the reverse), got body:\n%s", body)
	}
}

func TestTelemetryWorkerShowsUsageUnknownWhenAbsent(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15}})

	req := httptest.NewRequest(http.MethodGet, "/workers/m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "unknown") {
		t.Fatalf("worker page body missing 'unknown':\n%s", rec.Body.String())
	}
}

func TestTelemetryWorkerShowsFreshUsagePercentage(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		Usage:    &shared.UsageSnapshot{FiveHourUsedPct: 33.3, CapturedAt: time.Now()},
	})

	req := httptest.NewRequest(http.MethodGet, "/workers/m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "33.3%") {
		t.Fatalf("worker page body missing usage percentage:\n%s", rec.Body.String())
	}
}

func TestTelemetryWorkerShowsLogTail(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		Logs:     []shared.LogRecord{{Body: "watching o/r for label ai-agent"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/workers/m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "watching o/r for label ai-agent") {
		t.Fatalf("worker page body missing log line:\n%s", rec.Body.String())
	}
}

func TestTelemetryWorkerUnknownIDRendersNotFound(t *testing.T) {
	s := newTestTelemetryServer(t)
	req := httptest.NewRequest(http.MethodGet, "/workers/does-not-exist", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a friendly not-found state, not an error status)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("body missing a not-found message:\n%s", rec.Body.String())
	}
}

func TestTelemetryWorkerShowsPersistedLogTree(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		IssueLogs: []shared.IssueLogDir{
			{Name: "issue-42", Files: []shared.IssueLogFile{{Name: "003-answer-1.output.md", Content: "the answer"}}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/workers/m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "Issue 42") {
		t.Fatalf("worker page body missing the issue-42 tree node:\n%s", body)
	}
	if !strings.Contains(body, "003-answer-1.output.md") {
		t.Fatalf("worker page body missing the file entry:\n%s", body)
	}
}

func TestTelemetryWorkerSelectsFileContent(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		IssueLogs: []shared.IssueLogDir{
			{Name: "issue-42", Files: []shared.IssueLogFile{{Name: "003-answer-1.output.md", Content: "the answer is 42"}}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/workers/m1?dir=issue-42&file=003-answer-1.output.md", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "the answer is 42") {
		t.Fatalf("worker page body missing selected file content:\n%s", rec.Body.String())
	}
}

func TestTelemetryWorkerPrettyPrintsJSONFile(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		IssueLogs: []shared.IssueLogDir{
			{Name: "issue-42", Files: []shared.IssueLogFile{{Name: "001-answer.json", Content: `{"a":1}`}}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/workers/m1?dir=issue-42&file=001-answer.json", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	// html/template HTML-escapes quotes inside <pre> content (security: file
	// content is worker-controlled, so it must never be injected unescaped).
	if !strings.Contains(rec.Body.String(), "&#34;a&#34;: 1") {
		t.Fatalf("worker page body missing pretty-printed JSON:\n%s", rec.Body.String())
	}
}

func TestTelemetryWorkerUnknownFileRendersNotFound(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{
		Resource:  shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		IssueLogs: []shared.IssueLogDir{{Name: "issue-42", Files: []shared.IssueLogFile{{Name: "state", Content: "wip"}}}},
	})

	req := httptest.NewRequest(http.MethodGet, "/workers/m1?dir=issue-42&file=nope", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "File not found") {
		t.Fatalf("worker page body missing a file-not-found message:\n%s", rec.Body.String())
	}
}

func TestTelemetryWorkerOrdersIssueDirsBeforeTriageNewestFirst(t *testing.T) {
	s := newTestTelemetryServer(t)
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		IssueLogs: []shared.IssueLogDir{
			{Name: "triage", Files: []shared.IssueLogFile{{Name: "session", ModTime: base.Add(3 * time.Hour)}}},
			{Name: "issue-1", Files: []shared.IssueLogFile{{Name: "state", ModTime: base}}},
			{Name: "issue-2", Files: []shared.IssueLogFile{{Name: "state", ModTime: base.Add(time.Hour)}}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/workers/m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	i2, i1, tri := strings.Index(body, "Issue 2"), strings.Index(body, "Issue 1"), strings.Index(body, "triage")
	if i2 == -1 || i1 == -1 || tri == -1 {
		t.Fatalf("worker page body missing a tree node:\n%s", body)
	}
	if !(i2 < i1 && i1 < tri) {
		t.Fatalf("expected order Issue 2, Issue 1, triage; got positions %d, %d, %d in:\n%s", i2, i1, tri, body)
	}
}

// TestAppCSSCoversTemplateClasses is the guard against the manual Tailwind
// regeneration step being skipped. telemetry-server/static/app.css must be
// regenerated from tailwind.css whenever a template's classes change, or the
// dashboard renders half-styled (or, for a brand-new class, unstyled). A miss
// here means someone changed a template class without re-running:
//
//	tailwindcss -i tailwind.css -o static/app.css --minify
func TestAppCSSCoversTemplateClasses(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if len(css) < 2048 {
		t.Fatalf("static/app.css is only %d bytes — the Tailwind build produced nothing useful", len(css))
	}
	for _, want := range []string{
		`bg-muted`,                // status dot (offline) — templates/index.html, templates/detail.html
		`grid-cols-6`,             // index page card grid — templates/index.html
		`grid-cols-2`,             // usage stats grid — templates/detail.html
		`grid-cols-\[280px_1fr\]`, // persisted-logs tree/viewer split — templates/detail.html
	} {
		if !strings.Contains(string(css), want) {
			t.Fatalf("static/app.css missing %q — regenerate it: tailwindcss -i tailwind.css -o static/app.css --minify", want)
		}
	}
}
