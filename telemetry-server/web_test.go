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
		`bg-muted`,    // status dot (offline) — templates/rail.html, templates/detail.html
		`w-\[420px\]`, // #rail width — templates/page.html
		`grid-cols-2`, // card grid — templates/rail.html
	} {
		if !strings.Contains(string(css), want) {
			t.Fatalf("static/app.css missing %q — regenerate it: tailwindcss -i tailwind.css -o static/app.css --minify", want)
		}
	}
}
