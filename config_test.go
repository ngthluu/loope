package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestModelsExecuteConfigFallsBackToArchitect: with no execute block, the
// plan-execution step must run with the architect's model config, so existing
// configs behave exactly as before.
func TestModelsExecuteConfigFallsBackToArchitect(t *testing.T) {
	m := Models{Architect: ModelConfig{Model: "opus", Effort: "high", MaxBudgetUSD: 15, MaxTurns: 100}}
	got := m.executeConfig()
	want := ModelConfig{Model: "opus", Effort: "high", MaxBudgetUSD: 15, MaxTurns: 100}
	if got != want {
		t.Errorf("executeConfig with no override = %+v, want architect %+v", got, want)
	}
}

// TestModelsExecuteConfigOverrides: a partial execute block raises just the
// fields it sets (turns/budget) while inheriting model/effort from architect.
func TestModelsExecuteConfigOverrides(t *testing.T) {
	m := Models{
		Architect: ModelConfig{Model: "opus", Effort: "high", MaxBudgetUSD: 15, MaxTurns: 100},
		Execute:   ModelConfig{MaxTurns: 300, MaxBudgetUSD: 30},
	}
	got := m.executeConfig()
	want := ModelConfig{Model: "opus", Effort: "high", MaxBudgetUSD: 30, MaxTurns: 300}
	if got != want {
		t.Errorf("executeConfig = %+v, want %+v", got, want)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "loope.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigValid(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone",
		"repoSlug": "org/repo",
		"workDir": "/tmp/work",
		"models": {"architect": {"model": "opus", "effort": "high", "maxBudgetUSD": 15, "maxTurns": 100}}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RepoSlug != "org/repo" {
		t.Errorf("RepoSlug = %q", cfg.RepoSlug)
	}
	if cfg.Models.Architect.Model != "opus" || cfg.Models.Architect.MaxTurns != 100 {
		t.Errorf("architect = %+v", cfg.Models.Architect)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EligibleLabel != "ai-agent" {
		t.Errorf("EligibleLabel = %q, want ai-agent", cfg.EligibleLabel)
	}
	if cfg.PollIntervalSec != 60 {
		t.Errorf("PollIntervalSec = %d, want 60", cfg.PollIntervalSec)
	}
	if cfg.MaxQARounds != 20 {
		t.Errorf("MaxQARounds = %d, want 20", cfg.MaxQARounds)
	}
}

func TestDefaultStateLabelsIncludesStopped(t *testing.T) {
	if got := defaultStateLabels().Stopped; got != "ai-stopped" {
		t.Fatalf("defaultStateLabels().Stopped = %q, want ai-stopped", got)
	}
}

func TestLoadConfigStateLabelDefaults(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := StateLabels{WIP: "ai-wip", Failed: "ai-failed", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}
	if cfg.StateLabels != want {
		t.Errorf("StateLabels = %+v, want %+v", cfg.StateLabels, want)
	}
}

func TestLoadConfigStateLabelPartialOverride(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w",
		"stateLabels": {"wip": "bot-wip"}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := StateLabels{WIP: "bot-wip", Failed: "ai-failed", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}
	if cfg.StateLabels != want {
		t.Errorf("StateLabels = %+v, want %+v", cfg.StateLabels, want)
	}
}

func TestLoadConfigConfidenceThresholdDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "loope.json")
	os.WriteFile(p, []byte(`{"repoPath":"/r","repoSlug":"o/r","workDir":"`+dir+`"}`), 0o644)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfidenceThreshold != 70 {
		t.Errorf("ConfidenceThreshold = %d, want 70", cfg.ConfidenceThreshold)
	}
}

func TestLoadConfigConfidenceThresholdOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "loope.json")
	os.WriteFile(p, []byte(`{"repoPath":"/r","repoSlug":"o/r","workDir":"`+dir+`","confidenceThreshold":40}`), 0o644)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfidenceThreshold != 40 {
		t.Errorf("ConfidenceThreshold = %d, want 40", cfg.ConfidenceThreshold)
	}
}

func TestLoadConfigNeedsInfoLabelDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "loope.json")
	os.WriteFile(p, []byte(`{"repoPath":"/r","repoSlug":"o/r","workDir":"`+dir+`"}`), 0o644)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateLabels.NeedsInfo != "ai-needs-info" {
		t.Errorf("NeedsInfo = %q, want ai-needs-info", cfg.StateLabels.NeedsInfo)
	}
}

func TestLoadConfigMissingRequired(t *testing.T) {
	for _, body := range []string{
		`{"repoSlug": "o/r", "workDir": "/w"}`,
		`{"repoPath": "/a", "workDir": "/w"}`,
		`{"repoPath": "/a", "repoSlug": "o/r"}`,
	} {
		if _, err := LoadConfig(writeTemp(t, body)); err == nil {
			t.Errorf("want error for %s, got nil", body)
		}
	}
}

func TestLoadConfigExpandsHome(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "~/clone", "repoSlug": "o/r", "workDir": "~/work", "claudeConfigDir": "~/.claude-personal"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(cfg.RepoPath, home) {
		t.Errorf("RepoPath = %q, want prefix %q", cfg.RepoPath, home)
	}
	if !strings.HasPrefix(cfg.WorkDir, home) {
		t.Errorf("WorkDir = %q, want prefix %q", cfg.WorkDir, home)
	}
	if !strings.HasPrefix(cfg.ClaudeConfigDir, home) {
		t.Errorf("ClaudeConfigDir = %q, want prefix %q", cfg.ClaudeConfigDir, home)
	}
}

func TestLoadConfigMakesWorkDirAbsolute(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "relwork"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.WorkDir) {
		t.Errorf("WorkDir = %q, want absolute path", cfg.WorkDir)
	}
}

func TestLoadConfigTicketsPerCycleDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TicketsPerCycle != 1 {
		t.Errorf("TicketsPerCycle = %d, want 1", cfg.TicketsPerCycle)
	}
}

func TestLoadConfigTicketsPerCycleOverride(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w", "ticketsPerCycle": 4}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TicketsPerCycle != 4 {
		t.Errorf("TicketsPerCycle = %d, want 4", cfg.TicketsPerCycle)
	}
}

func TestLoadConfigGitHubRetryDefaults(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := RetryConfig{MaxAttempts: 0, BaseDelaySec: 2, MaxDelaySec: 60}
	if cfg.GitHubRetry != want {
		t.Errorf("GitHubRetry = %+v, want %+v", cfg.GitHubRetry, want)
	}
}

func TestLoadConfigGitHubRetryPartialOverride(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w",
		"githubRetry": {"maxAttempts": 5}}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	// maxAttempts overridden; baseDelaySec/maxDelaySec keep their defaults (JSON merge).
	want := RetryConfig{MaxAttempts: 5, BaseDelaySec: 2, MaxDelaySec: 60}
	if cfg.GitHubRetry != want {
		t.Errorf("GitHubRetry = %+v, want %+v", cfg.GitHubRetry, want)
	}
}

func TestRetryConfigPolicy(t *testing.T) {
	got := RetryConfig{MaxAttempts: 3, BaseDelaySec: 2, MaxDelaySec: 60}.policy()
	want := RetryPolicy{MaxAttempts: 3, BaseDelay: 2 * time.Second, MaxDelay: 60 * time.Second}
	if got != want {
		t.Errorf("policy = %+v, want %+v", got, want)
	}
}

func TestLoadConfigAddrDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "localhost:8080" {
		t.Errorf("Addr = %q, want localhost:8080", cfg.Addr)
	}
}

func TestLoadConfigAddrOverride(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w", "addr": "localhost:9000"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "localhost:9000" {
		t.Errorf("Addr = %q, want localhost:9000", cfg.Addr)
	}
}
