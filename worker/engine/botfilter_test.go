package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// testGitHubHost builds the real gh adapter over a fake runner, so this test
// exercises the engine's outbound comment text against the adapter's inbound
// bot-comment filter end to end.
func testGitHubHost(f *testkit.FakeRunner) shared.CodeHost {
	cfg := &shared.Config{RepoPath: "/clone", RepoSlug: "org/repo", EligibleLabel: "ai-agent",
		StateLabels: testStateLabels()}
	return infra.NewGitHubWithRetry(f, cfg, testkit.TestRetry)
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The daemon's own status chatter (pickup, park + error dump, PR link, ...) is
// noise in the prompt the issue content becomes — and it piles up on every
// re-run. It must be stripped, both when carried by the marker and, for
// comments posted before the marker existed, by its leading text.
func TestFetchIssueContentDropsLoopeStatusComments(t *testing.T) {
	comments := []string{
		pickupComment("bug", "ai/issue-7"),
		parkComment("ai-rework", "", "dial tcp: i/o timeout"),
		prComment("https://example.test/pr/1"),
		alreadyDoneComment("already there."),
		stoppedComment(),
		// Legacy: posted before the marker was introduced.
		"🤖 Picked up (feature flow). Branch: `ai/issue-7`",
		"⏸ Stopped by user. Worktree, logs and session are preserved.",
	}
	var arr []string
	for _, c := range comments {
		b, _ := json.Marshal(struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body string `json:"body"`
		}{Body: c})
		arr = append(arr, string(b))
	}
	arr = append(arr,
		`{"author": {"login": "alice"}, "body": "repro attached"}`,
		`{"author": {"login": "loope"}, "body": `+mustJSON(needsInfoComment(42, "ai-needs-info", "Which database?"))+`}`,
		`{"author": {"login": "alice"}, "body": "1a, 2c"}`,
	)
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: `{
		"title": "Crash on save",
		"body": "It crashes.",
		"comments": [` + strings.Join(arr, ",") + `]
	}`}}}
	g := testGitHubHost(f)
	got, err := g.FetchIssueContent(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"Picked up", "Parked as", "PR: https", "Already implemented", "Stopped by user", "i/o timeout", shared.BotMarker} {
		if strings.Contains(got, bad) {
			t.Errorf("content still carries loope status text %q:\n%s", bad, got)
		}
	}
	// A human comment, and the questions its answer refers to, must survive.
	for _, want := range []string{"repro attached", "Which database?", "1a, 2c"} {
		if !strings.Contains(got, want) {
			t.Errorf("content missing %q:\n%s", want, got)
		}
	}
}
