package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// joinedCalls returns every recorded call's args, one per line, for assertions.
func joinedCalls(f *fakeRunner) string {
	var b strings.Builder
	for _, c := range f.calls {
		b.WriteString(strings.Join(c.args, " ") + "\n")
	}
	return b.String()
}

func TestBranchName(t *testing.T) {
	if got := branchName(42); got != "ai/issue-42" {
		t.Errorf("branchName = %q", got)
	}
}

func TestDefaultBranch(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: "origin/main\n"}}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	got, err := w.DefaultBranch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Errorf("DefaultBranch = %q, want main", got)
	}
	c := f.calls[0]
	if c.name != "git" || c.dir != "/clone" || !hasArg(c.args, "symbolic-ref") {
		t.Errorf("call = %+v", c)
	}
}

func TestCreateFetchesThenAddsWorktree(t *testing.T) {
	f := &fakeRunner{}
	w := &Worktree{runner: f, repoPath: "/clone"}
	workDir := t.TempDir()
	path, err := w.Create(context.Background(), workDir, 7, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workDir, "issue-7")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %+v, want fetch then worktree add", f.calls)
	}
	if !hasArg(f.calls[0].args, "fetch") {
		t.Errorf("first call = %+v, want fetch", f.calls[0])
	}
	add := f.calls[1]
	joined := strings.Join(add.args, " ")
	if !strings.Contains(joined, "worktree add") || !strings.Contains(joined, want) ||
		argAfter(add.args, "-b") != "ai/issue-7" || !hasArg(add.args, "origin/main") {
		t.Errorf("worktree add args = %v", add.args)
	}
}

func TestCreateReclaimsStaleBranchAndRetries(t *testing.T) {
	// fetch ok; first worktree add fails because a stale branch survives a
	// crashed prior run; cleanup (best-effort) then a retried add succeeds.
	f := &fakeRunner{queue: []rresp{
		{stdout: ""}, // fetch
		{err: errors.New("exit 255"), stderr: "fatal: a branch named 'ai/issue-7' already exists"}, // worktree add
		{stdout: ""}, // worktree remove --force (best-effort)
		{stdout: ""}, // worktree prune
		{stdout: ""}, // branch -D
		{stdout: ""}, // worktree add (retry)
	}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	workDir := t.TempDir()
	path, err := w.Create(context.Background(), workDir, 7, "main")
	if err != nil {
		t.Fatalf("Create should recover from a stale branch, got %v", err)
	}
	if want := filepath.Join(workDir, "issue-7"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	joined := joinedCalls(f)
	if !strings.Contains(joined, "branch -D ai/issue-7") {
		t.Errorf("expected stale branch delete, calls:\n%s", joined)
	}
	if n := strings.Count(joined, "worktree add"); n != 2 {
		t.Errorf("worktree add count = %d, want 2 (initial + retry)", n)
	}
}

func TestCreateReusesExistingWorktree(t *testing.T) {
	// fetch ok; worktree add fails because a worktree from an interrupted prior
	// run still occupies the path — Create should reuse it (continue working on
	// it), not delete the branch or recreate it.
	f := &fakeRunner{queue: []rresp{
		{stdout: ""}, // fetch
		{err: errors.New("exit 128"), stderr: "fatal: 'issue-7' already exists"}, // worktree add
	}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "issue-7"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := w.Create(context.Background(), workDir, 7, "main")
	if err != nil {
		t.Fatalf("Create should reuse an existing worktree, got %v", err)
	}
	if want := filepath.Join(workDir, "issue-7"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	joined := joinedCalls(f)
	if strings.Contains(joined, "branch -D") {
		t.Errorf("must not delete the branch when reusing a worktree, calls:\n%s", joined)
	}
	if n := strings.Count(joined, "worktree add"); n != 1 {
		t.Errorf("worktree add count = %d, want 1 (reuse, no reclaim/retry)", n)
	}
}

func TestCreateReturnsUnrelatedAddError(t *testing.T) {
	// worktree add fails for a reason that is NOT a stale-branch collision, and no
	// worktree exists at the path — Create must surface the error rather than
	// force-deleting on an unrelated failure.
	f := &fakeRunner{queue: []rresp{
		{stdout: ""}, // fetch
		{err: errors.New("exit 128"), stderr: "fatal: invalid reference: origin/nope"}, // worktree add
	}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	_, err := w.Create(context.Background(), t.TempDir(), 7, "nope")
	if err == nil {
		t.Fatal("want error surfaced for an unrelated worktree add failure")
	}
	joined := joinedCalls(f)
	if strings.Contains(joined, "branch -D") || strings.Contains(joined, "worktree remove") {
		t.Errorf("must not reclaim on an unrelated failure, calls:\n%s", joined)
	}
	if n := strings.Count(joined, "worktree add"); n != 1 {
		t.Errorf("worktree add count = %d, want 1 (no reclaim/retry)", n)
	}
}

func TestCreateReclaimRetryStillFails(t *testing.T) {
	// Stale branch triggers reclaim, but the retried add fails too (the condition
	// genuinely can't be fixed) — Create must return the terminal error.
	f := &fakeRunner{queue: []rresp{
		{stdout: ""}, // fetch
		{err: errors.New("exit 128"), stderr: "fatal: a branch named 'ai/issue-7' already exists"}, // add
		{stdout: ""}, // worktree remove --force (best-effort)
		{stdout: ""}, // worktree prune
		{stdout: ""}, // branch -D
		{err: errors.New("exit 128"), stderr: "fatal: a branch named 'ai/issue-7' already exists"}, // add retry
	}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	_, err := w.Create(context.Background(), t.TempDir(), 7, "main")
	if err == nil {
		t.Fatal("want terminal error when the reclaim retry also fails")
	}
	if n := strings.Count(joinedCalls(f), "worktree add"); n != 2 {
		t.Errorf("worktree add count = %d, want 2 (initial + one retry)", n)
	}
}

func TestRemoveAndDeleteBranch(t *testing.T) {
	f := &fakeRunner{}
	w := &Worktree{runner: f, repoPath: "/clone"}
	if err := w.Remove(context.Background(), "/work/issue-7"); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteBranch(context.Background(), "ai/issue-7"); err != nil {
		t.Fatal(err)
	}
	rm := strings.Join(f.calls[0].args, " ")
	if !strings.Contains(rm, "worktree remove --force /work/issue-7") {
		t.Errorf("remove args = %q", rm)
	}
	del := strings.Join(f.calls[1].args, " ")
	if !strings.Contains(del, "branch -D ai/issue-7") {
		t.Errorf("delete args = %q", del)
	}
}

func TestPushRunsInWorktree(t *testing.T) {
	f := &fakeRunner{}
	w := &Worktree{runner: f, repoPath: "/clone"}
	if err := w.Push(context.Background(), "/work/issue-7", "ai/issue-7"); err != nil {
		t.Fatal(err)
	}
	c := f.calls[0]
	if c.dir != "/work/issue-7" || !hasArg(c.args, "push") || !hasArg(c.args, "ai/issue-7") {
		t.Errorf("push call = %+v", c)
	}
}

func TestPushRetriesTransientFailure(t *testing.T) {
	f := &fakeRunner{queue: []rresp{
		{err: errors.New("exit 1"), stderr: "Connection reset by peer"},
		{stdout: ""},
	}}
	w := &Worktree{runner: f, repoPath: "/clone",
		retry: RetryPolicy{MaxAttempts: 3, BaseDelay: time.Microsecond, MaxDelay: time.Microsecond}}
	if err := w.Push(context.Background(), "/work/issue-7", "ai/issue-7"); err != nil {
		t.Fatalf("want success after one retry, got %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (fail then retry-success)", len(f.calls))
	}
}

func TestCommitCount(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: "3\n"}}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	n, err := w.CommitCount(context.Background(), "/work/issue-7", "main")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	joined := strings.Join(f.calls[0].args, " ")
	if !strings.Contains(joined, "rev-list --count origin/main..HEAD") {
		t.Errorf("args = %q", joined)
	}
}
