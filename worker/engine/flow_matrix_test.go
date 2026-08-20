package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// This file holds the flow-level scenarios from docs/worker-flow-test-matrix.md
// that flow_test.go doesn't already cover: an interruption in the middle of
// each remaining phase/session, driven end to end through the Orchestrator.

// setLabel simulates label state applied outside the daemon (a human or the
// dashboard adding a label directly on GitHub).
func (e *flowEnv) setLabel(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.labels[name] = true
}

// claudeCallsWithResume counts claude calls carrying any --resume flag.
func (e *flowEnv) claudeCallsWithResume() int {
	n := 0
	for _, c := range e.f.Snapshot() {
		if c.Name == "claude" && testkit.ArgAfter(c.Args, "--resume") != "" {
			n++
		}
	}
	return n
}

// killedAfterID is the stream a CLI killed mid-run leaves behind: the session
// id already streamed past (so the checkpoint recorded it), then death.
func killedAfterID(id string) (string, string, error) {
	return fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q}`+"\n", id),
		"usage limit", errors.New("signal: killed")
}

// TestFlowEntryKilledMidSessionResumesEntrySession: the entry CLI is killed
// after its session id streamed (P2/M2). The issue parks with the dead session
// as the chain head, and the rework-removal re-entry resumes THAT session — no
// second fresh entry.
func TestFlowEntryKilledMidSessionResumesEntrySession(t *testing.T) {
	env := newFlowEnv(t)
	happy := env.featureScript()
	killed := false
	env.claude = func(c testkit.RCall) (string, string, error) {
		switch {
		case testkit.ArgAfter(c.Args, "--resume") == "arch-dead":
			writeSpecFile(t, env.wt)
			return testkit.ClaudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-2"), "", nil
		case strings.HasPrefix(c.Stdin, entryPromptPrefix) && !killed:
			killed = true
			return killedAfterID("arch-dead")
		}
		return happy(c)
	}
	o := env.orchestrator()

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") {
		t.Fatalf("labels after kill = %v, want parked as ai-rework", env.labels)
	}
	if head, _ := shared.HeadSession(env.logDir()); head.ID != "arch-dead" || head.Stage != shared.StageEntry {
		t.Fatalf("head after kill = %+v, want the killed entry session", head)
	}

	env.humanRemovesLabel("ai-rework")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	got := env.claudeResumes("arch-dead")
	if len(got) != 1 || !strings.Contains(got[0], "continue") {
		t.Errorf("resumes of arch-dead = %q, want exactly one carrying the continue trigger", got)
	}
	if n := len(env.claudePrompts(entryPromptPrefix)); n != 1 {
		t.Errorf("fresh entry calls = %d, want 1 — a resume must not restart the run", n)
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
	// Lineage: the resumed architect turn descends from the killed session.
	for _, n := range env.realChain() {
		if n.ID == "arch-2" && n.Parent != "arch-dead" {
			t.Errorf("arch-2 parent = %q, want arch-dead", n.Parent)
		}
	}
}

// TestFlowEntryDiesBeforeSessionIdRerunsFresh: the entry CLI dies before any
// session id streams (P2/M1). Nothing is checkpointed, so the re-entry has no
// resume point and must run a fully fresh pipeline.
func TestFlowEntryDiesBeforeSessionIdRerunsFresh(t *testing.T) {
	env := newFlowEnv(t)
	happy := env.featureScript()
	crashed := false
	env.claude = func(c testkit.RCall) (string, string, error) {
		if strings.HasPrefix(c.Stdin, entryPromptPrefix) && !crashed {
			crashed = true
			return "", "cannot reach api.anthropic.com", errors.New("exit 1")
		}
		return happy(c)
	}
	o := env.orchestrator()

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") {
		t.Fatalf("labels after crash = %v, want parked as ai-rework", env.labels)
	}
	if _, ok := shared.ResumePoint(env.logDir()); ok {
		t.Fatal("resume point exists after a pre-session crash — nothing should be checkpointed")
	}

	env.humanRemovesLabel("ai-rework")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	if n := len(env.claudePrompts(entryPromptPrefix)); n != 2 {
		t.Errorf("entry calls = %d, want the crashed one plus one fresh re-run", n)
	}
	if n := env.claudeCallsWithResume(); n != 0 {
		t.Errorf("calls with --resume = %d, want 0 — there was never a session to resume", n)
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
}

// TestFlowQARoundsExhaustedParks: the entry session never reaches a terminal
// sentinel (P3/M10). After MaxQARounds replies the pipeline errors and the
// issue parks with the budget error as the handover.
func TestFlowQARoundsExhaustedParks(t *testing.T) {
	env := newFlowEnv(t)
	env.claude = func(c testkit.RCall) (string, string, error) {
		switch {
		case strings.HasPrefix(c.Stdin, entryPromptPrefix):
			return testkit.ClaudeJSON("What color should the button be?", "arch-1"), "", nil
		case testkit.ArgAfter(c.Args, "--resume") == "arch-1":
			return testkit.ClaudeJSON("And what size?", "arch-1"), "", nil
		}
		return testkit.ClaudeJSON("ok", "eph-1"), "", nil // answerer replies
	}

	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") || env.hasGHLabel("ai-wip") {
		t.Fatalf("labels = %v, want parked as ai-rework", env.labels)
	}
	if cause := shared.ReadParkCause(env.logDir()); !strings.Contains(cause, "exceeded 3 Q&A rounds") {
		t.Errorf("park cause = %q, want the exhausted-rounds error", cause)
	}
}

// TestFlowExecuteDiesBeforeSessionStartsRerunsFreshFromPlan: the execute CLI
// dies instantly (P6/M1). The chain head is the PENDING execute node written
// before the call, so the re-entry re-runs execute fresh on the committed
// plan — the completed plan stage must not re-run.
func TestFlowExecuteDiesBeforeSessionStartsRerunsFreshFromPlan(t *testing.T) {
	env := newFlowEnv(t)
	happy := env.featureScript()
	crashed := false
	env.claude = func(c testkit.RCall) (string, string, error) {
		if strings.HasPrefix(c.Stdin, "/superpowers:executing-plans") && !crashed {
			crashed = true
			return "", "out of memory", errors.New("exit 137")
		}
		return happy(c)
	}
	o := env.orchestrator()

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") {
		t.Fatalf("labels after crash = %v, want parked as ai-rework", env.labels)
	}
	node, ok := shared.ResumePoint(env.logDir())
	if !ok || node.ID != "" || node.Stage != shared.StageExecute {
		t.Fatalf("resume point = %+v ok=%v, want the pending execute node", node, ok)
	}

	env.humanRemovesLabel("ai-rework")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	if n := len(env.claudePrompts("/superpowers:writing-plans")); n != 1 {
		t.Errorf("plan calls = %d, want 1 — the committed plan stage must not re-run", n)
	}
	if n := len(env.claudePrompts("/superpowers:executing-plans")); n != 2 {
		t.Errorf("execute calls = %d, want the crashed one plus one fresh re-run", n)
	}
	if n := env.claudeCallsWithResume(); n != 0 {
		t.Errorf("calls with --resume = %d, want 0 — the pending node has no session", n)
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
}

// TestFlowFeatureNoCommitsParksAtShip: the pipeline reports success but the
// branch has zero commits over base (P8/M4). Ship must park, not open an
// empty PR.
func TestFlowFeatureNoCommitsParksAtShip(t *testing.T) {
	env := newFlowEnv(t)
	env.claude = env.featureScript()
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "git" && strings.Contains(strings.Join(c.Args, " "), "rev-list --count") {
			return "0\n", "", nil
		}
		return base(c)
	}

	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") {
		t.Fatalf("labels = %v, want parked as ai-rework", env.labels)
	}
	if cause := shared.ReadParkCause(env.logDir()); !strings.Contains(cause, "produced no commits") {
		t.Errorf("park cause = %q, want the no-commits error", cause)
	}
}

// TestFlowShipPushFailureParksThenResumesExecuteHead: every push fails during
// the first run (P8/M7) — the in-pipeline pushes are best-effort, so the run
// reaches ship, whose push failure parks. The chain head is the COMPLETED
// execute session, so the re-entry resumes it with "continue" and then ships.
func TestFlowShipPushFailureParksThenResumesExecuteHead(t *testing.T) {
	env := newFlowEnv(t)
	env.claude = env.featureScript()
	var failPush atomic.Bool
	failPush.Store(true)
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "git" && testkit.HasArg(c.Args, "push") && failPush.Load() {
			return "", "remote: hung up unexpectedly", errors.New("exit 128")
		}
		return base(c)
	}
	o := env.orchestrator()

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") {
		t.Fatalf("labels after push failure = %v, want parked as ai-rework", env.labels)
	}
	if head, _ := shared.HeadSession(env.logDir()); head.ID != "exec-1" || head.Stage != shared.StageExecute {
		t.Fatalf("head after park = %+v, want the completed execute session", head)
	}

	failPush.Store(false)
	env.humanRemovesLabel("ai-rework")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	if got := env.claudeResumes("exec-1"); len(got) != 1 || got[0] != "continue" {
		t.Errorf("resumes of exec-1 = %q, want exactly one with prompt \"continue\"", got)
	}
	if n := len(env.claudePrompts(entryPromptPrefix)); n != 1 {
		t.Errorf("brainstorm calls = %d, want 1 — a ship failure must not restart the pipeline", n)
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
	if shared.ReadParkCause(env.logDir()) != "" {
		t.Error("park cause must be cleared once the issue ships")
	}
}

// TestFlowCodeReviewKilledParksThenReentersShipOnly: a review round is killed
// after its session id streamed (P9/M2). The re-entry must route STRAIGHT to
// ship — none of brainstorm/plan/execute re-run — and resume the cut-short
// round's session with "continue".
func TestFlowCodeReviewKilledParksThenReentersShipOnly(t *testing.T) {
	env := newFlowEnv(t)
	happy := env.featureScript()
	killed := false
	env.claude = func(c testkit.RCall) (string, string, error) {
		switch {
		case testkit.ArgAfter(c.Args, "--resume") == "cr-dead":
			return testkit.ClaudeJSON(codeReviewBeginSentinel+"\nSTATUS: clean\nok\n"+codeReviewEndSentinel, "cr-2"), "", nil
		case strings.Contains(c.Stdin, "/code-review") && !killed:
			killed = true
			return killedAfterID("cr-dead")
		}
		return happy(c)
	}
	o := env.orchestrator()

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") {
		t.Fatalf("labels after review kill = %v, want parked as ai-rework", env.labels)
	}
	if head, _ := shared.HeadSession(env.logDir()); head.ID != "cr-dead" || head.Stage != shared.StageCodeReview {
		t.Fatalf("head after kill = %+v, want the killed review session", head)
	}

	env.humanRemovesLabel("ai-rework")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	if got := env.claudeResumes("cr-dead"); len(got) != 1 || got[0] != "continue" {
		t.Errorf("resumes of cr-dead = %q, want exactly one with prompt \"continue\"", got)
	}
	for _, stage := range []string{entryPromptPrefix, "/superpowers:writing-plans", "/superpowers:executing-plans"} {
		if n := len(env.claudePrompts(stage)); n != 1 {
			t.Errorf("%s calls = %d, want 1 — a review re-entry must skip the pipelines", stage, n)
		}
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
}

// TestFlowNeedsInfoAnswerResumesBrainstormWithDiff: the brainstorm scores
// below the confidence threshold (P2/M5), the issue escalates to
// ai-needs-info, a human answers with a comment and removes the label, and
// the re-entry resumes the SAME architect session with the added-lines diff —
// the answer — as its prompt.
func TestFlowNeedsInfoAnswerResumesBrainstormWithDiff(t *testing.T) {
	env := newFlowEnv(t)
	happy := env.featureScript()
	var answered atomic.Bool
	base := env.f.Handler
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		if c.Name == "gh" && strings.HasPrefix(strings.Join(c.Args, " "), "issue view") && answered.Load() {
			return `{"title": "Add exporter", "body": "want an exporter", "comments": [{"author": {"login": "alice"}, "body": "Use Postgres"}]}`, "", nil
		}
		return base(c)
	}
	env.claude = func(c testkit.RCall) (string, string, error) {
		switch {
		case testkit.ArgAfter(c.Args, "--resume") == "arch-low":
			writeSpecFile(t, env.wt)
			return testkit.ClaudeJSON("CONFIDENCE: 90\nSPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-2"), "", nil
		case strings.HasPrefix(c.Stdin, entryPromptPrefix):
			return testkit.ClaudeJSON("CONFIDENCE: 30\nWhich database should the exporter target?", "arch-low"), "", nil
		}
		return happy(c)
	}
	o := env.orchestrator()
	o.cfg.ConfidenceThreshold = 60

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-needs-info") || env.hasGHLabel("ai-wip") {
		t.Fatalf("labels = %v, want escalated to ai-needs-info", env.labels)
	}
	if got := shared.ReadState(env.logDir()); got != "ai-needs-info" {
		t.Errorf("state marker = %q, want ai-needs-info", got)
	}

	// The human answers on the issue and removes the needs-info label.
	answered.Store(true)
	env.humanRemovesLabel("ai-needs-info")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	got := env.claudeResumes("arch-low")
	if len(got) != 1 {
		t.Fatalf("resumes of arch-low = %d, want exactly one", len(got))
	}
	if !strings.Contains(got[0], "Use Postgres") {
		t.Errorf("resume prompt = %q, want it to carry the human's answer as the diff", got[0])
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
}

// TestFlowContinueAfterStopResumesChainHead: an execute session was killed and
// the ticket sits in ai-stopped (the Stop outcome — worktree, chain, and logs
// preserved). Continue re-queues it, and the next cycle resumes the stopped
// run's session in the preserved worktree.
func TestFlowContinueAfterStopResumesChainHead(t *testing.T) {
	env := newFlowEnv(t)
	happy := env.featureScript()
	killed := false
	env.claude = func(c testkit.RCall) (string, string, error) {
		if strings.HasPrefix(c.Stdin, "/superpowers:executing-plans") && !killed {
			killed = true
			return killedAfterID("exec-dead")
		}
		if testkit.ArgAfter(c.Args, "--resume") == "exec-dead" {
			return testkit.ClaudeJSON("Executed after continue.", "exec-2"), "", nil
		}
		return happy(c)
	}
	o := env.orchestrator()

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	// Rewrite the parked outcome into the Stop outcome: ai-stopped label and
	// state marker, chain untouched — the state pause() leaves behind.
	env.humanRemovesLabel("ai-rework")
	env.setLabel("ai-stopped")
	shared.RecordState(env.logDir(), "ai-stopped")

	if err := o.Continue(context.Background(), flowIssue); err != nil {
		t.Fatal(err)
	}
	if env.hasGHLabel("ai-stopped") {
		t.Fatal("Continue must remove the ai-stopped label")
	}
	if got := shared.ReadState(env.logDir()); got != "" {
		t.Fatalf("state marker after Continue = %q, want cleared", got)
	}

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if got := env.claudeResumes("exec-dead"); len(got) != 1 || got[0] != "continue" {
		t.Errorf("resumes of exec-dead = %q, want exactly one with prompt \"continue\"", got)
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
}
