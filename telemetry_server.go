package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultPushIntervalSec = 15
	maxLogLinesPerWorker   = 2000
	usageStaleAfter        = 30 * time.Minute
)

// WorkerState is the server's last-known view of one worker, keyed by
// Resource.MachineID.
type WorkerState struct {
	Resource   Resource
	LastPushAt time.Time
	Usage      *UsageSnapshot
	Logs       *LogRingBuffer
}

// online reports whether the worker has pushed recently enough to be
// considered live: at most 3 push intervals have elapsed since its last
// push. A worker whose Resource never carried a PushIntervalSec uses
// defaultPushIntervalSec.
func (w *WorkerState) online(now time.Time) bool {
	interval := w.Resource.PushIntervalSec
	if interval <= 0 {
		interval = defaultPushIntervalSec
	}
	return now.Sub(w.LastPushAt) < time.Duration(3*interval)*time.Second
}

// usableUsage returns w.Usage if it is fresh enough to show, else nil — a
// snapshot whose CapturedAt is older than usageStaleAfter renders as
// "unknown" rather than a stale number.
func (w *WorkerState) usableUsage(now time.Time) *UsageSnapshot {
	if w.Usage == nil || now.Sub(w.Usage.CapturedAt) > usageStaleAfter {
		return nil
	}
	return w.Usage
}

// TelemetryServer receives worker pushes and renders the fleet dashboard.
// All state is in-memory: a restart goes blank until workers' next push
// repopulates it (see the design doc's storage section) — this is a live
// view, not a system of record.
type TelemetryServer struct {
	token string
	now   func() time.Time

	mu      sync.Mutex
	workers map[string]*WorkerState // keyed by Resource.MachineID
}

// NewTelemetryServer returns a server that authenticates pushes against
// token.
func NewTelemetryServer(token string) *TelemetryServer {
	return &TelemetryServer{token: token, now: time.Now, workers: map[string]*WorkerState{}}
}

// Handler returns the telemetry server's HTTP routes: POST /v1/push (worker
// ingest).
func (s *TelemetryServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/push", s.handlePush)
	return mux
}

// checkAuth reports whether the request carries the correct bearer token.
func checkAuth(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, prefix) && strings.TrimPrefix(h, prefix) == token
}

// handlePush ingests one worker's PushRequest. New log lines are appended to
// the worker's ring buffer; Usage replaces the prior snapshot outright — the
// exporter always sends its latest read (nil when unavailable), so there is
// nothing to merge.
func (s *TelemetryServer) handlePush(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(r, s.token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Resource.MachineID == "" {
		http.Error(w, "resource.machineID is required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ws := s.workers[req.Resource.MachineID]
	if ws == nil {
		ws = &WorkerState{Logs: NewLogRingBuffer(maxLogLinesPerWorker)}
		s.workers[req.Resource.MachineID] = ws
	}
	ws.Resource = req.Resource
	ws.LastPushAt = s.now()
	ws.Usage = req.Usage
	lines := make([]string, len(req.Logs))
	for i, l := range req.Logs {
		lines[i] = l.Body
	}
	ws.Logs.Add(lines...)

	w.WriteHeader(http.StatusNoContent)
}
