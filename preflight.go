package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
	statusSkip
)

// CheckResult is one preflight check's outcome. Fix holds remediation
// commands, printed only when Status is not statusOK.
type CheckResult struct {
	Name   string
	Status checkStatus
	Detail string
	Fix    []string
}

// probeTimeout bounds each individual probe so one hung `gh` cannot stall
// startup indefinitely.
const probeTimeout = 10 * time.Second

var (
	fixGit    = []string{"brew install git  (macOS)", "apt install git  (Debian/Ubuntu)", "https://git-scm.com/downloads"}
	fixGH     = []string{"brew install gh  (macOS)", "https://cli.github.com"}
	fixClaude = []string{"npm install -g @anthropic-ai/claude-code", "https://docs.anthropic.com/en/docs/claude-code"}
)

// probe runs one command under its own timeout derived from ctx and returns
// trimmed stdout. On failure the error names the timeout or carries the first
// line of stderr, which is what the report shows the user.
func probe(ctx context.Context, r Runner, dir string, env []string, name string, args ...string) (string, error) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	stdout, stderr, err := r.Run(pctx, dir, env, "", name, args...)
	if err != nil {
		if pctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("timed out after %s", probeTimeout)
		}
		if msg := firstLine(stderr); msg != "" {
			return "", fmt.Errorf("%s", msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// binaryCheck probes for an installed binary by running its version command.
func binaryCheck(ctx context.Context, r Runner, name string, fix []string, args ...string) CheckResult {
	out, err := probe(ctx, r, "", nil, name, args...)
	if err != nil {
		return CheckResult{Name: name, Status: statusFail, Detail: "not found: " + err.Error(), Fix: fix}
	}
	return CheckResult{Name: name, Status: statusOK, Detail: firstLine(out)}
}

var fixSuperpowers = []string{"claude plugin install superpowers@claude-plugins-official"}

// skipIfBlocked returns a statusSkip result naming the first blocker that did
// not pass. A skipped check is never fatal on its own — its blocker already is.
func skipIfBlocked(name string, blockers ...CheckResult) (CheckResult, bool) {
	for _, b := range blockers {
		if b.Status == statusFail || b.Status == statusSkip {
			return CheckResult{Name: name, Status: statusSkip, Detail: fmt.Sprintf("skipped (%s failed)", b.Name)}, true
		}
	}
	return CheckResult{}, false
}

func checkGHAuth(ctx context.Context, r Runner, gh CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("gh auth", gh); skipped {
		return res
	}
	out, err := probe(ctx, r, "", nil, "gh", "auth", "status")
	if err != nil {
		return CheckResult{Name: "gh auth", Status: statusFail, Detail: "not authenticated", Fix: []string{"gh auth login"}}
	}
	detail := firstLine(out)
	if detail == "" {
		detail = "authenticated"
	}
	return CheckResult{Name: "gh auth", Status: statusOK, Detail: detail}
}

// checkSuperpowers verifies the superpowers plugin is installed in the *same*
// Claude profile the pipeline runs under: without CLAUDE_CONFIG_DIR a user on a
// dedicated profile would get a false pass from their default ~/.claude.
func checkSuperpowers(ctx context.Context, r Runner, cfg *Config, claude CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("superpowers", claude); skipped {
		return res
	}
	var env []string
	if cfg.ClaudeConfigDir != "" {
		env = []string{"CLAUDE_CONFIG_DIR=" + cfg.ClaudeConfigDir}
	}
	out, err := probe(ctx, r, "", env, "claude", "plugin", "list")
	if err != nil {
		return CheckResult{Name: "superpowers", Status: statusFail, Detail: "claude plugin list failed: " + err.Error(), Fix: fixSuperpowers}
	}
	if !strings.Contains(out, "superpowers@") {
		detail := "plugin not installed"
		if cfg.ClaudeConfigDir != "" {
			detail += " (CLAUDE_CONFIG_DIR=" + cfg.ClaudeConfigDir + ")"
		}
		return CheckResult{Name: "superpowers", Status: statusFail, Detail: detail, Fix: fixSuperpowers}
	}
	return CheckResult{Name: "superpowers", Status: statusOK, Detail: "installed"}
}

func checkRepoPath(ctx context.Context, r Runner, cfg *Config, git CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("repoPath", git); skipped {
		return res
	}
	out, err := probe(ctx, r, cfg.RepoPath, nil, "git", "rev-parse", "--is-inside-work-tree")
	if err != nil || out != "true" {
		return CheckResult{
			Name:   "repoPath",
			Status: statusFail,
			Detail: fmt.Sprintf("%s is not a git worktree", cfg.RepoPath),
			Fix:    []string{"git clone <your-repo> " + cfg.RepoPath, "or point repoPath at an existing clone in your config"},
		}
	}
	return CheckResult{Name: "repoPath", Status: statusOK, Detail: cfg.RepoPath}
}

func checkRepoAccess(ctx context.Context, r Runner, cfg *Config, gh, ghAuth CheckResult) CheckResult {
	if res, skipped := skipIfBlocked("repo access", gh, ghAuth); skipped {
		return res
	}
	if _, err := probe(ctx, r, "", nil, "gh", "repo", "view", cfg.RepoSlug, "--json", "name"); err != nil {
		return CheckResult{
			Name:   "repo access",
			Status: statusFail,
			Detail: fmt.Sprintf("cannot access %s: %v", cfg.RepoSlug, err),
			Fix:    []string{"gh auth refresh -h github.com -s repo", "or fix repoSlug in your config"},
		}
	}
	return CheckResult{Name: "repo access", Status: statusOK, Detail: cfg.RepoSlug}
}

// Preflight runs every check in order and returns the results.
func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult {
	git := binaryCheck(ctx, r, "git", fixGit, "--version")
	gh := binaryCheck(ctx, r, "gh", fixGH, "--version")
	ghAuth := checkGHAuth(ctx, r, gh)
	claude := binaryCheck(ctx, r, "claude", fixClaude, "--version")
	superpowers := checkSuperpowers(ctx, r, cfg, claude)
	repoPath := checkRepoPath(ctx, r, cfg, git)
	access := checkRepoAccess(ctx, r, cfg, gh, ghAuth)
	return []CheckResult{git, gh, ghAuth, claude, superpowers, repoPath, access}
}
