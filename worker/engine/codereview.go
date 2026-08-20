package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ngthluu/loope/worker/shared"
)

// codeReviewRoundFile holds the last completed round number, so a daemon
// restart mid-loop resumes at the next round instead of redoing finished
// ones (the CLAUDE.md "continue from existing state" principle).
const codeReviewRoundFile = "codereview-round"

// codeReviewResultSchema is the --json-schema for each review round's session.
const codeReviewResultSchema = `{
  "type": "object",
  "properties": {
    "status": {
      "type": "string",
      "enum": ["clean", "fixed", "blocked"],
      "description": "clean: /code-review found nothing to fix. fixed: fixes were applied and committed. blocked: a finding can't be safely auto-fixed."
    },
    "summary": {
      "type": "string",
      "description": "Short bullet summary of what was fixed, or explanation of what is blocked. Empty when clean."
    }
  },
  "required": ["status", "summary"]
}`

// codeReviewStatus is the outcome a review-and-fix session reports for one
// round, from its structured output's status field.
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
	// Kind is the issue's pipeline kind ("feature"/"bug"), recorded alongside
	// each review session so a parked review resumes with the right kind.
	Kind string
}

// Run drives the round loop: resolve the PR once, then for each round from
// lastCompletedRound(logDir)+1 through cfg.Models.CodeReview.Rounds (<=0
// treated as 1), run one Claude session, push whatever it committed, parse
// its status, post the finding, and persist progress. It stops early on
// STATUS: clean or STATUS: blocked, and stops (returning an error) if the PR
// lookup, the Claude call, or the push fails — the caller (ship()) parks that
// error like any other failure, so a human decides when to continue.
//
// Code review is a recorded session stage (shared.StageCodeReview): every round's
// session is persisted, and when logDir already holds a codereview-stage
// session (a parked or crashed review), the first round Run executes resumes
// THAT session with --resume and "continue" instead of starting a fresh
// round session — the round counter only ever names completed rounds, so the
// cut-short round is continued, never skipped.
func (r *CodeReview) Run(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, branch, base, logDir string) error {
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
	resume := ""
	if node, ok := shared.ResumePoint(logDir); ok && node.Stage == shared.StageCodeReview {
		resume = node.ID
	}
	for i := lastCompletedRound(logDir) + 1; i <= rounds; i++ {
		call := shared.ClaudeCall{
			Dir: wtPath, Label: fmt.Sprintf("codereview-%d", i), Prompt: codeReviewPrompt(i, rounds, base),
			Model:           cfg.Models.CodeReview.ModelConfig,
			SkipPermissions: true,
			JSONSchema:      codeReviewResultSchema,
			// Each round is a primary session: checkpointed in-flight, so even
			// a round killed mid-run (429, crash) is the chain head the
			// post-park re-entry resumes.
			Checkpoint: &shared.CallCheckpoint{Kind: r.Kind, Stage: shared.StageCodeReview},
		}
		if resume != "" {
			// Re-entry into a cut-short round: continue its session in place.
			call.Label = fmt.Sprintf("codereview-%d-resume", i)
			call.Prompt = "continue"
			call.Resume = resume
			resume = ""
		}
		res, err := c.Call(ctx, call)
		if err != nil {
			return fmt.Errorf("issue #%d: code review round %d session failed: %w", r.Num, i, err)
		}
		if err := r.Push(ctx, wtPath, branch); err != nil {
			return fmt.Errorf("issue #%d: code review round %d push failed: %w", r.Num, i, err)
		}
		status, summary := parseCodeReview(res)
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
// schema-validated structured output. An undecodable or unrecognized result
// is treated as blocked with the raw result text as the summary, so an
// off-contract session still surfaces something rather than being silently
// dropped.
func parseCodeReview(res *shared.ClaudeResult) (codeReviewStatus, string) {
	var cr struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if json.Unmarshal(res.StructuredOutput, &cr) != nil {
		return codeReviewBlocked, strings.TrimSpace(res.Result)
	}
	switch codeReviewStatus(cr.Status) {
	case codeReviewClean, codeReviewFixed, codeReviewBlocked:
		return codeReviewStatus(cr.Status), strings.TrimSpace(cr.Summary)
	default:
		return codeReviewBlocked, strings.TrimSpace(res.Result)
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
