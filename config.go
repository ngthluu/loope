package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	labelWIP       = "ai-wip"
	labelDone      = "ai-done"
	labelRework    = "ai-rework"
	labelNeedsInfo = "ai-needs-info"
	labelStopped   = "ai-stopped"
)

type ModelConfig struct {
	Model        string  `json:"model"`
	Effort       string  `json:"effort"`
	MaxBudgetUSD float64 `json:"maxBudgetUSD"`
	MaxTurns     int     `json:"maxTurns"`
}

type Models struct {
	Architect ModelConfig `json:"architect"`
	Answerer  ModelConfig `json:"answerer"`
	Triage    ModelConfig `json:"triage"`
	// Execute is the config for the plan-execution step of the feature pipeline.
	// It typically wants a much higher turn/budget ceiling than the bounded
	// architect Q&A rounds, since it implements the whole plan in one session.
	// Any field left unset falls back to Architect (see executeConfig), so
	// existing configs without an execute block behave exactly as before.
	Execute ModelConfig `json:"execute"`
	// UAT is the config for the UAT-checklist session. Unlike Execute it has no
	// fallback helper: the block is used exactly as written, so an absent block
	// means the claude CLI's own defaults with no budget or turn cap. The session
	// is short and read-only, so a cheap model with a low cap is the right shape.
	UAT ModelConfig `json:"uat"`
}

// executeConfig returns the model config for the plan-execution step, filling
// each field left unset on Execute from Architect. This lets a config raise
// just execute's maxTurns/maxBudgetUSD without restating the model or effort,
// and keeps pre-execute-block configs identical to the old behavior.
func (m Models) executeConfig() ModelConfig {
	e := m.Execute
	if e.Model == "" {
		e.Model = m.Architect.Model
	}
	if e.Effort == "" {
		e.Effort = m.Architect.Effort
	}
	if e.MaxBudgetUSD == 0 {
		e.MaxBudgetUSD = m.Architect.MaxBudgetUSD
	}
	if e.MaxTurns == 0 {
		e.MaxTurns = m.Architect.MaxTurns
	}
	return e
}

// StateLabels are the labels the loop applies to track issue state.
// Unset fields fall back to the ai-wip/ai-done defaults.
type StateLabels struct {
	WIP       string `json:"wip"`
	Done      string `json:"done"`
	Rework    string `json:"rework"`
	NeedsInfo string `json:"needsInfo"`
	Stopped   string `json:"stopped"`
}

func defaultStateLabels() StateLabels {
	return StateLabels{WIP: labelWIP, Done: labelDone, Rework: labelRework, NeedsInfo: labelNeedsInfo, Stopped: labelStopped}
}

// RetryConfig is the JSON-facing form of RetryPolicy: durations in seconds.
// MaxAttempts == 0 means retry until success / a permanent error / shutdown.
type RetryConfig struct {
	MaxAttempts  int `json:"maxAttempts"`
	BaseDelaySec int `json:"baseDelaySec"`
	MaxDelaySec  int `json:"maxDelaySec"`
}

func (rc RetryConfig) policy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: rc.MaxAttempts,
		BaseDelay:   time.Duration(rc.BaseDelaySec) * time.Second,
		MaxDelay:    time.Duration(rc.MaxDelaySec) * time.Second,
	}
}

// TelemetryConfig opts this daemon in to pushing its status, log tail, and
// Claude usage to a `loope telemetry-server`. Absent from the config, no
// exporter goroutine starts and nothing about daemon behavior changes.
type TelemetryConfig struct {
	ServerURL       string `json:"serverURL"`
	Token           string `json:"token"`
	PushIntervalSec int    `json:"pushIntervalSec"`
}

type Config struct {
	RepoPath            string `json:"repoPath"`
	RepoSlug            string `json:"repoSlug"`
	EligibleLabel       string `json:"eligibleLabel"`
	PollIntervalSec     int    `json:"pollIntervalSec"`
	TicketsPerCycle     int    `json:"ticketsPerCycle"`
	WorkDir             string `json:"workDir"`
	Addr                string `json:"addr"`
	PersonaPath         string `json:"personaPath"`
	ClaudeConfigDir     string `json:"claudeConfigDir"`
	MaxQARounds         int    `json:"maxQARounds"`
	ConfidenceThreshold int    `json:"confidenceThreshold"`
	// StepsPerSession caps how many plan steps one feature-pipeline execute
	// session attempts before handing off to a fresh session. 0 (the zero
	// value, and the default when the key is absent) means "unbounded": one
	// execute session implements the whole plan, exactly as before this field
	// existed. See executePlan in pipeline_feature.go.
	StepsPerSession int         `json:"stepsPerSession"`
	StateLabels     StateLabels `json:"stateLabels"`
	GitHubRetry     RetryConfig `json:"githubRetry"`
	Models          Models      `json:"models"`
	// Telemetry is nil unless the config file has a "telemetry" block —
	// participation is opt-in.
	Telemetry *TelemetryConfig `json:"telemetry"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{Addr: "localhost:8080", EligibleLabel: "ai-agent", PollIntervalSec: 60, MaxQARounds: 20, ConfidenceThreshold: 70, TicketsPerCycle: 1, StateLabels: defaultStateLabels(), GitHubRetry: RetryConfig{BaseDelaySec: 2, MaxDelaySec: 60}}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.RepoPath == "" {
		return nil, fmt.Errorf("config: repoPath is required")
	}
	if cfg.RepoSlug == "" {
		return nil, fmt.Errorf("config: repoSlug is required")
	}
	if cfg.WorkDir == "" {
		return nil, fmt.Errorf("config: workDir is required")
	}
	cfg.RepoPath = expandHome(cfg.RepoPath)
	cfg.WorkDir = expandHome(cfg.WorkDir)
	if abs, err := filepath.Abs(cfg.WorkDir); err == nil {
		cfg.WorkDir = abs
	}
	cfg.PersonaPath = expandHome(cfg.PersonaPath)
	cfg.ClaudeConfigDir = expandHome(cfg.ClaudeConfigDir)
	if cfg.Telemetry != nil && cfg.Telemetry.PushIntervalSec == 0 {
		cfg.Telemetry.PushIntervalSec = 15
	}
	return cfg, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
