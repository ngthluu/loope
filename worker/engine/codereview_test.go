package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// fakeCodeReviewTarget stands in for *GitHub: it records what was posted and
// can be scripted to fail either operation, mirroring fakeUATTarget.
type fakeCodeReviewTarget struct {
	prNum      int
	prErr      error
	comments   []string
	commentErr error
}

func (f *fakeCodeReviewTarget) PRNumberForBranch(ctx context.Context, branch string) (int, error) {
	if f.prErr != nil {
		return 0, f.prErr
	}
	return f.prNum, nil
}

func (f *fakeCodeReviewTarget) ReviewComment(ctx context.Context, prNumber int, body string) error {
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments = append(f.comments, body)
	return nil
}

func codeReviewTestConfig(rounds int) *shared.Config {
	return &shared.Config{Models: shared.Models{CodeReview: &shared.CodeReviewConfig{
		ModelConfig: shared.ModelConfig{Model: "sonnet", Effort: "medium"},
		Rounds:      rounds,
	}}}
}

// codeReviewResult builds a fake claude payload whose structured output
// carries the round's status and summary.
func codeReviewResult(status, summary string) string {
	return testkit.ClaudeStructured("cr-1", map[string]any{"status": status, "summary": summary})
}

func noopPush(ctx context.Context, wtPath, branch string) error { return nil }

func TestParseCodeReviewStatuses(t *testing.T) {
	cases := []struct {
		status  string
		summary string
		want    codeReviewStatus
	}{
		{"clean", "Nothing to fix.", codeReviewClean},
		{"fixed", "- fixed A\n- fixed B", codeReviewFixed},
		{"blocked", "Can't safely fix X.", codeReviewBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			res := mustResult(t, codeReviewResult(tc.status, tc.summary))
			status, summary := parseCodeReview(res)
			if status != tc.want || summary != tc.summary {
				t.Errorf("status=%q summary=%q, want %q/%q", status, summary, tc.want, tc.summary)
			}
		})
	}
}

// An off-contract session — one whose structured output is missing or carries
// an unrecognized status — is treated as blocked with the raw result as the
// summary, so it surfaces rather than being silently dropped.
func TestParseCodeReviewOffContract(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
	}{
		{"no structured output", testkit.ClaudeJSON("I could not find the marker.", "cr-1")},
		{"unrecognized status", testkit.ClaudeStructured("cr-1", map[string]any{"status": "weird", "summary": "s"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := mustResult(t, tc.stdout)
			status, summary := parseCodeReview(res)
			if status != codeReviewBlocked || summary != strings.TrimSpace(res.Result) {
				t.Errorf("status=%q summary=%q, want blocked with the raw result verbatim", status, summary)
			}
		})
	}
}

// mustResult decodes a fake claude payload into the ClaudeResult the parsers
// consume, the same way infra.Claude hands one back.
func mustResult(t *testing.T, payload string) *shared.ClaudeResult {
	t.Helper()
	var res shared.ClaudeResult
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		t.Fatalf("decode fake payload: %v", err)
	}
	return &res
}

func TestCodeReviewStopsOnClean(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := infra.NewClaude(f, "", "")
	var pushCalls int
	cr := &CodeReview{Target: tgt, Push: func(ctx context.Context, wtPath, branch string) error {
		pushCalls++
		return nil
	}, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (stop after round 1's clean, even though Rounds=3)", len(f.Calls))
	}
	if pushCalls != 1 {
		t.Errorf("push calls = %d, want 1", pushCalls)
	}
	if len(tgt.comments) != 1 || !strings.Contains(tgt.comments[0], "clean") {
		t.Errorf("comments = %v", tgt.comments)
	}
	if lastCompletedRound(logDir) != 1 {
		t.Errorf("lastCompletedRound = %d, want 1", lastCompletedRound(logDir))
	}
}

func TestCodeReviewRunsAllRoundsWhenAlwaysFixed(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Stdout: codeReviewResult("fixed", "- fixed A")},
		{Stdout: codeReviewResult("fixed", "- fixed B")},
	}}
	c := infra.NewClaude(f, "", "")
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(2), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("claude calls = %d, want 2 (exactly Rounds, since status never stops the loop)", len(f.Calls))
	}
	if len(tgt.comments) != 2 {
		t.Errorf("comments = %d, want 2", len(tgt.comments))
	}
	if lastCompletedRound(logDir) != 2 {
		t.Errorf("lastCompletedRound = %d, want 2", lastCompletedRound(logDir))
	}
}

func TestCodeReviewStopsOnBlocked(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: codeReviewResult("blocked", "Can't safely fix X.")}}}
	c := infra.NewClaude(f, "", "")
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (stop on blocked, even though Rounds=3)", len(f.Calls))
	}
	if len(tgt.comments) != 1 || !strings.Contains(tgt.comments[0], "Can't safely fix X.") {
		t.Errorf("comments = %v", tgt.comments)
	}
}

func TestCodeReviewResumesFromRoundFile(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	logDir := t.TempDir()
	recordCodeReviewRound(logDir, 1)
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := infra.NewClaude(f, "", "")
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (round 1 already recorded done, this call is round 2)", len(f.Calls))
	}
	if len(tgt.comments) != 1 || !strings.Contains(tgt.comments[0], "round 2/3") {
		t.Errorf("comments = %v, want a round-2 comment", tgt.comments)
	}
}

func TestCodeReviewPRLookupFailureSkipsLoop(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prErr: fmt.Errorf("gh: 404")}
	f := &testkit.FakeRunner{}
	c := infra.NewClaude(f, "", "")
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir())
	if err == nil {
		t.Fatal("want an error when the PR lookup fails")
	}
	if len(f.Calls) != 0 {
		t.Errorf("claude calls = %d, want 0 — there is nowhere to post findings", len(f.Calls))
	}
}

func TestCodeReviewPushFailureStopsLoop(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: codeReviewResult("fixed", "- fixed A")}}}
	c := infra.NewClaude(f, "", "")
	cr := &CodeReview{Target: tgt, Push: func(ctx context.Context, wtPath, branch string) error {
		return fmt.Errorf("push failed")
	}, Num: 7}
	err := cr.Run(context.Background(), c, codeReviewTestConfig(2), "/wt", "b", "main", t.TempDir())
	if err == nil {
		t.Fatal("want an error when push fails")
	}
	if len(tgt.comments) != 0 {
		t.Errorf("comments = %d, want 0 — push failed before anything was posted", len(tgt.comments))
	}
}

func TestCodeReviewSurvivesCommentFailure(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42, commentErr: fmt.Errorf("gh: 422")}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := infra.NewClaude(f, "", "")
	logDir := t.TempDir()
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if lastCompletedRound(logDir) != 1 {
		t.Errorf("lastCompletedRound = %d, want 1 even though the comment post failed — the round isn't repeated just because the comment failed", lastCompletedRound(logDir))
	}
}

func TestCodeReviewNilReceiverIsSafe(t *testing.T) {
	var cr *CodeReview
	f := &testkit.FakeRunner{}
	c := infra.NewClaude(f, "", "")
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %d, want 0", len(f.Calls))
	}
}

func TestCodeReviewNoTargetIsSafe(t *testing.T) {
	cr := &CodeReview{}
	f := &testkit.FakeRunner{}
	c := infra.NewClaude(f, "", "")
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %d, want 0", len(f.Calls))
	}
}

func TestCodeReviewNilConfigBlockIsSafe(t *testing.T) {
	cr := &CodeReview{Target: &fakeCodeReviewTarget{prNum: 42}, Push: noopPush}
	f := &testkit.FakeRunner{}
	c := infra.NewClaude(f, "", "")
	if err := cr.Run(context.Background(), c, &shared.Config{}, "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 0 {
		t.Errorf("calls = %d, want 0 — a nil Models.CodeReview must disable the step entirely", len(f.Calls))
	}
}

func TestCodeReviewCallUsesConfiguredModelAndDistinctLabel(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := infra.NewClaude(f, "", "")
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	call := f.Calls[0]
	if call.Dir != "/wt" || testkit.ArgAfter(call.Args, "--model") != "sonnet" {
		t.Errorf("call = %+v", call)
	}
	if !strings.Contains(call.Stdin, "/code-review") {
		t.Errorf("prompt should invoke /code-review: %s", call.Stdin)
	}
}

// TestCodeReviewRecordsSessionStage: every review round records its session
// under shared.StageCodeReview, so a park or crash mid-review resumes the review
// session instead of re-resuming the finished execute/debug session.
func TestCodeReviewRecordsSessionStage(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	logDir := t.TempDir()
	c := infra.NewClaude(f, logDir, "")
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7, Kind: "feature"}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	si, err := shared.ReadSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "cr-1" || si.Kind != "feature" || si.Stage != shared.StageCodeReview {
		t.Errorf("session = %+v, want cr-1/feature/codereview", si)
	}
}

// TestCodeReviewRecordsSessionOnError mirrors the pipeline stages: an errored
// round still records its session id, which is exactly what the post-park
// re-entry resumes.
func TestCodeReviewRecordsSessionOnError(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeErrorJSON("You've hit your session limit", "cr-429")}}}
	logDir := t.TempDir()
	c := infra.NewClaude(f, logDir, "")
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7, Kind: "bug"}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "ai/issue-7", "main", logDir); err == nil {
		t.Fatal("want the round error propagated so ship parks")
	}
	si, err := shared.ReadSession(logDir)
	if err != nil {
		t.Fatalf("session must be recorded even when the round errors: %v", err)
	}
	if si.SessionID != "cr-429" || si.Stage != shared.StageCodeReview {
		t.Errorf("session = %+v, want cr-429/codereview", si)
	}
}

// TestCodeReviewResumesRecordedReviewSession: when logDir holds a
// codereview-stage session (a parked review), the first executed round
// resumes THAT session with --resume and "continue" — continuing the
// cut-short round, never skipping to the next one — and later rounds run
// fresh as usual.
func TestCodeReviewResumesRecordedReviewSession(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	logDir := t.TempDir()
	recordCodeReviewRound(logDir, 1) // round 1 completed; round 2 was cut short
	f := &testkit.FakeRunner{Queue: []testkit.RResp{
		{Stdout: codeReviewResult("fixed", "- finished the cut-short fix")},
		{Stdout: codeReviewResult("clean", "Nothing left.")},
	}}
	c := infra.NewClaude(f, logDir, "")
	c.RecordCheckpoint(shared.SessionInfo{SessionID: "cr-parked", Kind: "feature", Stage: shared.StageCodeReview})
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7, Kind: "feature"}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("claude calls = %d, want 2 (resumed round 2, fresh round 3)", len(f.Calls))
	}
	if got := testkit.ArgAfter(f.Calls[0].Args, "--resume"); got != "cr-parked" {
		t.Errorf("first call --resume = %q, want cr-parked (continue the cut-short round)", got)
	}
	if f.Calls[0].Stdin != "continue" {
		t.Errorf("first call prompt = %q, want \"continue\"", f.Calls[0].Stdin)
	}
	if got := testkit.ArgAfter(f.Calls[1].Args, "--resume"); got != "" {
		t.Errorf("second call --resume = %q, want a fresh session for the next round", got)
	}
	if len(tgt.comments) != 2 || !strings.Contains(tgt.comments[0], "round 2/3") {
		t.Errorf("comments = %v, want the resumed round posted as round 2/3", tgt.comments)
	}
}
