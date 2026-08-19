package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// codeReviewBeginSentinel / codeReviewEndSentinel fence the status line and
	// summary inside the session's result text. Injected into the prompt via
	// promptData(), never hardcoded in a template.
	codeReviewBeginSentinel = "CODEREVIEW_BEGIN"
	codeReviewEndSentinel   = "CODEREVIEW_END"
	// codeReviewRoundFile holds the last completed round number, so a daemon
	// restart mid-loop resumes at the next round instead of redoing finished
	// ones (the CLAUDE.md "continue from existing state" principle).
	codeReviewRoundFile = "codereview-round"
)

// codeReviewStatus is the outcome a review-and-fix session reports for one
// round, parsed from its STATUS: line.
type codeReviewStatus string

const (
	codeReviewClean   codeReviewStatus = "clean"
	codeReviewFixed   codeReviewStatus = "fixed"
	codeReviewBlocked codeReviewStatus = "blocked"
)

// CodeReviewTarget is the GitHub surface the review loop reads the PR number
// from and posts findings to. *GitHub satisfies it; tests substitute a fake.
type CodeReviewTarget interface {
	PRNumberForBranch(ctx context.Context, branch string) (int, error)
	ReviewComment(ctx context.Context, prNumber int, body string) error
}

// CodeReview runs the post-ship review-and-fix loop: one or more Claude
// sessions that invoke /code-review --fix against the shipped diff, push
// whatever they commit, and post the finding as a PR review comment. A nil
// *CodeReview, a nil Target, or a nil cfg.Models.CodeReview all disable the
// step entirely, so callers never need a nil guard.
type CodeReview struct {
	Target CodeReviewTarget
	// Push pushes wtPath's branch to origin. Set to (*Worktree).Push by the
	// wiring in ship() — injected the same way Target is, so tests can fake it
	// without a real Worktree/Runner.
	Push func(ctx context.Context, wtPath, branch string) error
	Num  int
}

// Run drives the round loop: resolve the PR once, then for each round from
// lastCompletedRound(logDir)+1 through cfg.Models.CodeReview.Rounds (<=0
// treated as 1), run one Claude session, push whatever it committed, parse
// its status, post the finding, and persist progress. It stops early on
// STATUS: clean or STATUS: blocked, and stops (returning an error) if the PR
// lookup, the Claude call, or the push fails — but every one of those errors
// is the caller's (ship()'s) to log, never to propagate into a park: the PR
// already shipped successfully.
func (r *CodeReview) Run(ctx context.Context, c *Claude, cfg *Config, wtPath, branch, base, logDir string) error {
	if r == nil || r.Target == nil || cfg.Models.CodeReview == nil {
		return nil
	}
	rounds := cfg.Models.CodeReview.Rounds
	if rounds <= 0 {
		rounds = 1
	}
	prNum, err := r.Target.PRNumberForBranch(ctx, branch)
	if err != nil {
		return fmt.Errorf("issue #%d: code review PR lookup failed: %w", r.Num, err)
	}
	for i := lastCompletedRound(logDir) + 1; i <= rounds; i++ {
		res, err := c.Call(ctx, ClaudeCall{
			Dir: wtPath, Label: fmt.Sprintf("codereview-%d", i), Prompt: codeReviewPrompt(i, rounds, base),
			Model:           cfg.Models.CodeReview.ModelConfig,
			SkipPermissions: true,
		})
		if err != nil {
			return fmt.Errorf("issue #%d: code review round %d session failed: %w", r.Num, i, err)
		}
		if err := r.Push(ctx, wtPath, branch); err != nil {
			return fmt.Errorf("issue #%d: code review round %d push failed: %w", r.Num, i, err)
		}
		status, summary := parseCodeReview(res.Result)
		if err := r.Target.ReviewComment(ctx, prNum, codeReviewComment(i, rounds, status, summary)); err != nil {
			log.Printf("issue #%d: code review round %d comment failed: %v", r.Num, i, err)
		}
		recordCodeReviewRound(logDir, i)
		if status != codeReviewFixed {
			// clean: nothing left to do. blocked: the session says it can't
			// safely fix the finding, so another round won't help either.
			break
		}
	}
	return nil
}

// parseCodeReview extracts the status and summary from a review session's
// result: the text between codeReviewBeginSentinel/codeReviewEndSentinel,
// whose first line is "STATUS: clean|fixed|blocked" and the rest is the
// summary. A missing fence, a missing/unrecognized STATUS line, is treated as
// blocked with the raw result as the summary, so an off-contract session
// still surfaces something rather than being silently dropped.
func parseCodeReview(s string) (codeReviewStatus, string) {
	i := strings.Index(s, codeReviewBeginSentinel)
	if i < 0 {
		return codeReviewBlocked, strings.TrimSpace(s)
	}
	rest := s[i+len(codeReviewBeginSentinel):]
	if j := strings.Index(rest, codeReviewEndSentinel); j >= 0 {
		rest = rest[:j]
	}
	rest = strings.TrimSpace(rest)
	lines := strings.SplitN(rest, "\n", 2)
	first := strings.TrimSpace(lines[0])
	statusText, ok := strings.CutPrefix(first, "STATUS:")
	if !ok {
		return codeReviewBlocked, strings.TrimSpace(s)
	}
	summary := ""
	if len(lines) > 1 {
		summary = strings.TrimSpace(lines[1])
	}
	switch strings.TrimSpace(statusText) {
	case string(codeReviewClean):
		return codeReviewClean, summary
	case string(codeReviewFixed):
		return codeReviewFixed, summary
	case string(codeReviewBlocked):
		return codeReviewBlocked, summary
	default:
		return codeReviewBlocked, strings.TrimSpace(s)
	}
}

// recordCodeReviewRound writes the last completed round number to
// <logDir>/codereview-round. Best-effort, like the other log-writers in
// tracker.go: a no-op on an empty logDir.
func recordCodeReviewRound(logDir string, i int) {
	if logDir == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, codeReviewRoundFile), []byte(strconv.Itoa(i)), 0o644)
}

// lastCompletedRound reads the round progress written by
// recordCodeReviewRound, or 0 if none is recorded (fresh loop, or the file is
// missing/corrupt).
func lastCompletedRound(logDir string) int {
	b, err := os.ReadFile(filepath.Join(logDir, codeReviewRoundFile))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}
