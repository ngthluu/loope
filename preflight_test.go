package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// okHandler answers every preflight probe with a healthy default. Overrides are
// keyed by the full command line ("gh auth status") and replace the default.
func okHandler(overrides map[string]rresp) func(rcall) (string, string, error) {
	defaults := map[string]string{
		"git --version":                       "git version 2.39.5",
		"gh --version":                        "gh version 2.63.2",
		"gh auth status":                      "Logged in to github.com as you",
		"claude --version":                    "2.0.1 (Claude Code)",
		"claude plugin list":                  "superpowers@claude-plugins-official  enabled",
		"git rev-parse --is-inside-work-tree": "true",
		"gh repo view your-org/your-repo --json name":         `{"name":"your-repo"}`,
		"gh label list --repo your-org/your-repo --json name": `[{"name":"ai-agent"},{"name":"ai-wip"},{"name":"ai-failed"},{"name":"ai-done"},{"name":"ai-rework"},{"name":"ai-needs-info"}]`,
		"curl --version": "curl 8.7.1 (x86_64-apple-darwin23.0)",
	}
	return func(c rcall) (string, string, error) {
		key := strings.TrimSpace(c.name + " " + strings.Join(c.args, " "))
		if r, ok := overrides[key]; ok {
			return r.stdout, r.stderr, r.err
		}
		return defaults[key], "", nil
	}
}

func preflightConfig() *Config {
	return &Config{
		RepoPath:      "/tmp/repo",
		RepoSlug:      "your-org/your-repo",
		EligibleLabel: "ai-agent",
		StateLabels:   defaultStateLabels(),
	}
}

func resultByName(t *testing.T, results []CheckResult, name string) CheckResult {
	t.Helper()
	for _, c := range results {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %v", name, results)
	return CheckResult{}
}

func TestPreflightBinariesPass(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	results := Preflight(context.Background(), f, preflightConfig())
	for _, name := range []string{"git", "gh", "claude"} {
		c := resultByName(t, results, name)
		if c.Status != statusOK {
			t.Fatalf("%s: status = %d, want statusOK (detail %q)", name, c.Status, c.Detail)
		}
	}
	if got := resultByName(t, results, "git").Detail; got != "git version 2.39.5" {
		t.Fatalf("git detail = %q", got)
	}
}

func TestPreflightMissingBinaryFails(t *testing.T) {
	for _, name := range []string{"git", "gh", "claude"} {
		f := &fakeRunner{handler: okHandler(map[string]rresp{
			name + " --version": {err: errors.New("executable file not found in $PATH")},
		})}
		results := Preflight(context.Background(), f, preflightConfig())
		c := resultByName(t, results, name)
		if c.Status != statusFail {
			t.Fatalf("%s: status = %d, want statusFail", name, c.Status)
		}
		if len(c.Fix) == 0 {
			t.Fatalf("%s: missing binary must carry fix hints", name)
		}
	}
}

func TestPreflightGHAuthFailsAndBlocksNothingElseYet(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh auth status": {stderr: "You are not logged into any GitHub hosts.", err: errors.New("exit status 1")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "gh auth")
	if c.Status != statusFail {
		t.Fatalf("gh auth status = %d, want statusFail", c.Status)
	}
	if len(c.Fix) != 1 || c.Fix[0] != "gh auth login" {
		t.Fatalf("gh auth fix = %v, want [gh auth login]", c.Fix)
	}
}

func TestPreflightSkipsDependentChecks(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh --version":     {err: errors.New("not found")},
		"claude --version": {err: errors.New("not found")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	for name, blocker := range map[string]string{"gh auth": "gh", "superpowers": "claude"} {
		c := resultByName(t, results, name)
		if c.Status != statusSkip {
			t.Fatalf("%s status = %d, want statusSkip", name, c.Status)
		}
		if !strings.Contains(c.Detail, blocker) {
			t.Fatalf("%s detail = %q, want it to name %q", name, c.Detail, blocker)
		}
	}
}

func TestPreflightSuperpowersMissingPlugin(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"claude plugin list": {stdout: "some-other-plugin@vendor  enabled"},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "superpowers")
	if c.Status != statusFail {
		t.Fatalf("superpowers status = %d, want statusFail", c.Status)
	}
	if len(c.Fix) == 0 || !strings.Contains(c.Fix[0], "claude plugin install superpowers@") {
		t.Fatalf("superpowers fix = %v", c.Fix)
	}
}

func TestPreflightSuperpowersUsesClaudeConfigDir(t *testing.T) {
	cfg := preflightConfig()
	cfg.ClaudeConfigDir = "/home/you/.claude-personal"
	f := &fakeRunner{handler: okHandler(nil)}
	Preflight(context.Background(), f, cfg)
	var got []string
	for _, c := range f.calls {
		if c.name == "claude" && hasArg(c.args, "plugin") {
			got = c.env
		}
	}
	if len(got) != 1 || got[0] != "CLAUDE_CONFIG_DIR=/home/you/.claude-personal" {
		t.Fatalf("plugin list env = %v, want [CLAUDE_CONFIG_DIR=/home/you/.claude-personal]", got)
	}

	cfg.ClaudeConfigDir = ""
	f2 := &fakeRunner{handler: okHandler(nil)}
	Preflight(context.Background(), f2, cfg)
	for _, c := range f2.calls {
		if c.name == "claude" && hasArg(c.args, "plugin") && len(c.env) != 0 {
			t.Fatalf("plugin list env = %v, want none when claudeConfigDir is unset", c.env)
		}
	}
}
