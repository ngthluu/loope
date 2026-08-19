package main

import (
	"github.com/ngthluu/loope/shared"

	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func doPush(t *testing.T, h http.Handler, token string, req shared.PushRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/push", bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestHandlePushAuth(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	req := shared.PushRequest{Resource: shared.Resource{MachineID: "m1"}}

	if rec := doPush(t, h, "", req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d, want 401", rec.Code)
	}
	if rec := doPush(t, h, "wrong", req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}
	if rec := doPush(t, h, "secret", req); rec.Code != http.StatusNoContent {
		t.Fatalf("correct token: status = %d, want 204", rec.Code)
	}
}

func TestHandlePushRequiresMachineID(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	rec := doPush(t, s.Handler(), "secret", shared.PushRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandlePushStoresWorkerAndAppendsLogs(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	req1 := shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		Logs:     []shared.LogRecord{{Body: "line1"}, {Body: "line2"}},
	}
	if rec := doPush(t, h, "secret", req1); rec.Code != http.StatusNoContent {
		t.Fatalf("push 1 status = %d", rec.Code)
	}
	req2 := shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		Logs:     []shared.LogRecord{{Body: "line3"}},
	}
	if rec := doPush(t, h, "secret", req2); rec.Code != http.StatusNoContent {
		t.Fatalf("push 2 status = %d", rec.Code)
	}

	s.mu.Lock()
	ws := s.workers["m1"]
	s.mu.Unlock()
	if ws == nil {
		t.Fatal("worker m1 not stored")
	}
	got := ws.Logs.Lines()
	want := []string{"line1", "line2", "line3"}
	if len(got) != len(want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %v, want %v", got, want)
		}
	}
}

func TestWorkerStateOnlineOfflineTransition(t *testing.T) {
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	ws := &WorkerState{Resource: shared.Resource{PushIntervalSec: 15}, LastPushAt: base}

	if !ws.online(base.Add(44 * time.Second)) {
		t.Fatal("expected online just under 3x the 15s interval (45s)")
	}
	if ws.online(base.Add(46 * time.Second)) {
		t.Fatal("expected offline just over 3x the 15s interval (45s)")
	}
}

func TestWorkerStateOnlineDefaultsIntervalWhenUnset(t *testing.T) {
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	ws := &WorkerState{LastPushAt: base} // PushIntervalSec zero-value
	if !ws.online(base.Add(44 * time.Second)) {
		t.Fatal("expected the 15s default to apply when PushIntervalSec is unset")
	}
}

func TestWorkerStateUsableUsageStaleness(t *testing.T) {
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	fresh := &shared.UsageSnapshot{FiveHourUsedPct: 10, CapturedAt: base.Add(-29 * time.Minute)}
	stale := &shared.UsageSnapshot{FiveHourUsedPct: 10, CapturedAt: base.Add(-31 * time.Minute)}

	ws := &WorkerState{Usage: fresh}
	if ws.usableUsage(base) == nil {
		t.Fatal("29-minute-old usage must still be usable")
	}
	ws.Usage = stale
	if ws.usableUsage(base) != nil {
		t.Fatal("31-minute-old usage must render as unknown")
	}
	ws.Usage = nil
	if ws.usableUsage(base) != nil {
		t.Fatal("nil usage must stay nil")
	}
}

func TestHandlePushReplacesIssueLogsWholesale(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	req1 := shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		IssueLogs: []shared.IssueLogDir{
			{Name: "issue-1", Files: []shared.IssueLogFile{{Name: "state", Content: "wip"}}},
			{Name: "issue-2", Files: []shared.IssueLogFile{{Name: "state", Content: "done"}}},
		},
	}
	pushWorker(t, s, req1)

	req2 := shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		IssueLogs: []shared.IssueLogDir{
			{Name: "issue-1", Files: []shared.IssueLogFile{{Name: "state", Content: "done"}}},
		},
	}
	pushWorker(t, s, req2)

	s.mu.Lock()
	ws := s.workers["m1"]
	s.mu.Unlock()
	if len(ws.IssueLogs) != 1 {
		t.Fatalf("IssueLogs = %+v, want only issue-1 (issue-2 dropped by the second push)", ws.IssueLogs)
	}
	if got := ws.IssueLogs["issue-1"].Files[0].Content; got != "done" {
		t.Fatalf("issue-1 state = %q, want %q", got, "done")
	}
}

func TestHandlePushEvictsLeastRecentlyModifiedIssueLogDirsOverCap(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	base := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	var dirs []shared.IssueLogDir
	for i := 0; i < 51; i++ {
		dirs = append(dirs, shared.IssueLogDir{
			Name:  fmt.Sprintf("issue-%d", i),
			Files: []shared.IssueLogFile{{Name: "state", Content: "x", ModTime: base.Add(time.Duration(i) * time.Minute)}},
		})
	}
	req := shared.PushRequest{Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r"}, IssueLogs: dirs}
	rec := doPush(t, h, "secret", req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("push status = %d", rec.Code)
	}

	s.mu.Lock()
	ws := s.workers["m1"]
	s.mu.Unlock()
	if len(ws.IssueLogs) != 50 {
		t.Fatalf("len(IssueLogs) = %d, want 50", len(ws.IssueLogs))
	}
	if _, evicted := ws.IssueLogs["issue-0"]; evicted {
		t.Fatal("issue-0 has the oldest ModTime and should have been evicted")
	}
	if _, kept := ws.IssueLogs["issue-50"]; !kept {
		t.Fatal("issue-50 has the newest ModTime and should have been kept")
	}
}

// A push whose IssueLogs is null (the exporter's scan failed that cycle)
// must keep the server's previous view instead of wiping the archive; an
// explicit empty array still clears it.
func TestHandlePushNilIssueLogsKeepsPreviousView(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	pushWorker(t, s, shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		IssueLogs: []shared.IssueLogDir{
			{Name: "issue-1", Files: []shared.IssueLogFile{{Name: "state", Content: "wip"}}},
		},
	})
	pushWorker(t, s, shared.PushRequest{
		Resource:  shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		IssueLogs: nil,
	})
	s.mu.Lock()
	ws := s.workers["m1"]
	s.mu.Unlock()
	if len(ws.IssueLogs) != 1 || ws.IssueLogs["issue-1"].Files[0].Content != "wip" {
		t.Fatalf("IssueLogs = %+v, want issue-1 preserved across a nil push", ws.IssueLogs)
	}

	pushWorker(t, s, shared.PushRequest{
		Resource:  shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		IssueLogs: []shared.IssueLogDir{},
	})
	s.mu.Lock()
	ws = s.workers["m1"]
	s.mu.Unlock()
	if len(ws.IssueLogs) != 0 {
		t.Fatalf("IssueLogs = %+v, want cleared by an explicit empty array", ws.IssueLogs)
	}
}

// A push body over maxPushBodyBytes is rejected instead of being decoded
// into memory.
func TestHandlePushRejectsOversizedBody(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	req := shared.PushRequest{
		Resource: shared.Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		IssueLogs: []shared.IssueLogDir{
			{Name: "issue-1", Files: []shared.IssueLogFile{{Name: "huge", Content: strings.Repeat("x", maxPushBodyBytes)}}},
		},
	}
	rec := doPush(t, s.Handler(), "secret", req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d for an oversized push", rec.Code, http.StatusBadRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers["m1"] != nil {
		t.Fatal("an oversized push must not be ingested")
	}
}
