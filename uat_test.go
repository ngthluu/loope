package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseUATBeginAndEnd(t *testing.T) {
	got, ok := parseUAT("Here you go:\nUAT_BEGIN\n- [ ] click it\nUAT_END\nHope that helps!")
	if !ok {
		t.Fatal("want ok=true")
	}
	if got != "- [ ] click it" {
		t.Errorf("got %q, want the checklist with the surrounding prose stripped", got)
	}
}

// A session that opened the checklist but forgot the closing sentinel still
// publishes: take everything to the end of the result.
func TestParseUATBeginWithoutEnd(t *testing.T) {
	got, ok := parseUAT("UAT_BEGIN\n- [ ] one\n- [ ] two\n")
	if !ok {
		t.Fatal("want ok=true")
	}
	if got != "- [ ] one\n- [ ] two" {
		t.Errorf("got %q, want both items", got)
	}
}

func TestParseUATNoBegin(t *testing.T) {
	if got, ok := parseUAT("I could not find anything to verify."); ok {
		t.Errorf("want ok=false with no begin sentinel, got %q", got)
	}
}

// The bug route self-skips by emitting nothing at all, so an empty result must
// read as "nothing to publish".
func TestParseUATEmptyResult(t *testing.T) {
	if _, ok := parseUAT(""); ok {
		t.Error("want ok=false for an empty result")
	}
}

// Sentinels present but nothing between them is also "nothing to publish".
func TestParseUATEmptyContent(t *testing.T) {
	if got, ok := parseUAT("UAT_BEGIN\n\n   \nUAT_END"); ok {
		t.Errorf("want ok=false for an empty body between the sentinels, got %q", got)
	}
}

func TestUATSectionCarriesMarkerAndHeading(t *testing.T) {
	got := uatSection("- [ ] click it")
	if !strings.HasPrefix(got, uatMarker) {
		t.Errorf("section must lead with the idempotency marker:\n%s", got)
	}
	if !strings.Contains(got, "UAT checklist") || !strings.Contains(got, "- [ ] click it") {
		t.Errorf("section = %q", got)
	}
}

// fakeUATTarget stands in for *GitHub: it records what was appended and can be
// scripted to fail either operation.
type fakeUATTarget struct {
	body      string
	bodyErr   error
	appendErr error
	bodyCalls int
	appended  []string
}

func (f *fakeUATTarget) IssueBody(ctx context.Context, n int) (string, error) {
	f.bodyCalls++
	return f.body, f.bodyErr
}

func (f *fakeUATTarget) AppendIssueBody(ctx context.Context, n int, text string) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	f.appended = append(f.appended, text)
	f.body += "\n\n" + text
	return nil
}

func uatTestConfig() *Config {
	return &Config{Models: Models{UAT: ModelConfig{Model: "sonnet", Effort: "medium", MaxTurns: 30}}}
}

// uatResult builds a fake claude payload whose result carries a fenced checklist.
func uatResult(checklist string) string {
	return claudeJSON("Sure thing.\n"+uatBeginSentinel+"\n"+checklist+"\n"+uatEndSentinel, "uat-1")
}

func TestUATPublishesChecklist(t *testing.T) {
	tgt := &fakeUATTarget{body: "the original body"}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}
	c := &Claude{runner: f}
	u := &UAT{Target: tgt, Num: 7}
	u.RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")

	if len(tgt.appended) != 1 {
		t.Fatalf("appended %d sections, want 1", len(tgt.appended))
	}
	if !strings.Contains(tgt.appended[0], uatMarker) || !strings.Contains(tgt.appended[0], "- [ ] click it") {
		t.Errorf("appended = %q", tgt.appended[0])
	}
	if len(f.calls) != 1 {
		t.Fatalf("claude calls = %d, want 1", len(f.calls))
	}
	call := f.calls[0]
	if call.dir != "/wt" || argAfter(call.args, "--model") != "sonnet" {
		t.Errorf("call = %+v", call)
	}
	// Read-only: the session inspects and reports, it must not edit or commit.
	tools := argAfter(call.args, "--disallowedTools")
	for _, want := range []string{"AskUserQuestion", "Write", "Edit", "NotebookEdit"} {
		if !strings.Contains(tools, want) {
			t.Errorf("--disallowedTools = %q, must include %s", tools, want)
		}
	}
	if !strings.Contains(call.stdin, "docs/spec.md") {
		t.Errorf("prompt should carry the spec path: %s", call.stdin)
	}
}

// Idempotency: a body that already carries the marker costs nothing — no session,
// no append.
func TestUATSkipsWhenMarkerPresent(t *testing.T) {
	tgt := &fakeUATTarget{body: "body\n\n" + uatMarker + "\n\n## 🤖 UAT checklist\n\n- [ ] old"}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] new")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	if len(f.calls) != 0 {
		t.Errorf("claude calls = %d, want 0 — the marker check must run before the session", len(f.calls))
	}
	if len(tgt.appended) != 0 {
		t.Errorf("appended %d sections, want 0", len(tgt.appended))
	}
}

// A failed body fetch also skips: a duplicated UAT section is worse than a
// missing one, and the next run gets another chance.
func TestUATSkipsWhenBodyFetchFails(t *testing.T) {
	tgt := &fakeUATTarget{bodyErr: fmt.Errorf("gh: 503")}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] new")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	if len(f.calls) != 0 {
		t.Errorf("claude calls = %d, want 0", len(f.calls))
	}
	if len(tgt.appended) != 0 {
		t.Errorf("appended %d sections, want 0", len(tgt.appended))
	}
}

func TestUATSkipsWhenSessionErrors(t *testing.T) {
	tgt := &fakeUATTarget{body: "body"}
	f := &fakeRunner{queue: []rresp{{err: fmt.Errorf("exit 1")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	if len(tgt.appended) != 0 {
		t.Errorf("a failed session must publish nothing, got %q", tgt.appended)
	}
}

// The bug route self-skips a branch with no commits: the session prints nothing,
// so there is no begin sentinel to parse.
func TestUATSkipsWhenResultHasNoSentinel(t *testing.T) {
	tgt := &fakeUATTarget{body: "body"}
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("", "uat-1")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	if len(tgt.appended) != 0 {
		t.Errorf("appended %d sections, want 0", len(tgt.appended))
	}
}

// The UAT session is ephemeral: it must never overwrite the resumable primary
// session recorded by the debug/architect/execute call.
func TestUATDoesNotRecordSession(t *testing.T) {
	logDir := t.TempDir()
	c := &Claude{runner: &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}, logDir: logDir}
	c.RecordSession("primary-sess", "bug")
	(&UAT{Target: &fakeUATTarget{body: "body"}, Num: 7}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	si, err := readSession(logDir)
	if err != nil {
		t.Fatal(err)
	}
	if si.SessionID != "primary-sess" || si.Kind != "bug" {
		t.Errorf("session = %+v, want the primary session untouched", si)
	}
}

// The result text is persisted by Claude.Call as <seq>-uat.output.md — that is
// the "logged as a file" half of the requirement, with no extra plumbing.
func TestUATLogsResultAsOutputFile(t *testing.T) {
	logDir := t.TempDir()
	c := &Claude{runner: &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}, logDir: logDir}
	(&UAT{Target: &fakeUATTarget{body: "body"}, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	matches, err := filepath.Glob(filepath.Join(logDir, "*-uat.output.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("uat output files = %v, want exactly one", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "- [ ] click it") {
		t.Errorf("output file = %q", data)
	}
}

func TestUATTruncatesOversizedChecklist(t *testing.T) {
	tgt := &fakeUATTarget{body: "body"}
	huge := strings.Repeat("x", maxUATChars+500)
	f := &fakeRunner{queue: []rresp{{stdout: uatResult(huge)}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	if len(tgt.appended) != 1 {
		t.Fatalf("appended %d sections, want 1 (oversized is truncated, not skipped)", len(tgt.appended))
	}
	if n := strings.Count(tgt.appended[0], "x"); n != maxUATChars {
		t.Errorf("checklist kept %d chars, want it truncated to %d", n, maxUATChars)
	}
}

func TestUATSkipsWhenResultingBodyTooLarge(t *testing.T) {
	tgt := &fakeUATTarget{body: strings.Repeat("y", maxIssueBodyChars)}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}
	c := &Claude{runner: f}
	(&UAT{Target: tgt, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	if len(tgt.appended) != 0 {
		t.Errorf("a body that would exceed the cap must be skipped, not appended: %q", tgt.appended)
	}
}

func TestUATSurvivesAppendFailure(t *testing.T) {
	tgt := &fakeUATTarget{body: "body", appendErr: fmt.Errorf("gh: 422")}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}
	c := &Claude{runner: f}
	// No panic, no error to propagate: the pipeline continues.
	(&UAT{Target: tgt, Num: 7}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
}

// StartFeature runs the session on its own goroutine — the caller gets control
// back while it is still in flight — and its wait func joins it, so the
// checklist is published by the time wait returns.
func TestUATStartFeatureRunsInBackgroundAndWaitJoins(t *testing.T) {
	tgt := &fakeUATTarget{body: "the issue body"}
	f := &fakeRunner{queue: []rresp{{stdout: uatResult("- [ ] click it")}}}
	release := make(chan struct{})
	g := &gateRunner{inner: f, gate: func(dir, name, stdin string) chan struct{} { return release }}
	c := &Claude{runner: g}

	wait := (&UAT{Target: tgt, Num: 7}).StartFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	// The session is parked in the gate, so StartFeature plainly did not run it
	// inline: control is back here with nothing published yet.
	if len(tgt.appended) != 0 {
		t.Fatalf("StartFeature ran the session inline: appended %d sections", len(tgt.appended))
	}
	close(release)
	wait()
	if len(tgt.appended) != 1 {
		t.Errorf("appended %d sections after wait(), want 1 — wait must join the session", len(tgt.appended))
	}
}

// Disabled UAT: no goroutine, no calls, and still a usable wait func.
func TestUATStartFeatureNilReceiverIsSafe(t *testing.T) {
	var u *UAT
	f := &fakeRunner{}
	c := &Claude{runner: f}
	u.StartFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")()
	(&UAT{}).StartFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")()
	if len(f.calls) != 0 {
		t.Errorf("a disabled UAT must make no calls, got %d", len(f.calls))
	}
}

// A nil *UAT (and a UAT with no target) disables the step entirely, so callers
// never need a nil guard.
func TestUATNilReceiverIsSafe(t *testing.T) {
	var u *UAT
	f := &fakeRunner{}
	c := &Claude{runner: f}
	u.RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	u.RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	(&UAT{}).RunFeature(context.Background(), c, uatTestConfig(), "/wt", "docs/spec.md")
	(&UAT{}).RunBug(context.Background(), c, uatTestConfig(), "/wt", "ISSUE", "main")
	if len(f.calls) != 0 {
		t.Errorf("a disabled UAT must make no calls, got %d", len(f.calls))
	}
}
