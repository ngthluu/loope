package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type Worktree struct {
	runner   Runner
	repoPath string
	retry    RetryPolicy
	mu       sync.Mutex // serializes repo-level git ops (fetch/worktree/branch) across parallel goroutines
}

func branchName(issueNum int) string {
	return fmt.Sprintf("ai/issue-%d", issueNum)
}

// worktreePath is the deterministic worktree location for an issue, shared by
// Create and the rework command so both agree on where the worktree lives.
func worktreePath(workDir string, issueNum int) string {
	return filepath.Join(workDir, fmt.Sprintf("issue-%d", issueNum))
}

func (w *Worktree) git(ctx context.Context, dir string, args ...string) (string, error) {
	stdout, stderr, err := w.runner.Run(ctx, dir, nil, "", "git", args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args[:min(2, len(args))], " "), err, tail(stderr, 300))
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
	if err := w.retry.do(ctx, isTransientGitHubError, func() error {
		_, e := w.git(ctx, w.repoPath, "fetch", "origin", "--prune")
		return e
	}); err != nil {
		return "", err
	}
	path := worktreePath(workDir, issueNum)
	branch := branchName(issueNum)
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

func (w *Worktree) Push(ctx context.Context, wtPath, branch string) error {
	return w.retry.do(ctx, isTransientGitHubError, func() error {
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
