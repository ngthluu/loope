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

// Preflight runs every check in order and returns the results.
func Preflight(ctx context.Context, r Runner, cfg *Config) []CheckResult {
	git := binaryCheck(ctx, r, "git", fixGit, "--version")
	gh := binaryCheck(ctx, r, "gh", fixGH, "--version")
	claude := binaryCheck(ctx, r, "claude", fixClaude, "--version")
	return []CheckResult{git, gh, claude}
}
