package main

import (
	"github.com/ngthluu/loope/shared"

	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// maxLogLinesPerPush caps how many new daemon-log lines one push carries, so
// a worker that was offline a while (or just started against a large
// existing log) does not send one enormous request; the remainder catches
// up over the next few push cycles.
const maxLogLinesPerPush = 500

// TelemetryExporter pushes this daemon's log tail and Claude usage to a
// telemetry-server on a fixed interval. It is opt-in: nothing constructs or
// runs it when cfg.Telemetry is nil.
type TelemetryExporter struct {
	cfg       *Config
	client    *http.Client
	tailer    *LogTailer
	usagePath string // "" if the user's home directory could not be resolved
}

// NewTelemetryExporter builds an exporter for cfg.Telemetry, tailing the
// daemon log at logPath (the same path main.go points the rotating log
// writer at).
func NewTelemetryExporter(cfg *Config, logPath string) *TelemetryExporter {
	usagePath, _ := usageHookFile() // empty on error: readUsageSnapshot then always reports "unavailable"
	return &TelemetryExporter{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, tailer: NewLogTailer(logPath), usagePath: usagePath}
}

// Run pushes on cfg.Telemetry.PushIntervalSec until ctx is cancelled. A push
// failure is logged and retried next tick — a slow or unreachable server
// never blocks the daemon's own work, since this runs in its own goroutine.
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

// pushOnce assembles and sends one PushRequest.
func (e *TelemetryExporter) pushOnce(ctx context.Context) error {
	lines, err := e.tailer.Next(maxLogLinesPerPush)
	if err != nil {
		log.Printf("telemetry: read log tail: %v", err)
	}
	now := time.Now()
	logs := make([]shared.LogRecord, len(lines))
	for i, l := range lines {
		logs[i] = shared.LogRecord{Timestamp: now, Body: l}
	}

	usage, err := readUsageSnapshot(e.usagePath, now)
	if err != nil {
		log.Printf("telemetry: read usage snapshot: %v", err)
	}

	hostname, _ := os.Hostname()
	req := shared.PushRequest{
		Resource: shared.Resource{
			RepoSlug:        e.cfg.RepoSlug,
			MachineID:       shared.MachineID(hostname, e.cfg.WorkDir),
			Hostname:        hostname,
			WorkDir:         e.cfg.WorkDir,
			Version:         version,
			PushIntervalSec: e.cfg.Telemetry.PushIntervalSec,
		},
		Logs:   logs,
		Usage:  usage,
		SentAt: now,
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

// readUsageSnapshot reads the usage-hook file at path, returning nil when
// path is empty, the file is missing, or its CapturedAt is older than
// shared.UsageStaleAfter — the dashboard then renders "usage: unknown" rather than
// a stale or fabricated number.
func readUsageSnapshot(path string, now time.Time) (*shared.UsageSnapshot, error) {
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
	var u shared.UsageSnapshot
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, err
	}
	if now.Sub(u.CapturedAt) > shared.UsageStaleAfter {
		return nil, nil
	}
	return &u, nil
}
