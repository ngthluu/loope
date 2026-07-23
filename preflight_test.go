package main

import (
	"bytes"
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

func TestPreflightRepoPathNotAWorktree(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"git rev-parse --is-inside-work-tree": {stderr: "fatal: not a git repository", err: errors.New("exit status 128")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "repoPath")
	if c.Status != statusFail {
		t.Fatalf("repoPath status = %d, want statusFail", c.Status)
	}
	if !strings.Contains(c.Detail, "/tmp/repo") {
		t.Fatalf("repoPath detail = %q, want it to name the configured path", c.Detail)
	}
}

func TestPreflightRepoPathRunsInRepoDir(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	Preflight(context.Background(), f, preflightConfig())
	found := false
	for _, c := range f.calls {
		if c.name == "git" && hasArg(c.args, "rev-parse") {
			found = true
			if c.dir != "/tmp/repo" {
				t.Fatalf("rev-parse dir = %q, want /tmp/repo", c.dir)
			}
		}
	}
	if !found {
		t.Fatal("git rev-parse was never run")
	}
}

func TestPreflightRepoAccessSkippedWhenAuthFails(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh auth status": {err: errors.New("exit status 1")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "repo access")
	if c.Status != statusSkip {
		t.Fatalf("repo access status = %d, want statusSkip", c.Status)
	}
	if !strings.Contains(c.Detail, "gh auth") {
		t.Fatalf("repo access detail = %q, want it to name the blocker", c.Detail)
	}
}

func TestPreflightRepoAccessFails(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh repo view your-org/your-repo --json name": {stderr: "GraphQL: Could not resolve to a Repository", err: errors.New("exit status 1")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "repo access")
	if c.Status != statusFail {
		t.Fatalf("repo access status = %d, want statusFail", c.Status)
	}
	if !strings.Contains(c.Detail, "your-org/your-repo") {
		t.Fatalf("repo access detail = %q, want it to name the slug", c.Detail)
	}
}

func TestPreflightMissingLabelsWarn(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh label list --repo your-org/your-repo --json name": {stdout: `[{"name":"ai-agent"},{"name":"ai-wip"},{"name":"ai-done"}]`},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "labels")
	if c.Status != statusWarn {
		t.Fatalf("labels status = %d, want statusWarn", c.Status)
	}
	want := []string{
		"gh label create ai-failed --repo your-org/your-repo",
		"gh label create ai-rework --repo your-org/your-repo",
		"gh label create ai-needs-info --repo your-org/your-repo",
	}
	if len(c.Fix) != len(want) {
		t.Fatalf("labels fix = %v, want %v", c.Fix, want)
	}
	for i := range want {
		if c.Fix[i] != want[i] {
			t.Fatalf("labels fix[%d] = %q, want %q", i, c.Fix[i], want[i])
		}
	}
	if ReportPreflightFailedCount(results) != 0 {
		t.Fatal("a labels warning must not be fatal")
	}
}

func TestPreflightAllLabelsPresent(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	results := Preflight(context.Background(), f, preflightConfig())
	if c := resultByName(t, results, "labels"); c.Status != statusOK {
		t.Fatalf("labels status = %d (detail %q), want statusOK", c.Status, c.Detail)
	}
}

func TestPreflightMissingCurlWarns(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"curl --version": {err: errors.New("not found")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	c := resultByName(t, results, "curl")
	if c.Status != statusWarn {
		t.Fatalf("curl status = %d, want statusWarn", c.Status)
	}
	if !strings.Contains(c.Detail, "image attachments") {
		t.Fatalf("curl detail = %q, want it to explain the degradation", c.Detail)
	}
	if ReportPreflightFailedCount(results) != 0 {
		t.Fatal("a missing curl must not be fatal")
	}
}

func TestPreflightHealthyMachineHasNoFailures(t *testing.T) {
	f := &fakeRunner{handler: okHandler(nil)}
	results := Preflight(context.Background(), f, preflightConfig())
	if len(results) != 9 {
		t.Fatalf("got %d checks, want 9", len(results))
	}
	for _, c := range results {
		if c.Status != statusOK {
			t.Fatalf("%s: status = %d (detail %q), want statusOK", c.Name, c.Status, c.Detail)
		}
	}
}

func TestPreflightSkippedChecksAreNotFailures(t *testing.T) {
	f := &fakeRunner{handler: okHandler(map[string]rresp{
		"gh --version": {err: errors.New("not found")},
	})}
	results := Preflight(context.Background(), f, preflightConfig())
	// Only `gh` itself is a failure; gh auth / repo access / labels all skip.
	if n := ReportPreflightFailedCount(results); n != 1 {
		t.Fatalf("failed count = %d, want 1 (only gh)", n)
	}
	if c := resultByName(t, results, "labels"); c.Status != statusSkip {
		t.Fatalf("labels status = %d, want statusSkip", c.Status)
	}
}

func TestReportPreflightRendersAllStatuses(t *testing.T) {
	results := []CheckResult{
		{Name: "git", Status: statusOK, Detail: "git version 2.39.5"},
		{Name: "gh auth", Status: statusFail, Detail: "not authenticated", Fix: []string{"gh auth login"}},
		{Name: "repo access", Status: statusSkip, Detail: "skipped (gh auth failed)"},
		{Name: "curl", Status: statusWarn, Detail: "not found — issue image attachments will be skipped"},
	}
	var buf bytes.Buffer
	failed := ReportPreflight(&buf, results)
	if !failed {
		t.Fatal("failed = false, want true (gh auth is a required check)")
	}
	want := "loope preflight\n\n" +
		"  ✔ git           git version 2.39.5\n" +
		"  ✘ gh auth       not authenticated\n" +
		"      → gh auth login\n" +
		"  - repo access   skipped (gh auth failed)\n" +
		"  ! curl          not found — issue image attachments will be skipped\n" +
		"\n1 required check failed. Fix them and re-run `loope -doctor` to verify.\n"
	if got := buf.String(); got != want {
		t.Fatalf("report =\n%q\nwant\n%q", got, want)
	}
}

func TestReportPreflightHealthyOmitsSummary(t *testing.T) {
	var buf bytes.Buffer
	failed := ReportPreflight(&buf, []CheckResult{{Name: "git", Status: statusOK, Detail: "git version 2.39.5"}})
	if failed {
		t.Fatal("failed = true, want false")
	}
	if strings.Contains(buf.String(), "required check") {
		t.Fatalf("healthy report must omit the summary line, got %q", buf.String())
	}
}

func TestReportPreflightPluralSummary(t *testing.T) {
	var buf bytes.Buffer
	ReportPreflight(&buf, []CheckResult{
		{Name: "gh", Status: statusFail, Detail: "not found"},
		{Name: "claude", Status: statusFail, Detail: "not found"},
	})
	if !strings.Contains(buf.String(), "2 required checks failed.") {
		t.Fatalf("report = %q, want a plural summary line", buf.String())
	}
}
