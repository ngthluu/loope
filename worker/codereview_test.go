package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
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

func codeReviewTestConfig(rounds int) *Config {
	return &Config{Models: Models{CodeReview: &CodeReviewConfig{
		ModelConfig: ModelConfig{Model: "sonnet", Effort: "medium", MaxTurns: 30},
		Rounds:      rounds,
	}}}
}

// codeReviewResult builds a fake claude payload whose result carries a
// fenced STATUS line and summary.
func codeReviewResult(status, summary string) string {
	return claudeJSON("Reviewing...\n"+codeReviewBeginSentinel+"\nSTATUS: "+status+"\n"+summary+"\n"+codeReviewEndSentinel, "cr-1")
}

func noopPush(ctx context.Context, wtPath, branch string) error { return nil }

func TestParseCodeReviewClean(t *testing.T) {
	status, summary := parseCodeReview(codeReviewBeginSentinel + "\nSTATUS: clean\nNothing to fix.\n" + codeReviewEndSentinel)
	if status != codeReviewClean || summary != "Nothing to fix." {
		t.Errorf("status=%q summary=%q", status, summary)
	}
}

func TestParseCodeReviewFixed(t *testing.T) {
	status, summary := parseCodeReview(codeReviewBeginSentinel + "\nSTATUS: fixed\n- fixed A\n- fixed B\n" + codeReviewEndSentinel)
	if status != codeReviewFixed || summary != "- fixed A\n- fixed B" {
		t.Errorf("status=%q summary=%q", status, summary)
	}
}

func TestParseCodeReviewBlocked(t *testing.T) {
	status, summary := parseCodeReview(codeReviewBeginSentinel + "\nSTATUS: blocked\nCan't safely fix X.\n" + codeReviewEndSentinel)
	if status != codeReviewBlocked || summary != "Can't safely fix X." {
		t.Errorf("status=%q summary=%q", status, summary)
	}
}

func TestParseCodeReviewFencePresentNoStatusLine(t *testing.T) {
	raw := codeReviewBeginSentinel + "\nI reviewed the code.\n" + codeReviewEndSentinel
	status, summary := parseCodeReview(raw)
	if status != codeReviewBlocked || summary != raw {
		t.Errorf("status=%q summary=%q, want blocked with the raw result verbatim", status, summary)
	}
}

func TestParseCodeReviewNoFence(t *testing.T) {
	status, summary := parseCodeReview("I could not find the marker.")
	if status != codeReviewBlocked || summary != "I could not find the marker." {
		t.Errorf("status=%q summary=%q, want blocked with the raw result verbatim", status, summary)
	}
}

func TestCodeReviewStopsOnClean(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := &Claude{runner: f}
	var pushCalls int
	cr := &CodeReview{Target: tgt, Push: func(ctx context.Context, wtPath, branch string) error {
		pushCalls++
		return nil
	}, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (stop after round 1's clean, even though Rounds=3)", len(f.calls))
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
	f := &fakeRunner{queue: []rresp{
		{stdout: codeReviewResult("fixed", "- fixed A")},
		{stdout: codeReviewResult("fixed", "- fixed B")},
	}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(2), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("claude calls = %d, want 2 (exactly Rounds, since status never stops the loop)", len(f.calls))
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
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("blocked", "Can't safely fix X.")}}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	logDir := t.TempDir()
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (stop on blocked, even though Rounds=3)", len(f.calls))
	}
	if len(tgt.comments) != 1 || !strings.Contains(tgt.comments[0], "Can't safely fix X.") {
		t.Errorf("comments = %v", tgt.comments)
	}
}

func TestCodeReviewResumesFromRoundFile(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	logDir := t.TempDir()
	recordCodeReviewRound(logDir, 1)
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1 (round 1 already recorded done, this call is round 2)", len(f.calls))
	}
	if len(tgt.comments) != 1 || !strings.Contains(tgt.comments[0], "round 2/3") {
		t.Errorf("comments = %v, want a round-2 comment", tgt.comments)
	}
}

func TestCodeReviewPRLookupFailureSkipsLoop(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prErr: fmt.Errorf("gh: 404")}
	f := &fakeRunner{}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir())
	if err == nil {
		t.Fatal("want an error when the PR lookup fails")
	}
	if len(f.calls) != 0 {
		t.Errorf("claude calls = %d, want 0 — there is nowhere to post findings", len(f.calls))
	}
}

func TestCodeReviewPushFailureStopsLoop(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("fixed", "- fixed A")}}}
	c := &Claude{runner: f}
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
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := &Claude{runner: f}
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
	f := &fakeRunner{}
	c := &Claude{runner: f}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %d, want 0", len(f.calls))
	}
}

func TestCodeReviewNoTargetIsSafe(t *testing.T) {
	cr := &CodeReview{}
	f := &fakeRunner{}
	c := &Claude{runner: f}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %d, want 0", len(f.calls))
	}
}

func TestCodeReviewNilConfigBlockIsSafe(t *testing.T) {
	cr := &CodeReview{Target: &fakeCodeReviewTarget{prNum: 42}, Push: noopPush}
	f := &fakeRunner{}
	c := &Claude{runner: f}
	if err := cr.Run(context.Background(), c, &Config{}, "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %d, want 0 — a nil Models.CodeReview must disable the step entirely", len(f.calls))
	}
}

func TestCodeReviewCallUsesConfiguredModelAndDistinctLabel(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	c := &Claude{runner: f}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "b", "main", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	call := f.calls[0]
	if call.dir != "/wt" || argAfter(call.args, "--model") != "sonnet" {
		t.Errorf("call = %+v", call)
	}
	if !strings.Contains(call.stdin, "/code-review") {
		t.Errorf("prompt should invoke /code-review: %s", call.stdin)
	}
}

// TestCodeReviewRecordsSessionStage: every review round records its session
// under stageCodeReview, so a park or crash mid-review resumes the review
// session instead of re-resuming the finished execute/debug session.
func TestCodeReviewRecordsSessionStage(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: codeReviewResult("clean", "Nothing to fix.")}}}
	logDir := t.TempDir()
	c := &Claude{runner: f, logDir: logDir}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7, Kind: "feature"}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session not recorded: %v", err)
	}
	if si.SessionID != "cr-1" || si.Kind != "feature" || si.Stage != stageCodeReview {
		t.Errorf("session = %+v, want cr-1/feature/codereview", si)
	}
}

// TestCodeReviewRecordsSessionOnError mirrors the pipeline stages: an errored
// round still records its session id, which is exactly what the post-park
// re-entry resumes.
func TestCodeReviewRecordsSessionOnError(t *testing.T) {
	tgt := &fakeCodeReviewTarget{prNum: 42}
	f := &fakeRunner{queue: []rresp{{stdout: claudeErrorJSON("You've hit your session limit", "cr-429")}}}
	logDir := t.TempDir()
	c := &Claude{runner: f, logDir: logDir}
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7, Kind: "bug"}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(1), "/wt", "ai/issue-7", "main", logDir); err == nil {
		t.Fatal("want the round error propagated so ship parks")
	}
	si, err := readSession(logDir)
	if err != nil {
		t.Fatalf("session must be recorded even when the round errors: %v", err)
	}
	if si.SessionID != "cr-429" || si.Stage != stageCodeReview {
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
	c := &Claude{runner: nil, logDir: logDir}
	c.RecordSession("cr-parked", "feature", stageCodeReview)
	f := &fakeRunner{queue: []rresp{
		{stdout: codeReviewResult("fixed", "- finished the cut-short fix")},
		{stdout: codeReviewResult("clean", "Nothing left.")},
	}}
	c.runner = f
	cr := &CodeReview{Target: tgt, Push: noopPush, Num: 7, Kind: "feature"}
	if err := cr.Run(context.Background(), c, codeReviewTestConfig(3), "/wt", "ai/issue-7", "main", logDir); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatalf("claude calls = %d, want 2 (resumed round 2, fresh round 3)", len(f.calls))
	}
	if got := argAfter(f.calls[0].args, "--resume"); got != "cr-parked" {
		t.Errorf("first call --resume = %q, want cr-parked (continue the cut-short round)", got)
	}
	if f.calls[0].stdin != "continue" {
		t.Errorf("first call prompt = %q, want \"continue\"", f.calls[0].stdin)
	}
	if got := argAfter(f.calls[1].args, "--resume"); got != "" {
		t.Errorf("second call --resume = %q, want a fresh session for the next round", got)
	}
	if len(tgt.comments) != 2 || !strings.Contains(tgt.comments[0], "round 2/3") {
		t.Errorf("comments = %v, want the resumed round posted as round 2/3", tgt.comments)
	}
}
