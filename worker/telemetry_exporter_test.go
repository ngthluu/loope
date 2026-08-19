package main

import (
	"github.com/ngthluu/loope/shared"

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
	var gotReq shared.PushRequest
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
	var gotReq shared.PushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	usagePath := filepath.Join(t.TempDir(), "loope-usage.json")
	snap := shared.UsageSnapshot{FiveHourUsedPct: 42, CapturedAt: time.Now()}
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

func TestScanIssueLogsReadsFilesFromEachDir(t *testing.T) {
	workDir := t.TempDir()
	issueDir := filepath.Join(workDir, "logs", "issue-42")
	if err := os.MkdirAll(issueDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(issueDir, "003-answer-1.output.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(issueDir, "state"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	triageDir := filepath.Join(workDir, "logs", "triage")
	if err := os.MkdirAll(triageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(triageDir, "session"), []byte("sid"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirs, err := scanIssueLogs(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 {
		t.Fatalf("dirs = %+v, want 2 entries", dirs)
	}
	byName := map[string]shared.IssueLogDir{}
	for _, d := range dirs {
		byName[d.Name] = d
	}
	issue42, ok := byName["issue-42"]
	if !ok || len(issue42.Files) != 2 {
		t.Fatalf("issue-42 = %+v", issue42)
	}
	byFileName := map[string]string{}
	for _, f := range issue42.Files {
		byFileName[f.Name] = f.Content
	}
	if byFileName["003-answer-1.output.md"] != "hello" || byFileName["state"] != "wip" {
		t.Fatalf("issue-42 files = %+v", byFileName)
	}
	triage, ok := byName["triage"]
	if !ok || len(triage.Files) != 1 || triage.Files[0].Name != "session" || triage.Files[0].Content != "sid" {
		t.Fatalf("triage = %+v", triage)
	}
}

func TestScanIssueLogsEmptyOrMissingLogsDirReturnsEmptySlice(t *testing.T) {
	dirs, err := scanIssueLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if dirs == nil || len(dirs) != 0 {
		t.Fatalf("dirs = %#v, want a non-nil empty slice", dirs)
	}
}

func TestExporterPushOnceIncludesIssueLogs(t *testing.T) {
	var gotReq shared.PushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	workDir := t.TempDir()
	issueDir := filepath.Join(workDir, "logs", "issue-7")
	if err := os.MkdirAll(issueDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(issueDir, "title"), []byte("fix the bug"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{RepoSlug: "o/r", WorkDir: workDir, Telemetry: &TelemetryConfig{ServerURL: srv.URL, Token: "secret", PushIntervalSec: 15}}
	e := &TelemetryExporter{cfg: cfg, client: srv.Client(), tailer: &LogTailer{path: filepath.Join(t.TempDir(), "daemon.log")}}

	if err := e.pushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gotReq.IssueLogs) != 1 || gotReq.IssueLogs[0].Name != "issue-7" ||
		len(gotReq.IssueLogs[0].Files) != 1 || gotReq.IssueLogs[0].Files[0].Content != "fix the bug" {
		t.Fatalf("issueLogs = %+v", gotReq.IssueLogs)
	}
}

func TestReadUsageSnapshotStaleReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	stale := shared.UsageSnapshot{CapturedAt: time.Now().Add(-31 * time.Minute)}
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
