package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func doPush(t *testing.T, h http.Handler, token string, req PushRequest) *httptest.ResponseRecorder {
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
	s := NewTelemetryServer("secret")
	h := s.Handler()
	req := PushRequest{Resource: Resource{MachineID: "m1"}}

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
	s := NewTelemetryServer("secret")
	rec := doPush(t, s.Handler(), "secret", PushRequest{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandlePushStoresWorkerAndAppendsLogs(t *testing.T) {
	s := NewTelemetryServer("secret")
	h := s.Handler()
	req1 := PushRequest{
		Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		Logs:     []LogRecord{{Body: "line1"}, {Body: "line2"}},
	}
	if rec := doPush(t, h, "secret", req1); rec.Code != http.StatusNoContent {
		t.Fatalf("push 1 status = %d", rec.Code)
	}
	req2 := PushRequest{
		Resource: Resource{MachineID: "m1", RepoSlug: "o/r", Hostname: "host1"},
		Logs:     []LogRecord{{Body: "line3"}},
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
	ws := &WorkerState{Resource: Resource{PushIntervalSec: 15}, LastPushAt: base}

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
	fresh := &UsageSnapshot{FiveHourUsedPct: 10, CapturedAt: base.Add(-29 * time.Minute)}
	stale := &UsageSnapshot{FiveHourUsedPct: 10, CapturedAt: base.Add(-31 * time.Minute)}

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
