package infra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// joinedCalls returns every recorded call's args, one per line, for assertions.
func joinedCalls(f *testkit.FakeRunner) string {
	var b strings.Builder
	for _, c := range f.Calls {
		b.WriteString(strings.Join(c.Args, " ") + "\n")
	}
	return b.String()
}

func TestBranchName(t *testing.T) {
	if got := shared.BranchName(42); got != "ai/issue-42" {
		t.Errorf("branchName = %q", got)
	}
}

func TestDefaultBranch(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "origin/main\n"}}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	got, err := w.DefaultBranch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Errorf("DefaultBranch = %q, want main", got)
	}
	c := f.Calls[0]
	if c.Name != "git" || c.Dir != "/clone" || !testkit.HasArg(c.Args, "symbolic-ref") {
		t.Errorf("call = %+v", c)
	}
}

func TestCreateFetchesThenAddsWorktree(t *testing.T) {
	f := &testkit.FakeRunner{}
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
	if len(f.Calls) != 2 {
		t.Fatalf("calls = %+v, want fetch then worktree add", f.Calls)
	}
	if !testkit.HasArg(f.Calls[0].Args, "fetch") {
		t.Errorf("first call = %+v, want fetch", f.Calls[0])
	}
	add := f.Calls[1]
	joined := strings.Join(add.Args, " ")
	if !strings.Contains(joined, "worktree add") || !strings.Contains(joined, want) ||
		testkit.ArgAfter(add.Args, "-b") != "ai/issue-7" || !testkit.HasArg(add.Args, "origin/main") {
		t.Errorf("worktree add args = %v", add.Args)
	}
}

func TestCreateReclaimsStaleBranchAndRetries(t *testing.T) {
	// fetch ok; first worktree add fails because a stale branch survives a
	// crashed prior run; cleanup (best-effort) then a retried add succeeds.
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Stdout: ""}, // fetch
		{Err: errors.New("exit 255"), Stderr: "fatal: a branch named 'ai/issue-7' already exists"}, // worktree add
		{Stdout: ""}, // worktree remove --force (best-effort)
		{Stdout: ""}, // worktree prune
		{Stdout: ""}, // branch -D
		{Stdout: ""}, // worktree add (retry)
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
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Stdout: ""}, // fetch
		{Err: errors.New("exit 128"), Stderr: "fatal: 'issue-7' already exists"}, // worktree add
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
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Stdout: ""}, // fetch
		{Err: errors.New("exit 128"), Stderr: "fatal: invalid reference: origin/nope"}, // worktree add
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
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Stdout: ""}, // fetch
		{Err: errors.New("exit 128"), Stderr: "fatal: a branch named 'ai/issue-7' already exists"}, // add
		{Stdout: ""}, // worktree remove --force (best-effort)
		{Stdout: ""}, // worktree prune
		{Stdout: ""}, // branch -D
		{Err: errors.New("exit 128"), Stderr: "fatal: a branch named 'ai/issue-7' already exists"}, // add retry
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
	f := &testkit.FakeRunner{}
	w := &Worktree{runner: f, repoPath: "/clone"}
	if err := w.Remove(context.Background(), "/work/issue-7"); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteBranch(context.Background(), "ai/issue-7"); err != nil {
		t.Fatal(err)
	}
	rm := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(rm, "worktree remove --force /work/issue-7") {
		t.Errorf("remove args = %q", rm)
	}
	del := strings.Join(f.Calls[1].Args, " ")
	if !strings.Contains(del, "branch -D ai/issue-7") {
		t.Errorf("delete args = %q", del)
	}
}

func TestPushRunsInWorktree(t *testing.T) {
	f := &testkit.FakeRunner{}
	w := &Worktree{runner: f, repoPath: "/clone"}
	if err := w.Push(context.Background(), "/work/issue-7", "ai/issue-7"); err != nil {
		t.Fatal(err)
	}
	c := f.Calls[0]
	if c.Dir != "/work/issue-7" || !testkit.HasArg(c.Args, "push") || !testkit.HasArg(c.Args, "ai/issue-7") {
		t.Errorf("push call = %+v", c)
	}
}

func TestPushRetriesTransientFailure(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Err: errors.New("exit 1"), Stderr: "Connection reset by peer"},
		{Stdout: ""},
	}}
	w := &Worktree{runner: f, repoPath: "/clone",
		retry: shared.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Microsecond, MaxDelay: time.Microsecond}}
	if err := w.Push(context.Background(), "/work/issue-7", "ai/issue-7"); err != nil {
		t.Fatalf("want success after one retry, got %v", err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("calls = %d, want 2 (fail then retry-success)", len(f.Calls))
	}
}

func TestCommitCount(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "3\n"}}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	n, err := w.CommitCount(context.Background(), "/work/issue-7", "main")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	joined := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(joined, "rev-list --count origin/main..HEAD") {
		t.Errorf("args = %q", joined)
	}
}

func TestFetchFetchesOriginWithPrune(t *testing.T) {
	f := &testkit.FakeRunner{}
	w := &Worktree{runner: f, repoPath: "/clone"}
	if err := w.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := f.Calls[0]
	if c.Dir != "/clone" || !testkit.HasArg(c.Args, "fetch") || !testkit.HasArg(c.Args, "--prune") {
		t.Errorf("fetch call = %+v", c)
	}
}

func TestFetchRetriesTransientFailure(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Err: errors.New("exit 1"), Stderr: "Connection reset by peer"},
		{Stdout: ""},
	}}
	w := &Worktree{runner: f, repoPath: "/clone", retry: testkit.TestRetry}
	if err := w.Fetch(context.Background()); err != nil {
		t.Fatalf("want success after one retry, got %v", err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("calls = %d, want 2 (fail then retry-success)", len(f.Calls))
	}
}

func TestMergeRunsInWorktreeWithNoEdit(t *testing.T) {
	f := &testkit.FakeRunner{}
	w := &Worktree{runner: f, repoPath: "/clone"}
	if err := w.Merge(context.Background(), "/work/issue-7", "origin/main"); err != nil {
		t.Fatal(err)
	}
	c := f.Calls[0]
	if c.Dir != "/work/issue-7" || !testkit.HasArg(c.Args, "merge") || !testkit.HasArg(c.Args, "--no-edit") || !testkit.HasArg(c.Args, "origin/main") {
		t.Errorf("merge call = %+v", c)
	}
}

func TestHasUnmergedPaths(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Stdout: "100644 abc 1\tmain.go\n"},
		{Stdout: "\n"},
	}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	got, err := w.HasUnmergedPaths(context.Background(), "/work/issue-7")
	if err != nil || !got {
		t.Errorf("HasUnmergedPaths with conflict entries = %v, %v, want true", got, err)
	}
	got, err = w.HasUnmergedPaths(context.Background(), "/work/issue-7")
	if err != nil || got {
		t.Errorf("HasUnmergedPaths with empty output = %v, %v, want false", got, err)
	}
	if !testkit.HasArg(f.Calls[0].Args, "--unmerged") {
		t.Errorf("args = %v", f.Calls[0].Args)
	}
}

func TestMergeInProgress(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Stdout: "abc123\n"},
		{Err: errors.New("exit 1")},
	}}
	w := &Worktree{runner: f, repoPath: "/clone"}
	if !w.MergeInProgress(context.Background(), "/work/issue-7") {
		t.Error("MERGE_HEAD resolves: want MergeInProgress true")
	}
	if w.MergeInProgress(context.Background(), "/work/issue-7") {
		t.Error("rev-parse fails: want MergeInProgress false")
	}
	joined := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(joined, "rev-parse -q --verify MERGE_HEAD") {
		t.Errorf("args = %q", joined)
	}
}
