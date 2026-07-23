package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func testGitHub(f *fakeRunner) *GitHub {
	return NewGitHub(f, &Config{RepoPath: "/clone", RepoSlug: "org/repo", StateLabels: defaultStateLabels()})
}

func TestHasStateLabelExcludesStopped(t *testing.T) {
	g := NewGitHub(&fakeRunner{}, &Config{RepoSlug: "o/r", StateLabels: defaultStateLabels()})
	is := Issue{Number: 5, Labels: []Label{{Name: "ai-agent"}, {Name: "ai-stopped"}}}
	if !g.hasStateLabel(is) {
		t.Fatal("an issue carrying ai-stopped must count as having a state label (dropped from the eligible queue)")
	}
}

func TestHasStateLabelIncludesNeedsInfo(t *testing.T) {
	g := &GitHub{state: defaultStateLabels()}
	is := Issue{Labels: []Label{{Name: "ai-agent"}, {Name: "ai-needs-info"}}}
	if !g.hasStateLabel(is) {
		t.Error("an issue labeled ai-needs-info must be treated as having a state label")
	}
}

func TestListEligibleIssuesFiltersStateLabels(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `[
		{"number": 1, "title": "A", "body": "a", "labels": [{"name": "ai-agent"}]},
		{"number": 2, "title": "B", "body": "b", "labels": [{"name": "ai-agent"}, {"name": "ai-wip"}]},
		{"number": 3, "title": "C", "body": "c", "labels": [{"name": "ai-agent"}, {"name": "ai-failed"}]},
		{"number": 4, "title": "D", "body": "d", "labels": [{"name": "ai-agent"}, {"name": "ai-done"}]}
	]`}}}
	g := testGitHub(f)
	issues, err := g.ListEligibleIssues(context.Background(), "ai-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Number != 1 {
		t.Fatalf("issues = %+v, want only #1", issues)
	}
	call := f.calls[0]
	if call.name != "gh" || !hasArg(call.args, "ai-agent") || !hasArg(call.args, "--json") {
		t.Errorf("call = %+v", call)
	}
	if got := argAfter(call.args, "--repo"); got != "org/repo" {
		t.Errorf("--repo = %q", got)
	}
}

func TestListEligibleIssuesFiltersConfiguredStateLabels(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `[
		{"number": 1, "title": "A", "body": "a", "labels": [{"name": "ai-agent"}, {"name": "bot-wip"}]},
		{"number": 2, "title": "B", "body": "b", "labels": [{"name": "ai-agent"}, {"name": "bot-done"}]},
		{"number": 3, "title": "C", "body": "c", "labels": [{"name": "ai-agent"}, {"name": "ai-wip"}]}
	]`}}}
	g := NewGitHub(f, &Config{RepoPath: "/clone", RepoSlug: "org/repo",
		StateLabels: StateLabels{WIP: "bot-wip", Failed: "bot-failed", Done: "bot-done"}})
	issues, err := g.ListEligibleIssues(context.Background(), "ai-agent")
	if err != nil {
		t.Fatal(err)
	}
	// #1 and #2 carry configured state labels; #3's "ai-wip" is just an
	// ordinary label under this config and must not be filtered.
	if len(issues) != 1 || issues[0].Number != 3 {
		t.Fatalf("issues = %+v, want only #3", issues)
	}
}

func TestListIssuesWithLabelNoStateFilter(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `[
		{"number": 3, "title": "parked", "labels": [{"name": "ai-rework"}]},
		{"number": 4, "title": "also parked", "labels": [{"name": "ai-rework"}, {"name": "ai-agent"}]}
	]`}}}
	g := NewGitHub(f, &Config{RepoPath: "/r", RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: defaultStateLabels()})
	g.retry = testRetry
	issues, err := g.ListIssuesWithLabel(context.Background(), "ai-rework")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].Number != 3 || issues[1].Number != 4 {
		t.Fatalf("issues = %+v, want #3 and #4 (state labels must NOT filter)", issues)
	}
	joined := strings.Join(f.calls[0].args, " ")
	if !strings.Contains(joined, "--label ai-rework") || !strings.Contains(joined, "--state open") {
		t.Errorf("gh args = %q", joined)
	}
	// The query is scoped to this instance's eligible label so a multi-user repo
	// doesn't resume/sweep another user's shared-state-label issues.
	if !strings.Contains(joined, "--label ai-agent") {
		t.Errorf("query must also require the eligible label; gh args = %q", joined)
	}
}

// With no eligible label configured, the query stays a bare single-label scan
// (no dangling empty --label) so older single-user configs are unchanged.
func TestListIssuesWithLabelNoEligibleLabel(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `[{"number": 3, "title": "parked", "labels": [{"name": "ai-rework"}]}]`}}}
	g := NewGitHub(f, &Config{RepoPath: "/r", RepoSlug: "o/r", StateLabels: defaultStateLabels()})
	g.retry = testRetry
	if _, err := g.ListIssuesWithLabel(context.Background(), "ai-rework"); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.calls[0].args {
		if a == "" {
			t.Fatalf("empty arg in gh call; args = %q", strings.Join(f.calls[0].args, " "))
		}
	}
	if n := strings.Count(strings.Join(f.calls[0].args, " "), "--label"); n != 1 {
		t.Errorf("want exactly one --label without an eligible label, got %d", n)
	}
}

func TestLabelOps(t *testing.T) {
	f := &fakeRunner{}
	g := testGitHub(f)
	if err := g.AddLabel(context.Background(), 7, "ai-wip"); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveLabel(context.Background(), 7, "ai-wip"); err != nil {
		t.Fatal(err)
	}
	add, rem := f.calls[0], f.calls[1]
	if got := argAfter(add.args, "--add-label"); got != "ai-wip" {
		t.Errorf("add args = %v", add.args)
	}
	if got := argAfter(rem.args, "--remove-label"); got != "ai-wip" {
		t.Errorf("remove args = %v", rem.args)
	}
	if !hasArg(add.args, "7") {
		t.Errorf("issue number missing: %v", add.args)
	}
}

func TestSwapLabels(t *testing.T) {
	f := &fakeRunner{}
	g := testGitHub(f)
	if err := g.SwapLabels(context.Background(), 7, "ai-wip", "ai-done"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want exactly 1 (single atomic gh call)", len(f.calls))
	}
	c := f.calls[0]
	if got := argAfter(c.args, "--remove-label"); got != "ai-wip" {
		t.Errorf("--remove-label = %q", got)
	}
	if got := argAfter(c.args, "--add-label"); got != "ai-done" {
		t.Errorf("--add-label = %q", got)
	}
	if !hasArg(c.args, "7") {
		t.Errorf("issue number missing: %v", c.args)
	}
}

func TestComment(t *testing.T) {
	f := &fakeRunner{}
	g := testGitHub(f)
	if err := g.Comment(context.Background(), 7, "hello"); err != nil {
		t.Fatal(err)
	}
	c := f.calls[0]
	if got := argAfter(c.args, "--body"); got != "hello" {
		t.Errorf("comment args = %v", c.args)
	}
}

func TestCloseIssue(t *testing.T) {
	f := &fakeRunner{}
	g := testGitHub(f)
	if err := g.CloseIssue(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	c := f.calls[0]
	if c.name != "gh" || !hasArg(c.args, "issue") || !hasArg(c.args, "close") || !hasArg(c.args, "7") {
		t.Errorf("close call = %v", c.args)
	}
	if got := argAfter(c.args, "--repo"); got != "org/repo" {
		t.Errorf("--repo = %q", got)
	}
}

func TestFetchIssueContent(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `{
		"title": "Crash on save",
		"body": "It crashes.",
		"comments": [{"author": {"login": "alice"}, "body": "repro attached"}]
	}`}}}
	g := testGitHub(f)
	got, err := g.FetchIssueContent(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Crash on save", "It crashes.", "@alice", "repro attached"} {
		if !strings.Contains(got, want) {
			t.Errorf("content missing %q:\n%s", want, got)
		}
	}
}

func TestCreatePRReturnsURL(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: "https://github.com/org/repo/pull/9\n"}}}
	g := testGitHub(f)
	url, err := g.CreatePR(context.Background(), "ai/issue-7", "Fix (#7)", "Closes #7")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/org/repo/pull/9" {
		t.Errorf("url = %q", url)
	}
	c := f.calls[0]
	if got := argAfter(c.args, "--head"); got != "ai/issue-7" {
		t.Errorf("--head = %q", got)
	}
	if got := argAfter(c.args, "--title"); got != "Fix (#7)" {
		t.Errorf("--title = %q", got)
	}
}

// When the head branch already has an open PR (e.g. a prior run pushed the
// branch and opened the PR but never reached the Done state), `gh pr create`
// exits non-zero. That is the desired end state, not a failure: CreatePR must
// recover the existing PR's URL and return it as success.
func TestCreatePRRecoversExistingPR(t *testing.T) {
	f := &fakeRunner{queue: []rresp{
		{err: errors.New("exit status 1"), stderr: `a pull request for branch "ai/issue-527" into branch "main" already exists:
#824
`},
		{stdout: `{"url": "https://github.com/org/repo/pull/824"}`},
	}}
	g := testGitHub(f)
	url, err := g.CreatePR(context.Background(), "ai/issue-527", "Fix (#527)", "Closes #527")
	if err != nil {
		t.Fatalf("CreatePR should recover from an existing PR, got error: %v", err)
	}
	if url != "https://github.com/org/repo/pull/824" {
		t.Errorf("url = %q, want existing PR url", url)
	}
	// Second call must look up the PR by head branch.
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (create then view)", len(f.calls))
	}
	view := f.calls[1]
	if !hasArg(view.args, "view") || !hasArg(view.args, "ai/issue-527") {
		t.Errorf("view call = %v", view.args)
	}
}

func TestPRURLForBranch(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: `{"url":"https://github.com/o/r/pull/5"}`}}}
	g := testGitHub(f)
	url, err := g.PRURLForBranch(context.Background(), "ai/issue-5")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/o/r/pull/5" {
		t.Errorf("url = %q", url)
	}
}

func TestGHRetriesTransientThenSucceeds(t *testing.T) {
	f := &fakeRunner{queue: []rresp{
		{err: errors.New("exit 1"), stderr: "HTTP 502 Bad Gateway"},
		{stdout: ""},
	}}
	g := testGitHub(f)
	g.retry = testRetry
	if err := g.Comment(context.Background(), 7, "hi"); err != nil {
		t.Fatalf("want success after one retry, got %v", err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (fail then retry-success)", len(f.calls))
	}
}

func TestGHDoesNotRetryPermanentError(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{err: errors.New("exit 1"), stderr: "not found"}}}
	g := testGitHub(f)
	g.retry = testRetry
	if err := g.Comment(context.Background(), 7, "hi"); err == nil {
		t.Fatal("want error for permanent failure")
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on permanent error)", len(f.calls))
	}
}

func TestIssueTitle(t *testing.T) {
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		return `{"title": "Fix the thing"}`, "", nil
	}}
	g := &GitHub{runner: f, slug: "org/repo", retry: testRetry}
	title, err := g.IssueTitle(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Fix the thing" {
		t.Errorf("title = %q", title)
	}
}

func TestHasStateLabelRecognizesRework(t *testing.T) {
	g := &GitHub{state: defaultStateLabels()}
	is := Issue{Number: 1, Labels: []Label{{Name: "ai-agent"}, {Name: "ai-rework"}}}
	if !g.hasStateLabel(is) {
		t.Error("an ai-rework issue must count as having a state label so it is not re-picked")
	}
}
