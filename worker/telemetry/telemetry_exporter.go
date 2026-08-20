package telemetry

import (
	wire "github.com/ngthluu/loope/shared"

	"github.com/ngthluu/loope/worker/shared"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// maxLogLinesPerPush caps how many new daemon-log lines one push carries, so
// a worker that was offline a while (or just started against a large
// existing log) does not send one enormous request; the remainder catches
// up over the next few push cycles.
const maxLogLinesPerPush = 500

// maxPendingLogLines caps the unsent daemon-log lines the exporter retains
// across failed pushes, so a long server outage does not grow memory without
// bound; past the cap the oldest lines are dropped (the newest are what an
// operator wants when the server comes back).
const maxPendingLogLines = maxLogLinesPerPush * 4

// maxIssueLogFileBytes caps one persisted log file's pushed content. Most of
// these files are small (prompts, outputs, single-line state files), but
// *.stream.jsonl session transcripts grow to many megabytes — and the whole
// archive is re-sent every push interval, so an uncapped file burns bandwidth
// every cycle and bloats the server's in-memory copy. The tail is kept (the
// most recent output is what an operator reads), behind a truncation banner.
const maxIssueLogFileBytes = 64 << 10

// maxIssueLogContentBytes budgets the total content across one push's whole
// IssueLogs payload, newest directories first. Files past the budget keep
// their name and mod time (the dashboard tree stays complete) but carry a
// placeholder instead of content. Together with maxIssueLogFileBytes this
// bounds both the worker's per-cycle upload and the server's per-worker
// memory — the server rejects anything larger outright (see its
// maxPushBodyBytes).
const maxIssueLogContentBytes = 6 << 20

// issueLogTruncatedBanner leads a file whose pushed content was cut to its
// tail; issueLogElidedContent replaces content that fell past the per-push
// budget entirely.
const (
	issueLogTruncatedBanner = "[loope: file truncated, showing the last 64KB]\n"
	issueLogElidedContent   = "[loope: content not pushed — the log archive exceeds the per-push budget; read it on the worker's disk]"
)

// TelemetryExporter pushes this daemon's log tail and Claude usage to a
// telemetry-server on a fixed interval. It is opt-in: nothing constructs or
// runs it when cfg.Telemetry is nil.
type TelemetryExporter struct {
	cfg       *shared.Config
	version   string
	client    *http.Client
	tailer    *LogTailer
	usagePath string // "" if the user's home directory could not be resolved

	// pending holds log lines read from the tailer but not yet acknowledged
	// by the server. The tailer advances its offset as it reads, so a failed
	// push must keep its batch here and resend it next tick, else those
	// lines are lost forever. Capped at maxPendingLogLines (newest kept).
	pending []string
}

// NewTelemetryExporter builds an exporter for cfg.Telemetry, tailing the
// daemon log at logPath (the same path main.go points the rotating log
// writer at).
func NewTelemetryExporter(cfg *shared.Config, logPath, version string) *TelemetryExporter {
	usagePath, _ := UsageHookFile() // empty on error: readUsageSnapshot then always reports "unavailable"
	return &TelemetryExporter{cfg: cfg, version: version, client: &http.Client{Timeout: 10 * time.Second}, tailer: NewLogTailer(logPath), usagePath: usagePath}
}

// Run pushes on cfg.Telemetry.PushIntervalSec until ctx is cancelled. A push
// failure is logged and retried next tick — the unsent log lines are kept in
// e.pending and lead the next push — and a slow or unreachable server never
// blocks the daemon's own work, since this runs in its own goroutine.
func (e *TelemetryExporter) Run(ctx context.Context) {
	interval := time.Duration(e.cfg.Telemetry.PushIntervalSec) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := e.pushOnce(ctx); err != nil {
			log.Printf("telemetry: push failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pushOnce assembles and sends one PushRequest. Log lines are only dropped
// from e.pending once the server acknowledges them, so a failed push resends
// them next tick instead of losing them.
func (e *TelemetryExporter) pushOnce(ctx context.Context) error {
	lines, err := e.tailer.Next(maxLogLinesPerPush)
	if err != nil {
		log.Printf("telemetry: read log tail: %v", err)
	}
	e.pending = append(e.pending, lines...)
	if n := len(e.pending); n > maxPendingLogLines {
		e.pending = append([]string(nil), e.pending[n-maxPendingLogLines:]...)
	}
	if err := e.send(ctx, e.pending); err != nil {
		return err
	}
	e.pending = nil
	return nil
}

// send builds and POSTs one PushRequest carrying lines as the log tail.
func (e *TelemetryExporter) send(ctx context.Context, lines []string) error {
	now := time.Now()
	logs := make([]wire.LogRecord, len(lines))
	for i, l := range lines {
		logs[i] = wire.LogRecord{Timestamp: now, Body: l}
	}

	usage, err := readUsageSnapshot(e.usagePath, now)
	if err != nil {
		log.Printf("telemetry: read usage snapshot: %v", err)
	}

	// On a scan error issueLogs stays nil, which marshals as JSON null — the
	// server reads that as "no update this cycle" and keeps its previous
	// view, rather than wiping the archive over a transient ReadDir failure.
	issueLogs, err := scanIssueLogs(e.cfg.WorkDir)
	if err != nil {
		log.Printf("telemetry: scan issue logs: %v", err)
	}

	hostname, _ := os.Hostname()
	req := wire.PushRequest{
		Resource: wire.Resource{
			RepoSlug:        e.cfg.RepoSlug,
			MachineID:       wire.MachineID(hostname, e.cfg.WorkDir),
			Hostname:        hostname,
			WorkDir:         e.cfg.WorkDir,
			Version:         e.version,
			PushIntervalSec: e.cfg.Telemetry.PushIntervalSec,
		},
		Logs:      logs,
		Usage:     usage,
		IssueLogs: issueLogs,
		SentAt:    now,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Telemetry.ServerURL+"/v1/push", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+e.cfg.Telemetry.Token)
	resp, err := e.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("push: server returned %s", resp.Status)
	}
	return nil
}

// scanIssueLogs reads workDir/logs and returns one IssueLogDir per
// subdirectory (one per issue's pipeline run),
// carrying the contents of every regular file directly inside — capped per
// file (maxIssueLogFileBytes) and per push (maxIssueLogContentBytes), since
// session transcripts grow to many megabytes. This is a full re-read and
// re-send every cycle (design decision 1), and the scan is non-recursive:
// the existing log writers in tracker.go/claude.go never nest
// subdirectories. A missing or empty logs dir yields an empty slice, never
// nil, so the field marshals as [] — a real "archive is empty", as opposed
// to the null a failed scan sends (see pushOnce).
func scanIssueLogs(workDir string) ([]wire.IssueLogDir, error) {
	logsDir := filepath.Join(workDir, "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []wire.IssueLogDir{}, nil
		}
		return nil, err
	}
	dirs := []wire.IssueLogDir{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := scanIssueLogFiles(filepath.Join(logsDir, e.Name()))
		if err != nil {
			log.Printf("telemetry: scan logs/%s: %v", e.Name(), err)
			continue
		}
		dirs = append(dirs, wire.IssueLogDir{Name: e.Name(), Files: files})
	}
	applyIssueLogBudget(dirs, maxIssueLogContentBytes)
	return dirs, nil
}

// applyIssueLogBudget spends budget bytes of file content across dirs, newest
// directory first, replacing content that falls past the budget with
// issueLogElidedContent. Names and mod times always survive, so the
// dashboard's tree stays complete even when a large archive's older content
// is elided.
func applyIssueLogBudget(dirs []wire.IssueLogDir, budget int) {
	order := make([]int, len(dirs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		return dirModTime(dirs[order[a]]).After(dirModTime(dirs[order[b]]))
	})
	remaining := budget
	for _, di := range order {
		files := dirs[di].Files
		for fi := range files {
			n := len(files[fi].Content)
			if n <= remaining {
				remaining -= n
				continue
			}
			files[fi].Content = issueLogElidedContent
		}
	}
}

// dirModTime returns the latest ModTime across a dir's files.
func dirModTime(d wire.IssueLogDir) time.Time {
	var latest time.Time
	for _, f := range d.Files {
		if f.ModTime.After(latest) {
			latest = f.ModTime
		}
	}
	return latest
}

// scanIssueLogFiles reads every regular file directly inside dir (no
// recursion) into an IssueLogFile.
func scanIssueLogFiles(dir string) ([]wire.IssueLogFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := []wire.IssueLogFile{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		body, err := readFileTail(filepath.Join(dir, e.Name()), info.Size(), maxIssueLogFileBytes)
		if err != nil {
			continue
		}
		files = append(files, wire.IssueLogFile{Name: e.Name(), Content: body, ModTime: info.ModTime()})
	}
	return files, nil
}

// readFileTail returns the whole file when size <= max, else only its last
// max bytes (seeking past the head rather than reading a multi-megabyte
// transcript just to discard most of it), led by issueLogTruncatedBanner.
func readFileTail(path string, size int64, max int) (string, error) {
	if size <= int64(max) {
		content, err := os.ReadFile(path)
		return string(content), err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(size-int64(max), io.SeekStart); err != nil {
		return "", err
	}
	buf := make([]byte, max)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return "", err
	}
	return issueLogTruncatedBanner + string(buf[:n]), nil
}

// readUsageSnapshot reads the usage-hook file at path, returning nil when
// path is empty, the file is missing, or its CapturedAt is older than
// wire.UsageStaleAfter — the dashboard then renders "usage: unknown" rather than
// a stale or fabricated number.
func readUsageSnapshot(path string, now time.Time) (*wire.UsageSnapshot, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var u wire.UsageSnapshot
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	if now.Sub(u.CapturedAt) > wire.UsageStaleAfter {
		return nil, nil
	}
	return &u, nil
}
