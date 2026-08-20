package shared

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

// ModelConfig selects the model and effort for one Claude session role; they
// map to the claude CLI's --model and --effort flags, and "" leaves the CLI's default.
type ModelConfig struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// Models is the per-role model configuration ("models" in loope.json): one
// ModelConfig per pipeline role, some of which fall back to Architect when unset.
type Models struct {
	Architect ModelConfig `json:"architect"`
	Answerer  ModelConfig `json:"answerer"`
	// Execute is the config for the plan-execution step of the feature pipeline.
	// Any field left unset falls back to Architect (see executeConfig), so
	// existing configs without an execute block behave exactly as before.
	Execute ModelConfig `json:"execute"`
	// UAT is the config for the UAT-checklist session. Unlike Execute it has no
	// fallback helper: the block is used exactly as written, so an absent block
	// means the claude CLI's own defaults. The session is short and read-only,
	// so a cheap model is the right shape.
	UAT ModelConfig `json:"uat"`
	// CodeReview is the config for the post-ship review-and-fix loop. Unlike
	// Execute it has no fallback to Architect: a real model choice here matters
	// (the session must both find and fix issues), so an absent block means the
	// whole step is skipped, not "use defaults" — hence the pointer, mirroring
	// Telemetry rather than UAT's always-constructed value field.
	CodeReview *CodeReviewConfig `json:"codeReview"`
	// MergeResolve is the config for the merge-conflict-resolution session of
	// the merge-resolve flow. Any field left unset falls back to Architect
	// (see mergeResolveConfig), mirroring Execute, so existing configs get the
	// flow without a new block.
	MergeResolve ModelConfig `json:"mergeResolve"`
}

// CodeReviewConfig is the config for the post-ship review-and-fix loop.
// Rounds <= 0 is treated as 1 by CodeReview.Run — LoadConfig does not apply
// that default itself, so a config that never sets rounds and one that
// explicitly sets "rounds": 0 are indistinguishable, and both mean "run once".
type CodeReviewConfig struct {
	ModelConfig
	Rounds int `json:"rounds"`
}

// withArchitectFallback fills each field left unset on c from Architect. This
// lets a config override just one step's model or effort without restating the
// other, and keeps configs without the step's block identical to running that
// step with the architect config.
func (m Models) withArchitectFallback(c ModelConfig) ModelConfig {
	if c.Model == "" {
		c.Model = m.Architect.Model
	}
	if c.Effort == "" {
		c.Effort = m.Architect.Effort
	}
	return c
}

// ExecuteConfig returns the model config for the plan-execution step.
func (m Models) ExecuteConfig() ModelConfig { return m.withArchitectFallback(m.Execute) }

// MergeResolveConfig returns the model config for the merge-resolve session.
func (m Models) MergeResolveConfig() ModelConfig { return m.withArchitectFallback(m.MergeResolve) }

// StateLabels are the labels the loop applies to track issue state.
// Unset fields fall back to the ai-wip/ai-done defaults.
type StateLabels struct {
	WIP       string `json:"wip"`
	Done      string `json:"done"`
	Rework    string `json:"rework"`
	NeedsInfo string `json:"needsInfo"`
	Stopped   string `json:"stopped"`
}

// All returns every state label, WIP first: the order Current uses to pick
// the label an issue is wearing when it (transiently) wears more than one.
func (s StateLabels) All() []string {
	return []string{s.WIP, s.Rework, s.NeedsInfo, s.Stopped, s.Done}
}

// Current returns the state label present in labels, or "" when the issue is
// in no state (i.e. eligible for pickup).
func (s StateLabels) Current(labels []Label) string {
	for _, name := range s.All() {
		if HasLabel(labels, name) {
			return name
		}
	}
	return ""
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

// Policy converts the second-valued JSON form into the RetryPolicy the
// retry helpers consume.
func (rc RetryConfig) Policy() RetryPolicy {
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

// Config is the daemon's loope.json. RepoPath, RepoSlug and WorkDir are
// required; everything else has a default applied by LoadConfig (see there).
type Config struct {
	RepoPath      string `json:"repoPath"`
	RepoSlug      string `json:"repoSlug"`
	EligibleLabel string `json:"eligibleLabel"`
	// MergeResolveLabel is the human-applied trigger label of the merge-resolve
	// flow: added to an issue (in any state) with an existing worktree, it asks
	// the daemon to merge origin/<default-branch> into the issue's branch,
	// resolve conflicts with one Claude session, and push. It is a trigger like
	// EligibleLabel, not a StateLabels member: state labels are mutually
	// exclusive, while this one rides on top of whatever state the issue is in.
	// "" disables the flow.
	MergeResolveLabel   string      `json:"mergeResolveLabel"`
	PollIntervalSec     int         `json:"pollIntervalSec"`
	TicketsPerCycle     int         `json:"ticketsPerCycle"`
	WorkDir             string      `json:"workDir"`
	Addr                string      `json:"addr"`
	PersonaPath         string      `json:"personaPath"`
	ClaudeConfigDir     string      `json:"claudeConfigDir"`
	MaxQARounds         int         `json:"maxQARounds"`
	ConfidenceThreshold int         `json:"confidenceThreshold"`
	StateLabels         StateLabels `json:"stateLabels"`
	GitHubRetry         RetryConfig `json:"githubRetry"`
	Models              Models      `json:"models"`
	// Telemetry is nil unless the config file has a "telemetry" block —
	// participation is opt-in.
	Telemetry *TelemetryConfig `json:"telemetry"`
}

// LoadConfig reads and parses the JSON config at path, applying defaults
// (addr localhost:8080, eligibleLabel ai-agent, mergeResolveLabel
// ai-resolve-merge, pollIntervalSec 60, ticketsPerCycle 1, maxQARounds 20,
// confidenceThreshold 70, the default state labels, githubRetry 2s/60s, and
// telemetry.pushIntervalSec 15 when a telemetry block is present), expanding
// "~" in paths, and requiring repoPath, repoSlug and workDir.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{Addr: "localhost:8080", EligibleLabel: "ai-agent", MergeResolveLabel: "ai-resolve-merge", PollIntervalSec: 60, MaxQARounds: 20, ConfidenceThreshold: 70, TicketsPerCycle: 1, StateLabels: defaultStateLabels(), GitHubRetry: RetryConfig{BaseDelaySec: 2, MaxDelaySec: 60}}
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
