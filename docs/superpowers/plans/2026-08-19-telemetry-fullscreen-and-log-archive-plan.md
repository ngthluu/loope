# Telemetry: full-screen worker grid, per-worker page, and persisted-log archive Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the telemetry dashboard's rail+pane shell with a full-width index grid and a real per-worker page, and extend the worker→server push so the server also collects and lets you browse each worker's persisted per-issue log files.

**Architecture:** Extend the existing `shared.PushRequest` wire type with an `IssueLogs` field carrying full file contents from each worker's `<WorkDir>/logs/*` tree (no new transport, per decision 2). The server stores them per-worker, capped and replaced wholesale each push, exactly like the existing `Usage`/`Logs` fields. The dashboard's two htmx-polled fragment routes (`/rail`, `/detail`) are replaced by two real pages (`GET /`, `GET /workers/{machineID}`) that each self-poll via an inner `#content` element, detecting htmx polls via the `HX-Request` header to return just the fragment instead of the full document.

**Tech Stack:** Go 1.25.5, `html/template`, htmx + idiomorph (already vendored under `telemetry-server/static/`), Tailwind CSS v4 (`telemetry-server/tailwind.css` → `static/app.css`).

**Spec:** `docs/superpowers/specs/2026-08-19-telemetry-fullscreen-and-log-archive-design.md`

## Global Constraints

- No new third-party dependencies — the wire-format extension reuses the existing `PushRequest` push, no OTel SDK, no callback/pull endpoint (design decision 2).
- The grid is a fixed 6 columns, desktop-only — no responsive breakpoints (design decision 5).
- Log file contents render as plain `<pre>`, monospace, no syntax highlighting, except `.json` files are pretty-printed server-side (design's "Persisted-logs browser" section).
- `IssueLogs` is never persisted to disk on the server — still a live, in-memory view (design's "Out of scope").
- The worker-side scan of `logs/*` is non-recursive — the existing writers in `worker/tracker.go`/`worker/claude.go` never nest subdirectories (design's "Worker exporter" section).
- Server caps retained `IssueLogs` directories per worker at the 50 most-recently-modified (by max `ModTime` across a dir's files), evicting the rest on ingest (design's "Server storage" section).

---

## File Structure

- `shared/wire.go` — new `IssueLogFile`, `IssueLogDir` types; `PushRequest.IssueLogs` field.
- `worker/telemetry_exporter.go` — scans `<WorkDir>/logs/*` each push cycle and attaches the result as `IssueLogs`.
- `telemetry-server/server.go` — `WorkerState.IssueLogs` (`map[string]shared.IssueLogDir`), ingest-time wholesale replace + eviction cap.
- `telemetry-server/web.go` — view models and handlers for `GET /` (index) and `GET /workers/{machineID}` (worker detail + persisted-logs browser); `/rail` and `/detail` removed.
- `telemetry-server/templates/page.html` — becomes the shared `<head>`/nav shell plus the two full-page wrappers (`tindexPage`, `tworkerPage`).
- `telemetry-server/templates/rail.html` — deleted.
- `telemetry-server/templates/index.html` — new file: the 6-column grid content fragment (`tindex`), replacing `rail.html`'s content.
- `telemetry-server/templates/detail.html` — becomes the worker-detail content fragment (`tdetail`): usage stats, log tail, persisted-logs tree + file viewer.
- `telemetry-server/web_test.go` — routes re-pointed at `/` and `/workers/{id}`; new tests for ordering, not-found, and the log browser.
- `telemetry-server/tailwind.css` (source, unchanged) / `telemetry-server/static/app.css` (regenerated) — new classes (`grid-cols-6`, the log-tree layout) need to be present in the built CSS.

---

### Task 1: Wire format — `IssueLogFile`, `IssueLogDir`, `PushRequest.IssueLogs`

**Files:**
- Modify: `shared/wire.go`
- Test: `shared/wire_test.go`

**Interfaces:**
- Produces: `shared.IssueLogFile{Name, Content string; ModTime time.Time}`, `shared.IssueLogDir{Name string; Files []IssueLogFile}`, `shared.PushRequest.IssueLogs []IssueLogDir` (json tag `"issueLogs"`) — consumed by Tasks 2 and 3.

- [ ] **Step 1: Write the failing test**

Add to `shared/wire_test.go`:

```go
func TestPushRequestIssueLogsRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	want := PushRequest{
		Resource: Resource{RepoSlug: "o/r", MachineID: "abc123def456"},
		IssueLogs: []IssueLogDir{
			{
				Name: "issue-42",
				Files: []IssueLogFile{
					{Name: "003-answer-1.output.md", Content: "hello", ModTime: now},
				},
			},
			{Name: "triage", Files: []IssueLogFile{}},
		},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got PushRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.IssueLogs) != 2 {
		t.Fatalf("issueLogs round-trip = %+v", got.IssueLogs)
	}
	if got.IssueLogs[0].Name != "issue-42" || len(got.IssueLogs[0].Files) != 1 ||
		got.IssueLogs[0].Files[0].Name != "003-answer-1.output.md" ||
		got.IssueLogs[0].Files[0].Content != "hello" ||
		!got.IssueLogs[0].Files[0].ModTime.Equal(now) {
		t.Fatalf("issueLogs[0] round-trip = %+v", got.IssueLogs[0])
	}
	if got.IssueLogs[1].Name != "triage" || len(got.IssueLogs[1].Files) != 0 {
		t.Fatalf("issueLogs[1] round-trip = %+v", got.IssueLogs[1])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd shared && go test ./... -run TestPushRequestIssueLogsRoundTrip -v`
Expected: FAIL with a compile error (`IssueLogDir`/`IssueLogFile` undefined, `PushRequest` has no field `IssueLogs`).

- [ ] **Step 3: Add the types and field**

In `shared/wire.go`, add after `UsageSnapshot` and before `PushRequest`:

```go
// IssueLogFile is one persisted file from a worker's logs/<dir> tree.
type IssueLogFile struct {
	Name    string    `json:"name"`    // e.g. "003-answer-1.output.md", "state", "session"
	Content string    `json:"content"`
	ModTime time.Time `json:"modTime"`
}

// IssueLogDir is one directory under <WorkDir>/logs — one issue's pipeline
// run ("issue-42") or the shared "triage" dir.
type IssueLogDir struct {
	Name  string         `json:"name"` // dir name as on disk: "issue-42", "triage"
	Files []IssueLogFile `json:"files"`
}
```

Then add the field to `PushRequest`:

```go
type PushRequest struct {
	Resource  Resource       `json:"resource"`
	Logs      []LogRecord    `json:"logs"`
	Usage     *UsageSnapshot `json:"usage"`
	IssueLogs []IssueLogDir  `json:"issueLogs"`
	SentAt    time.Time      `json:"sentAt"`
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd shared && go test ./... -v`
Expected: PASS (all tests, including the pre-existing round-trip tests, which are unaffected by the new field).

- [ ] **Step 5: Commit**

```bash
git add shared/wire.go shared/wire_test.go
git commit -m "feat: add IssueLogDir/IssueLogFile to the telemetry push wire format"
```

---

### Task 2: Worker exporter — scan and attach `logs/*` each push

**Files:**
- Modify: `worker/telemetry_exporter.go`
- Test: `worker/telemetry_exporter_test.go`

**Interfaces:**
- Consumes: `shared.IssueLogDir`, `shared.IssueLogFile` (Task 1).
- Produces: `scanIssueLogs(workDir string) ([]shared.IssueLogDir, error)` — a package-level function in `worker`, called from `pushOnce`. `TelemetryExporter.pushOnce` now sets `req.IssueLogs`.

- [ ] **Step 1: Write the failing tests**

Add to `worker/telemetry_exporter_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd worker && go test ./... -run 'TestScanIssueLogs|TestExporterPushOnceIncludesIssueLogs' -v`
Expected: FAIL — `scanIssueLogs` undefined for the first two; the third fails because `gotReq.IssueLogs` is empty (field exists from Task 1 but nothing populates it yet).

- [ ] **Step 3: Implement the scan and wire it into `pushOnce`**

In `worker/telemetry_exporter.go`, add after `readUsageSnapshot`:

```go
// scanIssueLogs reads workDir/logs and returns one IssueLogDir per
// subdirectory (each issue's pipeline run, plus the shared "triage" dir),
// carrying the full contents of every regular file directly inside. This is
// a full re-read and re-send every cycle (design decision 1) — these files
// are small (prompts/outputs/postmortems for one Claude call, or single-line
// state/pr/session files) — and the scan is non-recursive: the existing log
// writers in tracker.go/claude.go never nest subdirectories. A missing or
// empty logs dir yields an empty slice, never nil, so the field always
// marshals as a JSON array rather than null.
func scanIssueLogs(workDir string) ([]shared.IssueLogDir, error) {
	logsDir := filepath.Join(workDir, "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []shared.IssueLogDir{}, nil
		}
		return nil, err
	}
	dirs := []shared.IssueLogDir{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := scanIssueLogFiles(filepath.Join(logsDir, e.Name()))
		if err != nil {
			log.Printf("telemetry: scan logs/%s: %v", e.Name(), err)
			continue
		}
		dirs = append(dirs, shared.IssueLogDir{Name: e.Name(), Files: files})
	}
	return dirs, nil
}

// scanIssueLogFiles reads every regular file directly inside dir (no
// recursion) into an IssueLogFile.
func scanIssueLogFiles(dir string) ([]shared.IssueLogFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := []shared.IssueLogFile{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		files = append(files, shared.IssueLogFile{Name: e.Name(), Content: string(content), ModTime: info.ModTime()})
	}
	return files, nil
}
```

Add `"path/filepath"` to the import block if not already present (it is not — check the current imports and add it alongside the existing `"os"`).

In `pushOnce`, after the `usage, err := readUsageSnapshot(...)` block, add:

```go
	issueLogs, err := scanIssueLogs(e.cfg.WorkDir)
	if err != nil {
		log.Printf("telemetry: scan issue logs: %v", err)
	}
```

And add `IssueLogs: issueLogs,` to the `shared.PushRequest{...}` literal, alongside `Logs:` and `Usage:`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd worker && go test ./... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add worker/telemetry_exporter.go worker/telemetry_exporter_test.go
git commit -m "feat: scan and push a worker's persisted issue logs each cycle"
```

---

### Task 3: Server storage — `WorkerState.IssueLogs`, ingest replace + eviction cap

**Files:**
- Modify: `telemetry-server/server.go`
- Test: `telemetry-server/server_test.go`

**Interfaces:**
- Consumes: `shared.IssueLogDir` (Task 1).
- Produces: `WorkerState.IssueLogs map[string]shared.IssueLogDir` (keyed by dir name) — consumed by Task 6's view-model builder. `dirModTime(d shared.IssueLogDir) time.Time` and `evictIssueLogs(m map[string]shared.IssueLogDir, cap int) map[string]shared.IssueLogDir` — package-level helpers, reused by Task 6 for tree ordering.

- [ ] **Step 1: Write the failing tests**

Add to `telemetry-server/server_test.go`:

```go
func TestHandlePushReplacesIssueLogsWholesale(t *testing.T) {
	s, err := NewTelemetryServer("secret")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
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
```

Add `"fmt"` to the test file's imports if not already present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd telemetry-server && go test ./... -run 'TestHandlePushReplacesIssueLogsWholesale|TestHandlePushEvictsLeastRecentlyModifiedIssueLogDirsOverCap' -v`
Expected: FAIL — `WorkerState` has no field `IssueLogs`.

- [ ] **Step 3: Implement storage, replace, and eviction**

In `telemetry-server/server.go`, add the `"sort"` import, then add the cap constant near `maxLogLinesPerWorker`:

```go
// maxIssueLogDirsPerWorker caps how many logs/<dir> entries the server keeps
// per worker, evicting the least-recently-modified ones on ingest — mirrors
// maxLogLinesPerWorker's role for the daemon-log ring buffer, just scoped to
// directories instead of lines.
const maxIssueLogDirsPerWorker = 50
```

Add the field to `WorkerState`:

```go
type WorkerState struct {
	Resource   shared.Resource
	LastPushAt time.Time
	Usage      *shared.UsageSnapshot
	Logs       *LogRingBuffer
	IssueLogs  map[string]shared.IssueLogDir // keyed by dir name, replaced wholesale each push
}
```

Add two package-level helpers (near the bottom of the file, or right after `handlePush`):

```go
// dirModTime returns the latest ModTime across a dir's files — used both to
// order the persisted-logs browser (newest first, Task 6) and to pick
// eviction candidates (oldest first, below).
func dirModTime(d shared.IssueLogDir) time.Time {
	var latest time.Time
	for _, f := range d.Files {
		if f.ModTime.After(latest) {
			latest = f.ModTime
		}
	}
	return latest
}

// evictIssueLogs drops the least-recently-modified directories when m holds
// more than max entries, keeping memory bounded on a long-running repo (the
// #57 design's live, in-memory view — no DB, no persistence across
// restarts). Returns m unchanged when it is already within the cap.
func evictIssueLogs(m map[string]shared.IssueLogDir, max int) map[string]shared.IssueLogDir {
	if len(m) <= max {
		return m
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return dirModTime(m[names[i]]).After(dirModTime(m[names[j]])) })
	kept := make(map[string]shared.IssueLogDir, max)
	for _, name := range names[:max] {
		kept[name] = m[name]
	}
	return kept
}
```

In `handlePush`, after `ws.Usage = req.Usage`, add:

```go
	issueLogs := make(map[string]shared.IssueLogDir, len(req.IssueLogs))
	for _, d := range req.IssueLogs {
		issueLogs[d.Name] = d
	}
	ws.IssueLogs = evictIssueLogs(issueLogs, maxIssueLogDirsPerWorker)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd telemetry-server && go test ./... -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add telemetry-server/server.go telemetry-server/server_test.go
git commit -m "feat: store and cap workers' persisted issue logs on ingest"
```

---

### Task 4: Full-width index page (6-column grid, online-before-offline)

**Files:**
- Modify: `telemetry-server/web.go`
- Modify: `telemetry-server/templates/page.html`
- Create: `telemetry-server/templates/index.html`
- Delete: `telemetry-server/templates/rail.html`
- Test: `telemetry-server/web_test.go`

**Interfaces:**
- Consumes: `WorkerState`, `buildTelemetryWorkerView` (existing, unchanged in this task).
- Produces: `telemetryIndexView{Groups []telemetryGroup}`, `(s *TelemetryServer) buildTelemetryIndexView() telemetryIndexView`, template names `"thead"`, `"tnav"`, `"tindexPage"` (page.html), `"tindex"` (index.html) — `tindexPage`/`tindex` consumed by `handleTelemetryIndex`; `thead`/`tnav` reused by Task 5's `tworkerPage`.

- [ ] **Step 1: Write the failing test**

Add to `telemetry-server/web_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd telemetry-server && go test ./... -run TestTelemetryIndexOrdersOnlineBeforeOfflineWithinRepoSlug -v`
Expected: FAIL — the current `buildTelemetryView` sorts by hostname only, so `m-aaa` (hostname `aaa-host`) renders before `m-zzz` (hostname `zzz-host`), the opposite of what the test wants.

- [ ] **Step 3: Add the index-only view model and handler**

In `telemetry-server/web.go`, add above `registerWebHandlers`:

```go
// telemetryIndexView is the template payload for GET /.
type telemetryIndexView struct {
	Groups []telemetryGroup
}

// buildTelemetryIndexView groups all workers by RepoSlug (sorted), and
// within each group sorts online workers before offline, both buckets by
// hostname (design's "Routing and pages" section).
func (s *TelemetryServer) buildTelemetryIndexView() telemetryIndexView {
	now := s.now()
	s.mu.Lock()
	byRepo := map[string][]telemetryWorkerView{}
	for _, ws := range s.workers {
		byRepo[ws.Resource.RepoSlug] = append(byRepo[ws.Resource.RepoSlug], buildTelemetryWorkerView(ws, now))
	}
	s.mu.Unlock()

	var slugs []string
	for slug := range byRepo {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	view := telemetryIndexView{}
	for _, slug := range slugs {
		workers := byRepo[slug]
		sort.Slice(workers, func(i, j int) bool {
			if workers[i].Online != workers[j].Online {
				return workers[i].Online // online sorts before offline
			}
			return workers[i].Hostname < workers[j].Hostname
		})
		view.Groups = append(view.Groups, telemetryGroup{RepoSlug: slug, Workers: workers})
	}
	return view
}
```

Replace `registerWebHandlers` and `handleTelemetryIndex`/`handleTelemetryRail` with:

```go
// registerWebHandlers wires the dashboard routes onto mux: GET / (index
// page), GET /workers/{machineID} (worker detail page), and the embedded
// static assets (staticHandler, static.go).
func (s *TelemetryServer) registerWebHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.handleTelemetryIndex)
	mux.HandleFunc("GET /detail", s.handleTelemetryDetail) // Task 5 removes this
	mux.Handle("GET /static/", staticHandler())
}

// handleTelemetryIndex serves the full-width index page. A poll from the
// page's own #content element (identified by the HX-Request header htmx
// sets) gets just the grid fragment; any other request gets the full
// document.
func (s *TelemetryServer) handleTelemetryIndex(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryIndexView()
	if r.Header.Get("HX-Request") == "true" {
		renderTelemetryHTML(w, s.tmpl, "tindex", v)
		return
	}
	renderTelemetryHTML(w, s.tmpl, "tindexPage", v)
}
```

Leave `handleTelemetryDetail` and `buildTelemetryView`/`telemetryView` exactly as they are for now — Task 5 replaces them. Remove only `handleTelemetryRail` (its route and body).

- [ ] **Step 4: Rewrite `page.html` as the shared shell + index page**

Replace the contents of `telemetry-server/templates/page.html` with:

```html
{{define "thead"}}<head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>loope // fleet telemetry</title>
<link rel="stylesheet" href="/static/app.css">
<script src="/static/htmx.min.js"></script>
<script src="/static/idiomorph-ext.min.js"></script>
</head>{{end}}

{{define "tnav"}}<header class="flex shrink-0 items-center gap-3 border-b border-line bg-panel px-5 py-3">
 <a href="/" class="font-mono text-[15px] font-semibold tracking-tight text-text">loope<span class="text-live">&middot;</span>fleet</a>
</header>{{end}}

{{define "tindexPage"}}<!doctype html>
<html lang="en">{{template "thead" .}}
<body class="font-sans text-text antialiased" hx-ext="morph">
<div class="flex h-screen flex-col">
{{template "tnav" .}}
<div id="content" class="scroll min-w-0 flex-1 overflow-y-auto"
     hx-get="/" hx-trigger="every 3s" hx-swap="morph:innerHTML">{{template "tindex" .}}</div>
</div>
</body></html>{{end}}
```

- [ ] **Step 5: Create `index.html` with the 6-column grid**

Create `telemetry-server/templates/index.html` (this is `rail.html`'s card markup unchanged, just re-laid-out into a `grid-cols-6` grid per repoSlug section, and cards become plain links instead of htmx swap targets):

```html
{{define "tindex"}}<div class="p-6">
{{range .Groups}}
 <div class="mb-6">
  <div class="mb-2 px-1 font-mono text-[11px] font-semibold uppercase tracking-[0.15em] text-faint">{{.RepoSlug}}</div>
  <div class="grid grid-cols-6 gap-3">
  {{range .Workers}}
   <a href="/workers/{{.MachineID}}"
      class="block overflow-hidden rounded-md border border-line2 bg-panel font-mono text-text hover:border-live/40">
    <div class="flex h-12 items-center justify-center border-b border-line2 bg-panel2">
     <svg class="h-5 w-5 text-faint" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" aria-hidden="true">
      <rect x="3" y="4" width="18" height="6" rx="1.5"></rect>
      <rect x="3" y="14" width="18" height="6" rx="1.5"></rect>
      <circle cx="7" cy="7" r="0.75" fill="currentColor" stroke="none"></circle>
      <circle cx="7" cy="17" r="0.75" fill="currentColor" stroke="none"></circle>
     </svg>
    </div>
    <div class="px-2.5 py-2">
     <div class="mb-1 flex items-center gap-1.5">
      <span class="inline-block h-2 w-2 shrink-0 rounded-full {{if .Online}}bg-ok{{else}}bg-muted{{end}}"></span>
      <span class="truncate text-[11px] font-semibold">{{.MachineID}}</span>
     </div>
     {{if .UsageUnknown}}<div class="text-[11px] text-faint">usage: unknown</div>
     {{else}}<div class="tabular-nums text-[11px] text-faint">5h: {{printf "%.0f%%" .FiveHourPct}}&nbsp;&nbsp;7d: {{printf "%.0f%%" .SevenDayPct}}</div>{{end}}
    </div>
   </a>
  {{end}}
  </div>
 </div>
{{else}}
 <div class="p-4 font-mono text-[12px] text-faint">No workers have reported yet.</div>
{{end}}
</div>{{end}}
```

- [ ] **Step 6: Delete `rail.html`**

```bash
git rm telemetry-server/templates/rail.html
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `cd telemetry-server && go test ./... -v`
Expected: PASS, including `TestTelemetryIndexGroupsByRepoSlugAndShowsWorkers` (unchanged assertions) and the new ordering test. `TestAppCSSCoversTemplateClasses` is expected to still FAIL at this point (it checks `w-[420px]`, which no longer exists) — Task 7 fixes it; do not fix it here.

- [ ] **Step 8: Commit**

```bash
git add telemetry-server/web.go telemetry-server/templates/page.html telemetry-server/templates/index.html telemetry-server/templates/rail.html telemetry-server/web_test.go
git commit -m "feat: replace the telemetry rail with a full-width 6-column index page"
```

---

### Task 5: Worker detail page (`GET /workers/{machineID}`, not-found state)

**Files:**
- Modify: `telemetry-server/web.go`
- Modify: `telemetry-server/templates/detail.html`
- Test: `telemetry-server/web_test.go`

**Interfaces:**
- Consumes: `"thead"`, `"tnav"` (Task 4's `page.html`), `buildTelemetryWorkerView` (existing).
- Produces: `telemetryWorkerPageView{MachineID string; Worker *telemetryWorkerView}`, `(s *TelemetryServer) buildTelemetryWorkerPageView(machineID string) telemetryWorkerPageView`, template names `"tworkerPage"` (added to page.html), `"tdetail"` (rewritten in detail.html) — `tdetail` extended by Task 6 to add the log tree/file viewer, `telemetryWorkerPageView` extended by Task 6 with `SelectedDir`/`SelectedFile`/`FileContent`/`FileNotFound`.

- [ ] **Step 1: Write the failing tests**

In `telemetry-server/web_test.go`, replace the URL in the three existing `/detail?worker=m1` tests with `/workers/m1`:

```go
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
```

Delete `TestTelemetryDetailShowsUsageUnknownWhenAbsent`, `TestTelemetryDetailShowsFreshUsagePercentage`, and `TestTelemetryDetailShowsLogTail` (the three tests above replace them).

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd telemetry-server && go test ./... -run TestTelemetryWorker -v`
Expected: FAIL — `GET /workers/m1` 404s (no such route yet).

- [ ] **Step 3: Add the worker-page view model and handler, remove `/detail`**

In `telemetry-server/web.go`, delete `telemetryView`, `buildTelemetryView`, and `handleTelemetryDetail`. Add in their place:

```go
// telemetryWorkerPageView is the template payload for GET /workers/{id}.
// Worker is nil when machineID matches no known worker (e.g. an offline
// worker evicted after a server restart) — the template then renders a
// "not found" state instead of erroring.
type telemetryWorkerPageView struct {
	MachineID string
	Worker    *telemetryWorkerView
}

// buildTelemetryWorkerPageView looks up machineID and renders its current
// state, or returns a Worker-less view when it has no match.
func (s *TelemetryServer) buildTelemetryWorkerPageView(machineID string) telemetryWorkerPageView {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.workers[machineID]
	if ws == nil {
		return telemetryWorkerPageView{MachineID: machineID}
	}
	v := buildTelemetryWorkerView(ws, now)
	return telemetryWorkerPageView{MachineID: machineID, Worker: &v}
}
```

Replace `registerWebHandlers`'s `/detail` line with `/workers/{machineID}`, and add the handler:

```go
func (s *TelemetryServer) registerWebHandlers(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.handleTelemetryIndex)
	mux.HandleFunc("GET /workers/{machineID}", s.handleTelemetryWorker)
	mux.Handle("GET /static/", staticHandler())
}
```

```go
// handleTelemetryWorker serves the worker detail page. Like the index page,
// a poll from the page's own #content element gets just the content
// fragment; a plain navigation gets the full document.
func (s *TelemetryServer) handleTelemetryWorker(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryWorkerPageView(r.PathValue("machineID"))
	if r.Header.Get("HX-Request") == "true" {
		renderTelemetryHTML(w, s.tmpl, "tdetail", v)
		return
	}
	renderTelemetryHTML(w, s.tmpl, "tworkerPage", v)
}
```

- [ ] **Step 4: Add the `tworkerPage` wrapper to `page.html`**

Append to `telemetry-server/templates/page.html`:

```html

{{define "tworkerPage"}}<!doctype html>
<html lang="en">{{template "thead" .}}
<body class="font-sans text-text antialiased" hx-ext="morph">
<div class="flex h-screen flex-col">
{{template "tnav" .}}
<div id="content" class="scroll min-w-0 flex-1 overflow-y-auto"
     hx-get="/workers/{{.MachineID}}" hx-trigger="every 3s" hx-swap="morph:innerHTML">{{template "tdetail" .}}</div>
</div>
</body></html>{{end}}
```

- [ ] **Step 5: Rewrite `detail.html`'s `tdetail` for the new `.Worker`-wrapped shape and add the not-found state**

Replace the contents of `telemetry-server/templates/detail.html` with:

```html
{{define "tdetail"}}<div class="max-w-[1000px] px-8 py-6">
{{with .Worker}}
 <div class="mb-4 flex items-center gap-2">
  <span class="inline-block h-2.5 w-2.5 rounded-full {{if .Online}}bg-ok{{else}}bg-muted{{end}}"></span>
  <h1 class="font-mono text-lg font-semibold text-text">{{.Hostname}}</h1>
  <span class="font-mono text-[11px] text-faint">{{.WorkDir}}</span>
  <span class="ml-auto font-mono text-[11px] text-faint">v{{.Version}}</span>
 </div>
 <dl class="mb-5 grid grid-cols-2 gap-px overflow-hidden rounded-md border border-line bg-line">
  <div class="bg-panel px-4 py-3">
   <dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">5-hour usage</dt>
   {{if .UsageUnknown}}<dd class="mt-1.5 font-mono text-sm text-faint">unknown</dd>
   {{else}}<dd class="mt-1.5 font-mono text-xl font-semibold tabular-nums text-text">{{printf "%.1f%%" .FiveHourPct}} <span class="text-[11px] font-normal text-faint">resets {{.FiveHourReset.Format "Jan 2 15:04"}}</span></dd>{{end}}
  </div>
  <div class="bg-panel px-4 py-3">
   <dt class="font-mono text-[10px] uppercase tracking-[0.15em] text-faint">7-day usage</dt>
   {{if .UsageUnknown}}<dd class="mt-1.5 font-mono text-sm text-faint">unknown</dd>
   {{else}}<dd class="mt-1.5 font-mono text-xl font-semibold tabular-nums text-text">{{printf "%.1f%%" .SevenDayPct}} <span class="text-[11px] font-normal text-faint">resets {{.SevenDayReset.Format "Jan 2 15:04"}}</span></dd>{{end}}
  </div>
 </dl>
 <div class="mb-5 rounded-md border border-line bg-ink/40 p-3 font-mono text-[11px] leading-relaxed text-muted">
  {{range .Logs}}<div>{{.}}</div>{{else}}<div class="text-faint">No log lines yet.</div>{{end}}
 </div>
{{else}}<div class="flex h-full items-center justify-center py-20 font-mono text-[12px] text-faint">Worker not found.</div>{{end}}
</div>{{end}}
```

(This is the prior `tdetail` content, unchanged except wrapped in `{{with .Worker}}` and given a "Worker not found." `{{else}}` branch. Task 6 adds the persisted-logs section right after the log-tail `</div>` and before the `{{else}}`.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd telemetry-server && go test ./... -v`
Expected: PASS for all `TestTelemetryWorker*` tests. `TestAppCSSCoversTemplateClasses` still fails (deferred to Task 7).

- [ ] **Step 7: Commit**

```bash
git add telemetry-server/web.go telemetry-server/templates/page.html telemetry-server/templates/detail.html telemetry-server/web_test.go
git commit -m "feat: give each worker a real /workers/{machineID} detail page"
```

---

### Task 6: Persisted-logs browser (tree + file viewer)

**Files:**
- Modify: `telemetry-server/web.go`
- Modify: `telemetry-server/templates/detail.html`
- Test: `telemetry-server/web_test.go`

**Interfaces:**
- Consumes: `WorkerState.IssueLogs` (Task 3), `dirModTime` (Task 3), `telemetryWorkerPageView`/`buildTelemetryWorkerPageView` (Task 5).
- Produces: `telemetryWorkerView.IssueLogs []issueLogDirView`, `issueLogDirView{Name, DisplayName string; Files []issueLogFileView}`, `issueLogFileView{Name string}`; `telemetryWorkerPageView` gains `SelectedDir, SelectedFile, FileContent string; FileNotFound bool`.

- [ ] **Step 1: Write the failing tests**

Add to `telemetry-server/web_test.go`:

```go
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
	if !strings.Contains(rec.Body.String(), "\"a\": 1") {
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd telemetry-server && go test ./... -run TestTelemetryWorker -v`
Expected: FAIL — no log-tree markup rendered yet, `?dir=`/`?file=` query params ignored.

- [ ] **Step 3: Add the tree view model and sorting helpers**

In `telemetry-server/web.go`, add `"strconv"` and `"strings"` (already imported) plus `"bytes"` (already imported) to the import block if missing, then add:

```go
// issueLogFileView is one file entry in the persisted-logs tree.
type issueLogFileView struct {
	Name string
}

// issueLogDirView is one directory entry in the persisted-logs tree,
// pre-sorted and carrying its display label.
type issueLogDirView struct {
	Name        string
	DisplayName string
	Files       []issueLogFileView
}

// issueNumber extracts N from an "issue-N" directory name.
func issueNumber(name string) (int, bool) {
	if !strings.HasPrefix(name, "issue-") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(name, "issue-"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// issueLogDisplayName renders "issue-42" as "Issue 42"; anything else
// (namely "triage") displays as-is, per the design doc.
func issueLogDisplayName(name string) string {
	if n, ok := issueNumber(name); ok {
		return fmt.Sprintf("Issue %d", n)
	}
	return name
}

// buildIssueLogDirViews converts a worker's raw IssueLogs into the sorted
// tree the template renders: numeric "issue-N" dirs (newest-modified first)
// before the shared "triage" dir, each with its files listed by name.
func buildIssueLogDirViews(dirs map[string]shared.IssueLogDir) []issueLogDirView {
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		_, iIsIssue := issueNumber(names[i])
		_, jIsIssue := issueNumber(names[j])
		if iIsIssue != jIsIssue {
			return iIsIssue // numeric issue dirs sort before non-numeric ("triage")
		}
		return dirModTime(dirs[names[i]]).After(dirModTime(dirs[names[j]]))
	})
	views := make([]issueLogDirView, 0, len(names))
	for _, name := range names {
		d := dirs[name]
		files := make([]issueLogFileView, 0, len(d.Files))
		for _, f := range d.Files {
			files = append(files, issueLogFileView{Name: f.Name})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
		views = append(views, issueLogDirView{Name: name, DisplayName: issueLogDisplayName(name), Files: files})
	}
	return views
}

// findIssueLogFileContent looks up one file's raw content within dirs.
func findIssueLogFileContent(dirs map[string]shared.IssueLogDir, dirName, fileName string) (string, bool) {
	d, ok := dirs[dirName]
	if !ok {
		return "", false
	}
	for _, f := range d.Files {
		if f.Name == fileName {
			return f.Content, true
		}
	}
	return "", false
}

// formatIssueLogFileContent pretty-prints .json files server-side (Claude's
// saveLog writes raw API responses); everything else renders as-is —
// syntax highlighting is explicitly out of scope (design doc).
func formatIssueLogFileContent(name, content string) string {
	if !strings.HasSuffix(name, ".json") {
		return content
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, []byte(content), "", "  "); err != nil {
		return content
	}
	return pretty.String()
}
```

Add `"fmt"` to the import block (used by `issueLogDisplayName`).

Update `telemetryWorkerView` to add the field:

```go
type telemetryWorkerView struct {
	MachineID     string
	Hostname      string
	WorkDir       string
	Version       string
	Online        bool
	LastPushAt    time.Time
	UsageUnknown  bool
	FiveHourPct   float64
	FiveHourReset time.Time
	SevenDayPct   float64
	SevenDayReset time.Time
	Logs          []string
	IssueLogs     []issueLogDirView
}
```

In `buildTelemetryWorkerView`, after `Logs: ws.Logs.Lines(),` add `IssueLogs: buildIssueLogDirViews(ws.IssueLogs),`.

- [ ] **Step 4: Extend the worker-page view model and handler for `?dir=`/`?file=`**

Replace `telemetryWorkerPageView` and `buildTelemetryWorkerPageView` in `telemetry-server/web.go`:

```go
// telemetryWorkerPageView is the template payload for GET /workers/{id}.
// Worker is nil when machineID matches no known worker. SelectedFile is
// non-empty when a specific log file is being viewed; FileNotFound is set
// when the requested dir/file no longer exists on the worker's latest push.
type telemetryWorkerPageView struct {
	MachineID    string
	Worker       *telemetryWorkerView
	SelectedDir  string
	SelectedFile string
	FileContent  string
	FileNotFound bool
}

// buildTelemetryWorkerPageView looks up machineID and renders its current
// state, optionally resolving the file named by dir/file for the log
// viewer pane. dir and file are both empty when no file is selected.
func (s *TelemetryServer) buildTelemetryWorkerPageView(machineID, dir, file string) telemetryWorkerPageView {
	now := s.now()
	s.mu.Lock()
	ws := s.workers[machineID]
	if ws == nil {
		s.mu.Unlock()
		return telemetryWorkerPageView{MachineID: machineID}
	}
	wv := buildTelemetryWorkerView(ws, now)
	var content string
	var found bool
	if dir != "" && file != "" {
		content, found = findIssueLogFileContent(ws.IssueLogs, dir, file)
	}
	s.mu.Unlock()

	v := telemetryWorkerPageView{MachineID: machineID, Worker: &wv, SelectedDir: dir, SelectedFile: file}
	if dir != "" && file != "" {
		if !found {
			v.FileNotFound = true
		} else {
			v.FileContent = formatIssueLogFileContent(file, content)
		}
	}
	return v
}
```

Update `handleTelemetryWorker` to pass the query params through:

```go
func (s *TelemetryServer) handleTelemetryWorker(w http.ResponseWriter, r *http.Request) {
	v := s.buildTelemetryWorkerPageView(r.PathValue("machineID"), r.URL.Query().Get("dir"), r.URL.Query().Get("file"))
	if r.Header.Get("HX-Request") == "true" {
		renderTelemetryHTML(w, s.tmpl, "tdetail", v)
		return
	}
	renderTelemetryHTML(w, s.tmpl, "tworkerPage", v)
}
```

- [ ] **Step 5: Add the tree + file-viewer markup to `detail.html`**

In `telemetry-server/templates/detail.html`, insert the following between the log-tail `</div>` and the `{{else}}` of the `{{with .Worker}}` block:

```html
 <div class="grid grid-cols-[280px_1fr] gap-4">
  <div class="rounded-md border border-line bg-panel p-2">
  {{range .IssueLogs}}
   <details class="mb-1" {{if eq $.SelectedDir .Name}}open{{end}}>
    <summary class="flex cursor-pointer items-center gap-1 rounded px-1.5 py-1 font-mono text-[11px] font-semibold text-text hover:bg-panel2">
     <svg class="chev h-3 w-3 shrink-0 text-faint transition-transform" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M9 6l6 6-6 6"/></svg>
     {{.DisplayName}}
    </summary>
    <div class="ml-4 border-l border-line2 pl-2">
    {{$dirName := .Name}}
    {{range .Files}}
     <a href="/workers/{{$.MachineID}}?dir={{$dirName}}&file={{.Name}}"
        class="block truncate rounded px-1.5 py-0.5 font-mono text-[11px] {{if and (eq $.SelectedDir $dirName) (eq $.SelectedFile .Name)}}bg-live/10 text-live{{else}}text-muted hover:bg-panel2{{end}}">{{.Name}}</a>
    {{end}}
    </div>
   </details>
  {{else}}
   <div class="p-2 font-mono text-[11px] text-faint">No persisted logs yet.</div>
  {{end}}
  </div>
  <div class="min-w-0 rounded-md border border-line bg-ink/40 p-3">
  {{if $.FileNotFound}}<div class="font-mono text-[11px] text-faint">File not found.</div>
  {{else if $.SelectedFile}}<pre class="scroll overflow-auto whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed text-muted">{{$.FileContent}}</pre>
  {{else}}<div class="font-mono text-[11px] text-faint">Select a file to view its contents.</div>{{end}}
  </div>
 </div>
```

This stays inside the same `{{define "tdetail"}}...{{with .Worker}}...{{end}}{{end}}` template invocation (no nested `{{template}}` call), so `$` correctly refers to the root `telemetryWorkerPageView` throughout (`$.MachineID`, `$.SelectedDir`, `$.SelectedFile`, `$.FileContent`, `$.FileNotFound`), while `.` inside the `{{range .IssueLogs}}`/`{{range .Files}}` blocks refers to the current `issueLogDirView`/`issueLogFileView`.

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd telemetry-server && go test ./... -v`
Expected: PASS for all tests except `TestAppCSSCoversTemplateClasses` (Task 7).

- [ ] **Step 7: Commit**

```bash
git add telemetry-server/web.go telemetry-server/templates/detail.html telemetry-server/web_test.go
git commit -m "feat: add the persisted-logs tree and file viewer to the worker page"
```

---

### Task 7: Regenerate Tailwind CSS and run full verification

**Files:**
- Modify: `telemetry-server/static/app.css` (regenerated, not hand-edited)
- Modify: `telemetry-server/web_test.go`

**Interfaces:**
- Consumes: all prior tasks' template changes.
- Produces: nothing new — this task closes out `TestAppCSSCoversTemplateClasses` and runs the full three-module test/build matrix.

- [ ] **Step 1: Update the CSS-coverage test's expected classes**

In `telemetry-server/web_test.go`, in `TestAppCSSCoversTemplateClasses`, replace the `w-[420px]` entry (the old `#rail` width, now gone) and add the new grid/tree classes:

```go
	for _, want := range []string{
		`bg-muted`,               // status dot (offline) — templates/index.html, templates/detail.html
		`grid-cols-6`,            // index page card grid — templates/index.html
		`grid-cols-2`,            // usage stats grid — templates/detail.html
		`grid-cols-\[280px_1fr\]`, // persisted-logs tree/viewer split — templates/detail.html
	} {
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd telemetry-server && go test ./... -run TestAppCSSCoversTemplateClasses -v`
Expected: FAIL — `static/app.css` was not regenerated for this branch's new template classes, so `grid-cols-6` and the bracketed grid-template-columns class are missing.

- [ ] **Step 3: Regenerate `static/app.css`**

From `telemetry-server/`, run:

```bash
npx @tailwindcss/cli@latest -i tailwind.css -o static/app.css --minify
```

If that package name is unavailable in the execution environment, fall back to:

```bash
npx tailwindcss@latest -i tailwind.css -o static/app.css --minify
```

**Assumption:** neither `tailwindcss` nor `npx` package resolution was available when this plan was written (no network access to the npm registry was verified in this environment). If both commands fail for the same reason in the implementation environment, install the CLI locally first (`npm install --no-save @tailwindcss/cli`) or run the build in an environment with registry access, then re-run the command above. Do not hand-edit `static/app.css` outside of this generated step — it is a committed build artifact, and hand-edits will drift from `tailwind.css` on the next real regeneration.

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd telemetry-server && go test ./... -run TestAppCSSCoversTemplateClasses -v`
Expected: PASS.

- [ ] **Step 5: Run the full test suite across all three modules**

```bash
cd shared && go build ./... && go vet ./... && go test ./...
cd ../worker && go build ./... && go vet ./... && go test ./...
cd ../telemetry-server && go build ./... && go vet ./... && go test ./...
```

Expected: all PASS, no vet warnings.

- [ ] **Step 6: Commit**

```bash
git add telemetry-server/static/app.css telemetry-server/web_test.go
git commit -m "chore: regenerate telemetry dashboard CSS for the new grid/tree classes"
```

---

## Self-Review Notes

- **Spec coverage:** full-width index with 6-col grid + online-before-offline ordering → Task 4; real per-worker page navigation → Task 5; persisted per-issue log collection over the existing push → Tasks 1–3; log tree + file viewer with JSON pretty-print → Task 6; `/rail`/`/detail` removal → Tasks 4–5; CSS/test housekeeping → Task 7. All "Testing" bullets from the spec map onto a task's test steps above (wire round-trip: Task 1; exporter scan: Task 2; server replace/eviction: Task 3; index ordering: Task 4; not-found + log tail: Task 5; tree ordering/file viewer/not-found: Task 6; re-pointed substring assertions: Tasks 4–6).
- **Type consistency:** `telemetryWorkerView.IssueLogs` (Task 6) matches `buildIssueLogDirViews`'s return type `[]issueLogDirView`; `telemetryWorkerPageView` fields introduced in Task 5 (`MachineID`, `Worker`) are only ever extended (Task 6), never renamed, so `page.html`'s `tworkerPage` (`.MachineID`) written in Task 5 keeps working after Task 6.
- **Out-of-scope guardrails respected:** no syntax highlighting library added (Task 6 uses plain `<pre>`); no server-side persistence of `IssueLogs` to disk (Task 3 keeps it in-memory only); the on-disk `logs/issue-N` files are only read, never deleted or rotated (Task 2); the grid stays a fixed `grid-cols-6` with no responsive variants (Task 4).
