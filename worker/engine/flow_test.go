package engine

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// flowEnv is a whole-flow dry-run environment: it simulates gh (with REAL
// label state that add/remove/swap calls mutate, so eligibility works across
// cycles), git (always succeeding, 2 commits ahead), and claude (scripted per
// test via the claude func). Tests drive full lifecycles through the
// Orchestrator — pickup, pipeline, park, human label removal, resume, ship —
// without any real subprocess.
type flowEnv struct {
	t       *testing.T
	f       *testkit.FakeRunner
	workDir string
	wt      string // the deterministic worktree path for issue 7

	mu     sync.Mutex
	labels map[string]bool

	claude func(c testkit.RCall) (string, string, error)
}

const flowIssue = 7

func newFlowEnv(t *testing.T) *flowEnv {
	t.Helper()
	workDir := t.TempDir()
	env := &flowEnv{
		t: t, f: &testkit.FakeRunner{}, workDir: workDir,
		wt:     shared.WorktreePath(workDir, flowIssue),
		labels: map[string]bool{"ai-agent": true},
	}
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		switch c.Name {
		case "gh":
			switch {
			case strings.HasPrefix(joined, "issue list"):
				return env.issueListJSON(), "", nil
			case strings.HasPrefix(joined, "issue view"):
				return `{"title": "Add exporter", "body": "want an exporter", "comments": []}`, "", nil
			case strings.HasPrefix(joined, "issue edit"):
				env.applyLabelEdit(c.Args)
				return "", "", nil
			case strings.HasPrefix(joined, "issue close"):
				return "", "", nil
			case strings.HasPrefix(joined, "pr create"):
				return "https://github.com/org/repo/pull/99\n", "", nil
			case strings.HasPrefix(joined, "pr view"), strings.HasPrefix(joined, "pr list"):
				return `{"number": 99, "url": "https://github.com/org/repo/pull/99"}`, "", nil
			}
			return "", "", nil
		case "git":
			switch {
			case strings.Contains(joined, "symbolic-ref"):
				return "origin/main\n", "", nil
			case strings.Contains(joined, "rev-list --count"):
				return "2\n", "", nil
			}
			return "", "", nil
		case "claude":
			return env.claude(c)
		}
		return "", "", nil
	}
	return env
}

func (e *flowEnv) issueListJSON() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ls []string
	for name, on := range e.labels {
		if on {
			ls = append(ls, fmt.Sprintf(`{"name": %q}`, name))
		}
	}
	return fmt.Sprintf(`[{"number": %d, "title": "Add exporter", "labels": [%s]}]`,
		flowIssue, strings.Join(ls, ","))
}

// applyLabelEdit mutates label state from a gh issue edit call's
// --add-label/--remove-label flags (a swap carries both in one call).
func (e *flowEnv) applyLabelEdit(args []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, a := range args {
		if i+1 >= len(args) {
			break
		}
		switch a {
		case "--add-label":
			e.labels[args[i+1]] = true
		case "--remove-label":
			delete(e.labels, args[i+1])
		}
	}
}

func (e *flowEnv) hasGHLabel(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.labels[name]
}

// humanRemovesLabel simulates the only manual action the flow needs: a person
// removing a state label on GitHub, which makes the issue eligible again.
func (e *flowEnv) humanRemovesLabel(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.labels, name)
}

func (e *flowEnv) orchestrator() *Orchestrator {
	cfg := &shared.Config{
		RepoPath: "/clone", RepoSlug: "org/repo", EligibleLabel: "ai-agent",
		WorkDir: e.workDir, MaxQARounds: 3, StateLabels: testStateLabels(),
		Models: shared.Models{
			Architect:  shared.ModelConfig{Model: "opus", Effort: "high"},
			Answerer:   shared.ModelConfig{Model: "sonnet"},
			CodeReview: &shared.CodeReviewConfig{ModelConfig: shared.ModelConfig{Model: "opus"}, Rounds: 1},
		},
	}
	return newTestOrch(cfg, e.f)
}

func (e *flowEnv) logDir() string {
	return shared.IssueLogDir(e.workDir, flowIssue)
}

// claudePrompts returns the stdin of every claude call whose prompt STARTS
// with prefix. Prompts are classified by their leading text (the entry
// prompt's fixed first line, or a skill invocation like
// "/superpowers:writing-plans"), never by substring: the entry prompt
// mentions the writing-plans skill in prose, so substring matching
// misclassifies it.
func (e *flowEnv) claudePrompts(prefix string) []string {
	var out []string
	for _, c := range e.f.Snapshot() {
		if c.Name == "claude" && strings.HasPrefix(c.Stdin, prefix) {
			out = append(out, c.Stdin)
		}
	}
	return out
}

// claudeResumes returns, for every claude call with --resume id, its stdin.
func (e *flowEnv) claudeResumes(id string) []string {
	var out []string
	for _, c := range e.f.Snapshot() {
		if c.Name == "claude" && testkit.ArgAfter(c.Args, "--resume") == id {
			out = append(out, c.Stdin)
		}
	}
	return out
}

// realChain returns the issue's chain nodes that carry a session id.
func (e *flowEnv) realChain() []shared.SessionNode {
	var out []shared.SessionNode
	for _, n := range shared.ReadSessionChain(e.logDir()) {
		if n.ID != "" {
			out = append(out, n)
		}
	}
	return out
}

// entryPromptPrefix is the merged entry prompt's fixed first line — the flow
// tests classify entry calls by it, mirroring how the other phases are
// classified by their leading skill invocation.
const entryPromptPrefix = "Handle this GitHub issue"

// featureScript scripts a well-behaved feature-outcome pipeline: the entry
// session commits a spec, plan commits a plan, execute succeeds, code review
// is clean, and any other session (UAT, answerer) returns a bland success.
// Tests override individual phases by wrapping it.
func (e *flowEnv) featureScript() func(c testkit.RCall) (string, string, error) {
	return func(c testkit.RCall) (string, string, error) {
		switch {
		case strings.HasPrefix(c.Stdin, entryPromptPrefix):
			writeSpecFile(e.t, e.wt)
			return testkit.ClaudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-1"), "", nil
		case strings.HasPrefix(c.Stdin, "/superpowers:writing-plans"):
			writePlanFile(e.t, e.wt)
			return testkit.ClaudeJSON("Plan written.\nPIPELINE_READY", "plan-1"), "", nil
		case strings.HasPrefix(c.Stdin, "/superpowers:executing-plans"), c.Stdin == "continue":
			return testkit.ClaudeJSON("Executed.", "exec-1"), "", nil
		case strings.Contains(c.Stdin, "/code-review"):
			return testkit.ClaudeJSON(codeReviewBeginSentinel+"\nSTATUS: clean\nNothing to fix.\n"+codeReviewEndSentinel, "cr-1"), "", nil
		}
		return testkit.ClaudeJSON("ok", "eph-1"), "", nil
	}
}

// TestFlowFeatureHappyPath drives the full feature lifecycle in one cycle —
// pickup -> brainstorm -> spec -> plan -> execute -> ship -> code review ->
// done — and verifies the lineage chain records every primary session linked
// to the session that spawned it.
func TestFlowFeatureHappyPath(t *testing.T) {
	env := newFlowEnv(t)
	env.claude = env.featureScript()
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}

	if !env.hasGHLabel("ai-done") || env.hasGHLabel("ai-wip") {
		t.Errorf("labels = %v, want ai-done without ai-wip", env.labels)
	}
	if got := shared.ReadState(env.logDir()); got != "ai-done" {
		t.Errorf("state marker = %q, want ai-done", got)
	}

	chain := env.realChain()
	if len(chain) != 4 {
		t.Fatalf("real chain = %+v, want entry/plan/execute/codereview", chain)
	}
	wantStages := []string{shared.StageEntry, shared.StagePlan, shared.StageExecute, shared.StageCodeReview}
	for i, want := range wantStages {
		if chain[i].Stage != want {
			t.Errorf("chain[%d].Stage = %q, want %q", i, chain[i].Stage, want)
		}
	}
	// Each session links to the one that spawned it: a -> b -> c -> d.
	for i := 1; i < len(chain); i++ {
		if chain[i].Parent != chain[i-1].ID {
			t.Errorf("chain[%d].Parent = %q, want %q (its spawner)", i, chain[i].Parent, chain[i-1].ID)
		}
	}
	if head, _ := shared.HeadSession(env.logDir()); head.Stage != shared.StageCodeReview {
		t.Errorf("head = %+v, want the codereview session as the final resume point", head)
	}
}

// TestFlowExecuteKilledParksThenReworkRemovalResumesSameSession is the outage
// scenario end to end: the execute session is killed mid-run (usage limit /
// network drop — the CLI dies after its session id streamed), the issue parks
// as ai-rework, a human removes the label, and the next cycle must resume THE
// SAME execute session with --resume and "continue" — no fresh brainstorm, no
// lost stage.
func TestFlowExecuteKilledParksThenReworkRemovalResumesSameSession(t *testing.T) {
	env := newFlowEnv(t)
	happy := env.featureScript()
	killed := false
	env.claude = func(c testkit.RCall) (string, string, error) {
		if strings.HasPrefix(c.Stdin, "/superpowers:executing-plans") && !killed {
			killed = true
			partial := `{"type":"system","subtype":"init","session_id":"exec-dead"}` + "\n"
			return partial, "usage limit", errors.New("signal: killed")
		}
		if testkit.ArgAfter(c.Args, "--resume") == "exec-dead" {
			return testkit.ClaudeJSON("Executed after resume.", "exec-2"), "", nil
		}
		return happy(c)
	}
	o := env.orchestrator()

	// Cycle 1: runs brainstorm/plan, dies mid-execute, parks.
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") || env.hasGHLabel("ai-wip") {
		t.Fatalf("labels after crash = %v, want parked as ai-rework", env.labels)
	}
	if head, _ := shared.HeadSession(env.logDir()); head.ID != "exec-dead" || head.Stage != shared.StageExecute {
		t.Fatalf("head after crash = %+v, want the killed execute session checkpointed", head)
	}

	// The human removes the rework label; the next cycle picks the issue up.
	env.humanRemovesLabel("ai-rework")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	if got := env.claudeResumes("exec-dead"); len(got) != 1 || got[0] != "continue" {
		t.Errorf("resumes of exec-dead = %q, want exactly one with prompt \"continue\"", got)
	}
	if n := len(env.claudePrompts(entryPromptPrefix)); n != 1 {
		t.Errorf("brainstorm calls = %d, want 1 — a resume must not restart the pipeline", n)
	}
	if n := len(env.claudePrompts("/superpowers:writing-plans")); n != 1 {
		t.Errorf("plan calls = %d, want 1 — the completed plan stage must not re-run", n)
	}
	if !env.hasGHLabel("ai-done") || env.hasGHLabel("ai-rework") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
	// Lineage: the resumed execute session descends from the killed one.
	chain := env.realChain()
	byID := map[string]shared.SessionNode{}
	for _, n := range chain {
		byID[n.ID] = n
	}
	if n, ok := byID["exec-2"]; !ok || n.Parent != "exec-dead" {
		t.Errorf("exec-2 node = %+v, want parent exec-dead", n)
	}
	if shared.ReadParkCause(env.logDir()) != "" {
		t.Error("park cause must be cleared once the issue ships")
	}
}

// TestFlowPlanDiesBeforeSessionStartsResumesFreshFromSpec covers the pending
// checkpoint: the plan CLI dies before any session id streams (immediate
// crash), so the chain head is the pending plan node. The re-entry must run a
// FRESH plan session on the committed spec — no --resume, and no brainstorm.
func TestFlowPlanDiesBeforeSessionStartsResumesFreshFromSpec(t *testing.T) {
	env := newFlowEnv(t)
	happy := env.featureScript()
	crashed := false
	env.claude = func(c testkit.RCall) (string, string, error) {
		if strings.HasPrefix(c.Stdin, "/superpowers:writing-plans") && !crashed {
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
	if node, ok := shared.ResumePoint(env.logDir()); !ok || node.ID != "" || node.Stage != shared.StagePlan {
		t.Fatalf("resume point after crash = %+v ok=%v, want the pending plan node", node, ok)
	}

	env.humanRemovesLabel("ai-rework")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	if n := len(env.claudePrompts(entryPromptPrefix)); n != 1 {
		t.Errorf("brainstorm calls = %d, want 1 — the spec is committed, re-entry must not redesign", n)
	}
	planCalls := env.claudePrompts("/superpowers:writing-plans")
	if len(planCalls) != 2 {
		t.Fatalf("plan calls = %d, want the crashed one plus one fresh re-run", len(planCalls))
	}
	if !strings.Contains(planCalls[1], "2026-07-13-thing-design.md") {
		t.Errorf("fresh plan prompt = %q, want it to carry the checkpointed spec", planCalls[1])
	}
	if got := env.claudeResumes("arch-1"); len(got) != 0 {
		t.Errorf("re-entry resumed the architect session %q — the issue-5 incident regressed", got)
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
}

// TestFlowEntryUsageLimitParksThenResumesAsFix drives the fix lane through a
// 429: the entry session errors with is_error (session id intact), parks, and
// the rework-removal re-entry resumes that exact session with the
// restated-contract wrapper around "continue". The resumed session commits a
// fix, so the run ships with the derived kind "bug".
func TestFlowEntryUsageLimitParksThenResumesAsFix(t *testing.T) {
	env := newFlowEnv(t)
	limited := false
	env.claude = func(c testkit.RCall) (string, string, error) {
		switch {
		case strings.Contains(c.Stdin, "/code-review"):
			return testkit.ClaudeJSON(codeReviewBeginSentinel+"\nSTATUS: clean\nok\n"+codeReviewEndSentinel, "cr-1"), "", nil
		case testkit.ArgAfter(c.Args, "--resume") == "s-429":
			return testkit.ClaudeJSON("FIX_COMMITTED: fixed the crash", "debug-2"), "", nil
		case !limited:
			limited = true
			return testkit.ClaudeErrorJSON("Claude AI usage limit reached", "s-429"), "", nil
		}
		return testkit.ClaudeJSON("ok", "eph-1"), "", nil
	}
	o := env.orchestrator()

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if !env.hasGHLabel("ai-rework") {
		t.Fatalf("labels after 429 = %v, want parked as ai-rework", env.labels)
	}
	// The kind is empty on the checkpoint: the session died before its outcome
	// could resolve it.
	if head, _ := shared.HeadSession(env.logDir()); head.ID != "s-429" || head.Kind != "" || head.Stage != shared.StageEntry {
		t.Fatalf("head after 429 = %+v, want the limited entry session", head)
	}

	env.humanRemovesLabel("ai-rework")
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}

	got := env.claudeResumes("s-429")
	if len(got) != 1 || !strings.HasPrefix(got[0], "continue") {
		t.Errorf("resumes of s-429 = %q, want exactly one leading with the continue trigger", got)
	}
	if !env.hasGHLabel("ai-done") || env.hasGHLabel("ai-rework") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
	if got := shared.ResolvedKind(env.logDir()); got != "bug" {
		t.Errorf("ResolvedKind = %q, want bug derived from the fix outcome", got)
	}
}

// A LEGACY bug chain (checkpointed by the pre-merge pipeline as kind "bug",
// stage "debug") still resumes its debug session with the bare "continue"
// trigger and ships — old in-flight work is never restarted from zero.
func TestFlowLegacyBugChainResumesDebugSession(t *testing.T) {
	env := newFlowEnv(t)
	env.claude = func(c testkit.RCall) (string, string, error) {
		switch {
		case strings.Contains(c.Stdin, "/code-review"):
			return testkit.ClaudeJSON(codeReviewBeginSentinel+"\nSTATUS: clean\nok\n"+codeReviewEndSentinel, "cr-1"), "", nil
		case testkit.ArgAfter(c.Args, "--resume") == "legacy-debug":
			return testkit.ClaudeJSON("Fixed and committed.", "debug-2"), "", nil
		}
		return testkit.ClaudeJSON("ok", "eph-1"), "", nil
	}
	// Seed the pre-merge chain residue: a debug session checkpointed by the
	// old bug pipeline.
	shared.AppendSessionNode(env.logDir(), shared.SessionNode{ID: "legacy-debug", Kind: "bug", Stage: shared.StageDebug})
	o := env.orchestrator()

	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
	if got := env.claudeResumes("legacy-debug"); len(got) != 1 || got[0] != "continue" {
		t.Errorf("resumes of legacy-debug = %q, want exactly one with the bare \"continue\"", got)
	}
	if n := len(env.claudePrompts(entryPromptPrefix)); n != 0 {
		t.Errorf("fresh entry calls = %d, want 0 — a legacy chain resumes its own route", n)
	}
	if !env.hasGHLabel("ai-done") {
		t.Errorf("final labels = %v, want ai-done", env.labels)
	}
}
