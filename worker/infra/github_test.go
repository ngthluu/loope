package infra

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

func testGitHub(f *testkit.FakeRunner) *GitHub {
	return NewGitHub(f, &shared.Config{RepoPath: "/clone", RepoSlug: "org/repo", StateLabels: shared.StateLabels{WIP: "ai-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}})
}

func TestHasStateLabelExcludesStopped(t *testing.T) {
	g := NewGitHub(&testkit.FakeRunner{}, &shared.Config{RepoSlug: "o/r", StateLabels: shared.StateLabels{WIP: "ai-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}})
	is := shared.Issue{Number: 5, Labels: []shared.Label{{Name: "ai-agent"}, {Name: "ai-stopped"}}}
	if !g.hasStateLabel(is) {
		t.Fatal("an issue carrying ai-stopped must count as having a state label (dropped from the eligible queue)")
	}
}

func TestHasStateLabelIncludesNeedsInfo(t *testing.T) {
	g := &GitHub{state: shared.StateLabels{WIP: "ai-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}}
	is := shared.Issue{Labels: []shared.Label{{Name: "ai-agent"}, {Name: "ai-needs-info"}}}
	if !g.hasStateLabel(is) {
		t.Error("an issue labeled ai-needs-info must be treated as having a state label")
	}
}

func TestListEligibleIssuesFiltersStateLabels(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `[
		{"number": 1, "title": "A", "body": "a", "labels": [{"name": "ai-agent"}]},
		{"number": 2, "title": "B", "body": "b", "labels": [{"name": "ai-agent"}, {"name": "ai-wip"}]},
		{"number": 3, "title": "C", "body": "c", "labels": [{"name": "ai-agent"}, {"name": "ai-rework"}]},
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
	call := f.Calls[0]
	if call.Name != "gh" || !testkit.HasArg(call.Args, "ai-agent") || !testkit.HasArg(call.Args, "--json") {
		t.Errorf("call = %+v", call)
	}
	if got := testkit.ArgAfter(call.Args, "--repo"); got != "org/repo" {
		t.Errorf("--repo = %q", got)
	}
}

func TestListEligibleIssuesFiltersConfiguredStateLabels(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `[
		{"number": 1, "title": "A", "body": "a", "labels": [{"name": "ai-agent"}, {"name": "bot-wip"}]},
		{"number": 2, "title": "B", "body": "b", "labels": [{"name": "ai-agent"}, {"name": "bot-done"}]},
		{"number": 3, "title": "C", "body": "c", "labels": [{"name": "ai-agent"}, {"name": "ai-wip"}]}
	]`}}}
	g := NewGitHub(f, &shared.Config{RepoPath: "/clone", RepoSlug: "org/repo",
		StateLabels: shared.StateLabels{WIP: "bot-wip", Done: "bot-done"}})
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
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `[
		{"number": 3, "title": "parked", "labels": [{"name": "ai-rework"}]},
		{"number": 4, "title": "also parked", "labels": [{"name": "ai-rework"}, {"name": "ai-agent"}]}
	]`}}}
	g := NewGitHub(f, &shared.Config{RepoPath: "/r", RepoSlug: "o/r", EligibleLabel: "ai-agent", StateLabels: shared.StateLabels{WIP: "ai-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}})
	g.retry = testkit.TestRetry
	issues, err := g.ListIssuesWithLabel(context.Background(), "ai-rework")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].Number != 3 || issues[1].Number != 4 {
		t.Fatalf("issues = %+v, want #3 and #4 (state labels must NOT filter)", issues)
	}
	joined := strings.Join(f.Calls[0].Args, " ")
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
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `[{"number": 3, "title": "parked", "labels": [{"name": "ai-rework"}]}]`}}}
	g := NewGitHub(f, &shared.Config{RepoPath: "/r", RepoSlug: "o/r", StateLabels: shared.StateLabels{WIP: "ai-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}})
	g.retry = testkit.TestRetry
	if _, err := g.ListIssuesWithLabel(context.Background(), "ai-rework"); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Calls[0].Args {
		if a == "" {
			t.Fatalf("empty arg in gh call; args = %q", strings.Join(f.Calls[0].Args, " "))
		}
	}
	if n := strings.Count(strings.Join(f.Calls[0].Args, " "), "--label"); n != 1 {
		t.Errorf("want exactly one --label without an eligible label, got %d", n)
	}
}

func TestLabelOps(t *testing.T) {
	f := &testkit.FakeRunner{}
	g := testGitHub(f)
	if err := g.AddLabel(context.Background(), 7, "ai-wip"); err != nil {
		t.Fatal(err)
	}
	if err := g.RemoveLabel(context.Background(), 7, "ai-wip"); err != nil {
		t.Fatal(err)
	}
	add, rem := f.Calls[0], f.Calls[1]
	if got := testkit.ArgAfter(add.Args, "--add-label"); got != "ai-wip" {
		t.Errorf("add args = %v", add.Args)
	}
	if got := testkit.ArgAfter(rem.Args, "--remove-label"); got != "ai-wip" {
		t.Errorf("remove args = %v", rem.Args)
	}
	if !testkit.HasArg(add.Args, "7") {
		t.Errorf("issue number missing: %v", add.Args)
	}
}

func TestSwapLabels(t *testing.T) {
	f := &testkit.FakeRunner{}
	g := testGitHub(f)
	if err := g.SwapLabels(context.Background(), 7, "ai-wip", "ai-done"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want exactly 1 (single atomic gh call)", len(f.Calls))
	}
	c := f.Calls[0]
	if got := testkit.ArgAfter(c.Args, "--remove-label"); got != "ai-wip" {
		t.Errorf("--remove-label = %q", got)
	}
	if got := testkit.ArgAfter(c.Args, "--add-label"); got != "ai-done" {
		t.Errorf("--add-label = %q", got)
	}
	if !testkit.HasArg(c.Args, "7") {
		t.Errorf("issue number missing: %v", c.Args)
	}
}

func TestComment(t *testing.T) {
	f := &testkit.FakeRunner{}
	g := testGitHub(f)
	if err := g.Comment(context.Background(), 7, "hello"); err != nil {
		t.Fatal(err)
	}
	c := f.Calls[0]
	if got := testkit.ArgAfter(c.Args, "--body"); got != "hello" {
		t.Errorf("comment args = %v", c.Args)
	}
}

func TestCloseIssue(t *testing.T) {
	f := &testkit.FakeRunner{}
	g := testGitHub(f)
	if err := g.CloseIssue(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	c := f.Calls[0]
	if c.Name != "gh" || !testkit.HasArg(c.Args, "issue") || !testkit.HasArg(c.Args, "close") || !testkit.HasArg(c.Args, "7") {
		t.Errorf("close call = %v", c.Args)
	}
	if got := testkit.ArgAfter(c.Args, "--repo"); got != "org/repo" {
		t.Errorf("--repo = %q", got)
	}
}

func TestFetchIssueContent(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `{
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

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestCreatePRReturnsURL(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: "https://github.com/org/repo/pull/9\n"}}}
	g := testGitHub(f)
	url, err := g.CreatePR(context.Background(), "ai/issue-7", "Fix (#7)", "Closes #7")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/org/repo/pull/9" {
		t.Errorf("url = %q", url)
	}
	c := f.Calls[0]
	if got := testkit.ArgAfter(c.Args, "--head"); got != "ai/issue-7" {
		t.Errorf("--head = %q", got)
	}
	if got := testkit.ArgAfter(c.Args, "--title"); got != "Fix (#7)" {
		t.Errorf("--title = %q", got)
	}
}

// When the head branch already has an open PR (e.g. a prior run pushed the
// branch and opened the PR but never reached the Done state), `gh pr create`
// exits non-zero. That is the desired end state, not a failure: CreatePR must
// recover the existing PR's URL and return it as success.
func TestCreatePRRecoversExistingPR(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Err: errors.New("exit status 1"), Stderr: `a pull request for branch "ai/issue-527" into branch "main" already exists:
#824
`},
		{Stdout: `{"url": "https://github.com/org/repo/pull/824"}`},
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
	if len(f.Calls) != 2 {
		t.Fatalf("calls = %d, want 2 (create then view)", len(f.Calls))
	}
	view := f.Calls[1]
	if !testkit.HasArg(view.Args, "view") || !testkit.HasArg(view.Args, "ai/issue-527") {
		t.Errorf("view call = %v", view.Args)
	}
}

func TestPRURLForBranch(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `{"url":"https://github.com/o/r/pull/5"}`}}}
	g := testGitHub(f)
	url, err := g.PRURLForBranch(context.Background(), "ai/issue-5")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/o/r/pull/5" {
		t.Errorf("url = %q", url)
	}
}

func TestPRNumberForBranch(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `{"number":42}`}}}
	g := testGitHub(f)
	n, err := g.PRNumberForBranch(context.Background(), "ai/issue-5")
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Errorf("number = %d, want 42", n)
	}
	if !testkit.HasArg(f.Calls[0].Args, "view") || !testkit.HasArg(f.Calls[0].Args, "ai/issue-5") {
		t.Errorf("call args = %v", f.Calls[0].Args)
	}
	if got := testkit.ArgAfter(f.Calls[0].Args, "--json"); !strings.Contains(got, "number") {
		t.Errorf("--json = %q, want it to request number", got)
	}
}

func TestPRNumberForBranchPropagatesError(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Err: errors.New("exit 1"), Stderr: "no pull requests found"}}}
	g := testGitHub(f)
	if _, err := g.PRNumberForBranch(context.Background(), "ai/issue-5"); err == nil {
		t.Fatal("want an error when gh pr view fails")
	}
}

func TestReviewComment(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: ""}}}
	g := testGitHub(f)
	if err := g.ReviewComment(context.Background(), 42, "looks good"); err != nil {
		t.Fatal(err)
	}
	call := f.Calls[0]
	if !testkit.HasArg(call.Args, "review") || !testkit.HasArg(call.Args, "42") || !testkit.HasArg(call.Args, "--comment") {
		t.Errorf("args = %v, want a `gh pr review 42 --comment ...` call", call.Args)
	}
	if testkit.ArgAfter(call.Args, "--body") != "looks good" {
		t.Errorf("--body = %q", testkit.ArgAfter(call.Args, "--body"))
	}
}

func TestGHRetriesTransientThenSucceeds(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Err: errors.New("exit 1"), Stderr: "HTTP 502 Bad Gateway"},
		{Stdout: ""},
	}}
	g := testGitHub(f)
	g.retry = testkit.TestRetry
	if err := g.AddLabel(context.Background(), 7, "ai-wip"); err != nil {
		t.Fatalf("want success after one retry, got %v", err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("calls = %d, want 2 (fail then retry-success)", len(f.Calls))
	}
}

// Comment/ReviewComment are non-idempotent: a 5xx/timeout/EOF may have landed
// the write before the connection died, so re-sending could duplicate it.
// Only pre-send failures (rate limit, connection refused, DNS) are retried.
func TestCommentDoesNotRetryAmbiguousFailure(t *testing.T) {
	for _, stderr := range []string{"HTTP 502 Bad Gateway", "net/http: request timeout", "unexpected EOF"} {
		f := &testkit.FakeRunner{Queue: []testkit.RResp{
			{Err: errors.New("exit 1"), Stderr: stderr},
			{Stdout: ""},
		}}
		g := testGitHub(f)
		g.retry = testkit.TestRetry
		if err := g.Comment(context.Background(), 7, "hi"); err == nil {
			t.Errorf("%q: want the ambiguous failure surfaced, not retried", stderr)
		}
		if len(f.Calls) != 1 {
			t.Errorf("%q: calls = %d, want 1 (no retry on a possibly-applied write)", stderr, len(f.Calls))
		}
	}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Err: errors.New("exit 1"), Stderr: "HTTP 500 Internal Server Error"},
	}}
	g := testGitHub(f)
	g.retry = testkit.TestRetry
	if err := g.ReviewComment(context.Background(), 42, "hi"); err == nil || len(f.Calls) != 1 {
		t.Errorf("ReviewComment: err = %v, calls = %d; want error and a single call", err, len(f.Calls))
	}
}

func TestCommentRetriesPreSendFailure(t *testing.T) {
	for _, stderr := range []string{"HTTP 429 Too Many Requests", "You have exceeded a secondary rate limit", "dial tcp: connection refused", "Could not resolve host: api.github.com"} {
		f := &testkit.FakeRunner{Queue: []testkit.RResp{
			{Err: errors.New("exit 1"), Stderr: stderr},
			{Stdout: ""},
		}}
		g := testGitHub(f)
		g.retry = testkit.TestRetry
		if err := g.Comment(context.Background(), 7, "hi"); err != nil {
			t.Errorf("%q: want success after one retry, got %v", stderr, err)
		}
		if len(f.Calls) != 2 {
			t.Errorf("%q: calls = %d, want 2 (request never reached the server: safe to retry)", stderr, len(f.Calls))
		}
	}
}

func TestListEligibleIssuesExcludesStateLabelsServerSide(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `[]`}}}
	g := testGitHub(f)
	if _, err := g.ListEligibleIssues(context.Background(), "ai-agent"); err != nil {
		t.Fatal(err)
	}
	args := f.Calls[0].Args
	search := testkit.ArgAfter(args, "--search")
	for _, l := range []string{"ai-wip", "ai-done", "ai-rework", "ai-needs-info", "ai-stopped"} {
		if !strings.Contains(search, "-label:"+l) {
			t.Errorf("--search = %q, want it to exclude %s", search, l)
		}
	}
	if got := testkit.ArgAfter(args, "--limit"); got == "50" || got == "" {
		t.Errorf("--limit = %q, want a page larger than the old 50", got)
	}
}

func TestGHDoesNotRetryPermanentError(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Err: errors.New("exit 1"), Stderr: "not found"}}}
	g := testGitHub(f)
	g.retry = testkit.TestRetry
	if err := g.Comment(context.Background(), 7, "hi"); err == nil {
		t.Fatal("want error for permanent failure")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on permanent error)", len(f.Calls))
	}
}

func TestIssueTitle(t *testing.T) {
	f := &testkit.FakeRunner{Handler: func(c testkit.RCall) (string, string, error) {
		return `{"title": "Fix the thing"}`, "", nil
	}}
	g := &GitHub{runner: f, slug: "org/repo", retry: testkit.TestRetry}
	title, err := g.IssueTitle(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Fix the thing" {
		t.Errorf("title = %q", title)
	}
}

func TestHasStateLabelRecognizesRework(t *testing.T) {
	g := &GitHub{state: shared.StateLabels{WIP: "ai-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}}
	is := shared.Issue{Number: 1, Labels: []shared.Label{{Name: "ai-agent"}, {Name: "ai-rework"}}}
	if !g.hasStateLabel(is) {
		t.Error("an ai-rework issue must count as having a state label so it is not re-picked")
	}
}

// UATSurfaces is one `gh issue view`: the body first, then every comment, so
// the UAT step can look for its marker on either surface.
func TestUATSurfacesReturnsBodyThenComments(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `{"body": "the original body", "comments": [{"body": "first"}, {"body": "second"}]}`}}}
	g := testGitHub(f)
	g.retry = testkit.TestRetry
	got, err := g.UATSurfaces(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"the original body", "first", "second"}
	if len(got) != len(want) {
		t.Fatalf("surfaces = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("surface %d = %q, want %q", i, got[i], want[i])
		}
	}
	joined := strings.Join(f.Calls[0].Args, " ")
	if !strings.Contains(joined, "issue view 7") || !strings.Contains(joined, "--repo org/repo") ||
		!strings.Contains(joined, "--json body,comments") {
		t.Errorf("gh args = %q", joined)
	}
}

func TestUATSurfacesPropagatesParseError(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `not json`}}}
	g := testGitHub(f)
	g.retry = testkit.TestRetry
	if _, err := g.UATSurfaces(context.Background(), 7); err == nil {
		t.Error("want a parse error, got nil")
	}
}
