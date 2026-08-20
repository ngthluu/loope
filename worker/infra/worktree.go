package infra

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/ngthluu/loope/worker/shared"
)

// Worktree is the git adapter behind shared.Workspace.
type Worktree struct {
	runner   shared.Runner
	repoPath string
	retry    shared.RetryPolicy
	mu       sync.Mutex // serializes repo-level git ops (fetch/worktree/branch) across parallel goroutines
}

var _ shared.Workspace = (*Worktree)(nil)

// NewWorktree builds the git adapter for cfg's repository.
func NewWorktree(r shared.Runner, cfg *shared.Config) *Worktree {
	return &Worktree{runner: r, repoPath: cfg.RepoPath, retry: cfg.GitHubRetry.Policy()}
}

func (w *Worktree) git(ctx context.Context, dir string, args ...string) (string, error) {
	stdout, stderr, err := w.runner.Run(ctx, dir, nil, "", "git", args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args[:min(2, len(args))], " "), err, shared.Tail(stderr, 300))
	}
	return stdout, nil
}

func (w *Worktree) DefaultBranch(ctx context.Context) (string, error) {
	out, err := w.git(ctx, w.repoPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "origin/"), nil
}

func (w *Worktree) Create(ctx context.Context, workDir string, issueNum int, baseBranch string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return "", err
	}
	if err := w.fetchLocked(ctx); err != nil {
		return "", err
	}
	path := shared.WorktreePath(workDir, issueNum)
	branch := shared.BranchName(issueNum)
	_, err := w.git(ctx, w.repoPath, "worktree", "add", path, "-b", branch, "origin/"+baseBranch)
	if err != nil {
		// A leftover from a crashed or aborted prior run blocks the add ("a branch
		// named '...' already exists" / "'...' already exists"). Create only runs
		// for fresh eligible picks — a live pipeline holds the mutex and rework
		// reuses its own preserved worktree without ever routing here — so the
		// leftover is stale. If its worktree still exists, reuse it so partial
		// progress is continued rather than discarded. Otherwise only a bare branch
		// remains (worktree gone): reclaim it best-effort and retry the add once.
		if _, statErr := os.Stat(path); statErr == nil {
			return path, nil
		}
		// Only a stale branch/worktree collision ("already exists") is reclaimable.
		// Any other add failure (bad base branch, transient git error) is a real
		// error for this fresh pick — return it rather than force-deleting on an
		// unrelated failure, which would be reacting to any error instead of the
		// specific condition that makes reuse impossible.
		if !strings.Contains(err.Error(), "already exists") {
			return "", err
		}
		_, _ = w.git(ctx, w.repoPath, "worktree", "remove", "--force", path)
		_, _ = w.git(ctx, w.repoPath, "worktree", "prune")
		_, _ = w.git(ctx, w.repoPath, "branch", "-D", branch)
		if _, err = w.git(ctx, w.repoPath, "worktree", "add", path, "-b", branch, "origin/"+baseBranch); err != nil {
			return "", err
		}
	}
	return path, nil
}

func (w *Worktree) Remove(ctx context.Context, path string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.git(ctx, w.repoPath, "worktree", "remove", "--force", path)
	return err
}

func (w *Worktree) DeleteBranch(ctx context.Context, branch string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.git(ctx, w.repoPath, "branch", "-D", branch)
	return err
}

// fetchLocked fetches origin with pruning, retried on transient errors.
// Callers must hold w.mu.
func (w *Worktree) fetchLocked(ctx context.Context) error {
	return w.retry.Do(ctx, shared.IsTransientGitHubError, func() error {
		_, e := w.git(ctx, w.repoPath, "fetch", "origin", "--prune")
		return e
	})
}

// Fetch is the self-locking form of fetchLocked, for callers outside the
// Create path (the merge-resolve flow refreshes origin/<base> before merging).
func (w *Worktree) Fetch(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fetchLocked(ctx)
}

// Merge merges ref into wtPath's checked-out branch. --no-edit keeps the
// default merge-commit message without ever opening an editor. A conflicted
// merge is a non-zero git exit, so it surfaces as a non-nil error — callers
// tell it apart from a genuine failure via HasUnmergedPaths. No w.mu: like
// Push/CommitCount it acts on one issue's worktree, which the orchestrator
// already confines to a single goroutine.
func (w *Worktree) Merge(ctx context.Context, wtPath, ref string) error {
	_, err := w.git(ctx, wtPath, "merge", "--no-edit", ref)
	return err
}

// HasUnmergedPaths reports whether wtPath has unresolved conflict entries,
// via `git ls-files --unmerged` (any output means at least one).
func (w *Worktree) HasUnmergedPaths(ctx context.Context, wtPath string) (bool, error) {
	out, err := w.git(ctx, wtPath, "ls-files", "--unmerged")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// MergeInProgress reports whether wtPath has an unconcluded merge (MERGE_HEAD
// present). Boolean-only: git's exit 1 for "no such ref" is the expected
// negative, not an error worth propagating.
func (w *Worktree) MergeInProgress(ctx context.Context, wtPath string) bool {
	_, err := w.git(ctx, wtPath, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	return err == nil
}

func (w *Worktree) Push(ctx context.Context, wtPath, branch string) error {
	return w.retry.Do(ctx, shared.IsTransientGitHubError, func() error {
		_, e := w.git(ctx, wtPath, "push", "-u", "origin", branch)
		return e
	})
}

func (w *Worktree) CommitCount(ctx context.Context, wtPath, baseBranch string) (int, error) {
	out, err := w.git(ctx, wtPath, "rev-list", "--count", "origin/"+baseBranch+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// NewWorktreeAt builds a git adapter for an explicit repo path and retry
// policy — used by tests, which need near-instant backoff.
func NewWorktreeAt(r shared.Runner, repoPath string, retry shared.RetryPolicy) *Worktree {
	return &Worktree{runner: r, repoPath: repoPath, retry: retry}
}
