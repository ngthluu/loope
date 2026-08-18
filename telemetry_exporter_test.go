package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExporterPushOnceSendsLogsAndAuth(t *testing.T) {
	var gotReq PushRequest
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	logPath := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(logPath, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{RepoSlug: "o/r", WorkDir: "/work", Telemetry: &TelemetryConfig{ServerURL: srv.URL, Token: "secret", PushIntervalSec: 15}}
	e := &TelemetryExporter{cfg: cfg, client: srv.Client(), tailer: &LogTailer{path: logPath}}

	if err := e.pushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotReq.Resource.RepoSlug != "o/r" || gotReq.Resource.WorkDir != "/work" || gotReq.Resource.PushIntervalSec != 15 {
		t.Fatalf("resource = %+v", gotReq.Resource)
	}
	if len(gotReq.Logs) != 2 || gotReq.Logs[0].Body != "line1" || gotReq.Logs[1].Body != "line2" {
		t.Fatalf("logs = %+v", gotReq.Logs)
	}
	if gotReq.Usage != nil {
		t.Fatalf("usage = %+v, want nil (no usage file configured)", gotReq.Usage)
	}
}

func TestExporterPushOnceIncludesFreshUsage(t *testing.T) {
	var gotReq PushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	usagePath := filepath.Join(t.TempDir(), "loope-usage.json")
	snap := UsageSnapshot{FiveHourUsedPct: 42, CapturedAt: time.Now()}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(usagePath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{RepoSlug: "o/r", WorkDir: "/work", Telemetry: &TelemetryConfig{ServerURL: srv.URL, Token: "secret", PushIntervalSec: 15}}
	e := &TelemetryExporter{
		cfg:       cfg,
		client:    srv.Client(),
		tailer:    &LogTailer{path: filepath.Join(t.TempDir(), "daemon.log")},
		usagePath: usagePath,
	}

	if err := e.pushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotReq.Usage == nil || gotReq.Usage.FiveHourUsedPct != 42 {
		t.Fatalf("usage = %+v, want FiveHourUsedPct=42", gotReq.Usage)
	}
}

func TestExporterPushOnceReturnsErrorOnNonNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	cfg := &Config{RepoSlug: "o/r", WorkDir: "/work", Telemetry: &TelemetryConfig{ServerURL: srv.URL, Token: "wrong", PushIntervalSec: 15}}
	e := &TelemetryExporter{cfg: cfg, client: srv.Client(), tailer: &LogTailer{path: filepath.Join(t.TempDir(), "daemon.log")}}

	if err := e.pushOnce(context.Background()); err == nil {
		t.Fatal("expected an error on a non-204 response")
	}
}

func TestReadUsageSnapshotStaleReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	stale := UsageSnapshot{CapturedAt: time.Now().Add(-31 * time.Minute)}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readUsageSnapshot(path, time.Now())
	if err != nil || got != nil {
		t.Fatalf("got = %v, err = %v, want nil, nil", got, err)
	}
}

func TestReadUsageSnapshotMissingFileReturnsNil(t *testing.T) {
	got, err := readUsageSnapshot(filepath.Join(t.TempDir(), "nope.json"), time.Now())
	if err != nil || got != nil {
		t.Fatalf("got = %v, err = %v, want nil, nil", got, err)
	}
}

func TestReadUsageSnapshotEmptyPathReturnsNil(t *testing.T) {
	got, err := readUsageSnapshot("", time.Now())
	if err != nil || got != nil {
		t.Fatalf("got = %v, err = %v, want nil, nil", got, err)
	}
}
