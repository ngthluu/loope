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
	labelFailed    = "ai-failed"
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
// Unset fields fall back to the ai-wip/ai-failed/ai-done defaults.
type StateLabels struct {
	WIP       string `json:"wip"`
	Failed    string `json:"failed"`
	Done      string `json:"done"`
	Rework    string `json:"rework"`
	NeedsInfo string `json:"needsInfo"`
	// Stopped is the operator-held state: work is halted and all progress
	// preserved, and only an explicit continue moves the issue out of it. It is
	// deliberately NOT ai-rework, which the daemon auto-resumes.
	Stopped string `json:"stopped"`
}

func defaultStateLabels() StateLabels {
	return StateLabels{WIP: labelWIP, Failed: labelFailed, Done: labelDone, Rework: labelRework, NeedsInfo: labelNeedsInfo, Stopped: labelStopped}
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

type Config struct {
	RepoPath            string      `json:"repoPath"`
	RepoSlug            string      `json:"repoSlug"`
	EligibleLabel       string      `json:"eligibleLabel"`
	PollIntervalSec     int         `json:"pollIntervalSec"`
	TicketsPerCycle     int         `json:"ticketsPerCycle"`
	WorkDir             string      `json:"workDir"`
	PersonaPath         string      `json:"personaPath"`
	ClaudeConfigDir     string      `json:"claudeConfigDir"`
	MaxQARounds         int         `json:"maxQARounds"`
	ConfidenceThreshold int         `json:"confidenceThreshold"`
	StateLabels         StateLabels `json:"stateLabels"`
	GitHubRetry         RetryConfig `json:"githubRetry"`
	Models              Models      `json:"models"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{EligibleLabel: "ai-agent", PollIntervalSec: 60, MaxQARounds: 20, ConfidenceThreshold: 70, TicketsPerCycle: 1, StateLabels: defaultStateLabels(), GitHubRetry: RetryConfig{BaseDelaySec: 2, MaxDelaySec: 60}}
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
