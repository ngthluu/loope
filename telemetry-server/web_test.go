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

func TestTelemetryDetailShowsUsageUnknownWhenAbsent(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15}})

	req := httptest.NewRequest(http.MethodGet, "/detail?worker=m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "unknown") {
		t.Fatalf("detail body missing 'unknown':\n%s", rec.Body.String())
	}
}

func TestTelemetryDetailShowsFreshUsagePercentage(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		Usage:    &shared.UsageSnapshot{FiveHourUsedPct: 33.3, CapturedAt: time.Now()},
	})

	req := httptest.NewRequest(http.MethodGet, "/detail?worker=m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "33.3%") {
		t.Fatalf("detail body missing usage percentage:\n%s", rec.Body.String())
	}
}

func TestTelemetryDetailShowsLogTail(t *testing.T) {
	s := newTestTelemetryServer(t)
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1", PushIntervalSec: 15},
		Logs:     []shared.LogRecord{{Body: "watching o/r for label ai-agent"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/detail?worker=m1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "watching o/r for label ai-agent") {
		t.Fatalf("detail body missing log line:\n%s", rec.Body.String())
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
