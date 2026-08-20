package shared

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
	m := Models{Architect: ModelConfig{Model: "opus", Effort: "high"}}
	got := m.ExecuteConfig()
	want := ModelConfig{Model: "opus", Effort: "high"}
	if got != want {
		t.Errorf("executeConfig with no override = %+v, want architect %+v", got, want)
	}
}

// TestModelsExecuteConfigOverrides: a partial execute block overrides just the
// fields it sets while inheriting the rest from architect.
func TestModelsExecuteConfigOverrides(t *testing.T) {
	m := Models{
		Architect: ModelConfig{Model: "opus", Effort: "high"},
		Execute:   ModelConfig{Effort: "max"},
	}
	got := m.ExecuteConfig()
	want := ModelConfig{Model: "opus", Effort: "max"}
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
		"models": {"architect": {"model": "opus", "effort": "high"}}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RepoSlug != "org/repo" {
		t.Errorf("RepoSlug = %q", cfg.RepoSlug)
	}
	if cfg.Models.Architect.Model != "opus" || cfg.Models.Architect.Effort != "high" {
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
	want := StateLabels{WIP: "ai-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}
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
	want := StateLabels{WIP: "bot-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}
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
	got := RetryConfig{MaxAttempts: 3, BaseDelaySec: 2, MaxDelaySec: 60}.Policy()
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

// The uat block is used exactly as written: unlike execute, it must NOT inherit
// anything from architect. An absent block means the claude CLI's own defaults.
func TestLoadConfigUATBlockRoundTrips(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone",
		"repoSlug": "org/repo",
		"workDir": "/tmp/work",
		"models": {
			"architect": {"model": "opus", "effort": "high"},
			"uat": {"model": "sonnet", "effort": "medium"}
		}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	want := ModelConfig{Model: "sonnet", Effort: "medium"}
	if cfg.Models.UAT != want {
		t.Errorf("Models.UAT = %+v, want %+v", cfg.Models.UAT, want)
	}
}

func TestLoadConfigUATDoesNotInheritFromArchitect(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone",
		"repoSlug": "org/repo",
		"workDir": "/tmp/work",
		"models": {"architect": {"model": "opus", "effort": "high"}}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.UAT != (ModelConfig{}) {
		t.Errorf("Models.UAT = %+v, want the zero value — uat must not inherit from architect", cfg.Models.UAT)
	}
}

func TestLoadConfigTelemetryAbsentByDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath":"/r","repoSlug":"o/r","workDir":"/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry != nil {
		t.Fatalf("Telemetry = %+v, want nil when the block is absent", cfg.Telemetry)
	}
}

func TestLoadConfigCodeReviewRoundTrips(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone",
		"repoSlug": "org/repo",
		"workDir": "/tmp/work",
		"models": {"codeReview": {"model": "sonnet", "effort": "medium", "rounds": 2}}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.CodeReview == nil {
		t.Fatal("Models.CodeReview = nil, want the parsed block")
	}
	want := CodeReviewConfig{ModelConfig: ModelConfig{Model: "sonnet", Effort: "medium"}, Rounds: 2}
	if *cfg.Models.CodeReview != want {
		t.Errorf("Models.CodeReview = %+v, want %+v", *cfg.Models.CodeReview, want)
	}
}

func TestLoadConfigCodeReviewAbsentByDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath":"/r","repoSlug":"o/r","workDir":"/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.CodeReview != nil {
		t.Fatalf("Models.CodeReview = %+v, want nil when the block is absent — an absent block must skip the step entirely, not run with defaults", cfg.Models.CodeReview)
	}
}

func TestLoadConfigCodeReviewZeroRoundsTreatedAsOne(t *testing.T) {
	p := writeTemp(t, `{
		"repoPath": "/tmp/clone", "repoSlug": "org/repo", "workDir": "/tmp/work",
		"models": {"codeReview": {"model": "sonnet"}}
	}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.CodeReview == nil || cfg.Models.CodeReview.Rounds != 0 {
		t.Fatalf("Models.CodeReview.Rounds = %+v, want 0 as parsed — the <=0-means-1 default is CodeReview.Run's job, not LoadConfig's", cfg.Models.CodeReview)
	}
}

func TestLoadConfigTelemetryPushIntervalDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath":"/r","repoSlug":"o/r","workDir":"/w","telemetry":{"serverURL":"http://host:9090","token":"secret"}}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry == nil {
		t.Fatal("Telemetry = nil, want a populated block")
	}
	if cfg.Telemetry.PushIntervalSec != 15 {
		t.Fatalf("PushIntervalSec = %d, want the 15s default", cfg.Telemetry.PushIntervalSec)
	}
	if cfg.Telemetry.ServerURL != "http://host:9090" || cfg.Telemetry.Token != "secret" {
		t.Fatalf("Telemetry = %+v", cfg.Telemetry)
	}
}

func TestLoadConfigTelemetryPushIntervalOverride(t *testing.T) {
	p := writeTemp(t, `{"repoPath":"/r","repoSlug":"o/r","workDir":"/w","telemetry":{"serverURL":"http://host:9090","token":"secret","pushIntervalSec":30}}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemetry.PushIntervalSec != 30 {
		t.Fatalf("PushIntervalSec = %d, want 30", cfg.Telemetry.PushIntervalSec)
	}
}

// TestExampleConfigParsesAndCoversEverything keeps loope.json.example honest:
// it must parse through LoadConfig exactly as a user's copied config would
// (the "//" comment keys are ignored as unknown fields), and the values it
// demonstrates must land where the comments claim — including the optional
// codeReview and telemetry blocks, which only this example documents.
func TestExampleConfigParsesAndCoversEverything(t *testing.T) {
	cfg, err := LoadConfig("../loope.json.example")
	if err != nil {
		t.Fatalf("the shipped example must always parse: %v", err)
	}
	if cfg.RepoSlug != "your-org/your-repo" || cfg.EligibleLabel != "ai-agent" {
		t.Errorf("basic fields = %q/%q", cfg.RepoSlug, cfg.EligibleLabel)
	}
	if cfg.Models.CodeReview == nil || cfg.Models.CodeReview.Rounds != 2 {
		t.Errorf("codeReview block = %+v, want rounds 2", cfg.Models.CodeReview)
	}
	if cfg.Telemetry == nil || cfg.Telemetry.PushIntervalSec != 15 || cfg.Telemetry.ServerURL == "" {
		t.Errorf("telemetry block = %+v", cfg.Telemetry)
	}
	if cfg.StateLabels != defaultStateLabels() {
		t.Errorf("stateLabels = %+v, want the defaults spelled out", cfg.StateLabels)
	}
}

// The merge-resolve session falls back to the architect config field-by-field,
// exactly like executeConfig, so existing configs need no mergeResolve block.
func TestModelsMergeResolveConfigFallsBackToArchitect(t *testing.T) {
	m := Models{Architect: ModelConfig{Model: "opus", Effort: "high"}}
	got := m.MergeResolveConfig()
	want := ModelConfig{Model: "opus", Effort: "high"}
	if got != want {
		t.Errorf("mergeResolveConfig with no override = %+v, want architect %+v", got, want)
	}
}

func TestModelsMergeResolveConfigOverrides(t *testing.T) {
	m := Models{
		Architect:    ModelConfig{Model: "opus", Effort: "high"},
		MergeResolve: ModelConfig{Model: "sonnet"},
	}
	got := m.MergeResolveConfig()
	want := ModelConfig{Model: "sonnet", Effort: "high"}
	if got != want {
		t.Errorf("mergeResolveConfig = %+v, want %+v", got, want)
	}
}

func TestLoadConfigMergeResolveLabelDefault(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w"}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MergeResolveLabel != "ai-resolve-merge" {
		t.Errorf("MergeResolveLabel = %q, want ai-resolve-merge", cfg.MergeResolveLabel)
	}
}

// An explicit "" opts out of the flow entirely, so the default must not
// resurrect it.
func TestLoadConfigMergeResolveLabelDisabled(t *testing.T) {
	p := writeTemp(t, `{"repoPath": "/a", "repoSlug": "o/r", "workDir": "/w", "mergeResolveLabel": ""}`)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MergeResolveLabel != "" {
		t.Errorf("MergeResolveLabel = %q, want \"\" (disabled)", cfg.MergeResolveLabel)
	}
}
