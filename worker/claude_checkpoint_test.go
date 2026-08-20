package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// streamWithInit builds a stream-json payload whose init event carries the
// session id, followed by a terminal result event — the shape the real CLI
// emits.
func streamWithInit(session, result string) string {
	return fmt.Sprintf(`{"type":"system","subtype":"init","session_id":%q}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
{"type":"result","is_error":false,"result":%q,"session_id":%q,"total_cost_usd":0.5}
`, session, result, session)
}

func TestCallCheckpointRecordsChainNodeOnSuccess(t *testing.T) {
	dir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: streamWithInit("s1", "ok")}}}
	c := &Claude{runner: f, logDir: dir}
	_, err := c.Call(context.Background(), ClaudeCall{
		Label: "brainstorm-0", Prompt: "x",
		Checkpoint: &CallCheckpoint{Kind: "feature", Stage: stageBrainstorm},
	})
	if err != nil {
		t.Fatal(err)
	}
	head, ok := headSession(dir)
	if !ok || head.ID != "s1" || head.Kind != "feature" || head.Stage != stageBrainstorm {
		t.Errorf("head = %+v ok=%v, want s1/feature/brainstorm", head, ok)
	}
	// The legacy session file is kept in step for the dashboard.
	si, err := readSession(dir)
	if err != nil || si.SessionID != "s1" || si.Kind != "feature" || si.Stage != stageBrainstorm {
		t.Errorf("legacy session = %+v err=%v, want s1/feature/brainstorm", si, err)
	}
}

// The whole point of in-flight checkpointing: a CLI killed mid-run (usage
// limit crash, network drop, SIGKILL) never returns a result JSON, but the
// session id already streamed in the init event — so the chain node must
// exist and the salvaged id must ride back with the error.
func TestCallCheckpointSurvivesKilledCLI(t *testing.T) {
	dir := t.TempDir()
	partial := `{"type":"system","subtype":"init","session_id":"s-dead"}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
`
	f := &fakeRunner{queue: []rresp{{stdout: partial, err: fmt.Errorf("signal: killed")}}}
	c := &Claude{runner: f, logDir: dir}
	res, err := c.Call(context.Background(), ClaudeCall{
		Label: "execute", Prompt: "x",
		Checkpoint: &CallCheckpoint{Kind: "feature", Stage: stageExecute, Artifact: "plans/x.md"},
	})
	if err == nil {
		t.Fatal("want error from the killed CLI")
	}
	head, ok := headSession(dir)
	if !ok || head.ID != "s-dead" || head.Stage != stageExecute || head.Artifact != "plans/x.md" {
		t.Errorf("head = %+v ok=%v, want the in-flight checkpoint s-dead/execute", head, ok)
	}
	if res == nil || res.SessionID != "s-dead" {
		t.Errorf("res = %+v, want the salvaged session id alongside the error", res)
	}
}

func TestCallCheckpointLinksParentFromChain(t *testing.T) {
	dir := t.TempDir()
	appendSessionNode(dir, SessionNode{ID: "sa", Kind: "feature", Stage: stageBrainstorm})
	f := &fakeRunner{queue: []rresp{{stdout: streamWithInit("sb", "PIPELINE_READY")}}}
	c := &Claude{runner: f, logDir: dir}
	_, err := c.Call(context.Background(), ClaudeCall{
		Label: "plan", Prompt: "x",
		Checkpoint: &CallCheckpoint{Kind: "feature", Stage: stagePlan, Artifact: "specs/x.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	head, _ := headSession(dir)
	if head.ID != "sb" || head.Parent != "sa" || head.Artifact != "specs/x.md" {
		t.Errorf("head = %+v, want sb spawned by sa", head)
	}
}

// A pending stage node (no id yet) must not become the parent — the parent is
// the previous real session.
func TestCallCheckpointParentSkipsPendingNodes(t *testing.T) {
	dir := t.TempDir()
	appendSessionNode(dir, SessionNode{ID: "sp", Kind: "feature", Stage: stagePlan})
	appendSessionNode(dir, SessionNode{Kind: "feature", Stage: stageExecute, Artifact: "plans/x.md"}) // pending
	f := &fakeRunner{queue: []rresp{{stdout: streamWithInit("se", "done")}}}
	c := &Claude{runner: f, logDir: dir}
	_, err := c.Call(context.Background(), ClaudeCall{
		Label: "execute", Prompt: "x",
		Checkpoint: &CallCheckpoint{Kind: "feature", Stage: stageExecute, Artifact: "plans/x.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	head, _ := headSession(dir)
	if head.ID != "se" || head.Parent != "sp" {
		t.Errorf("head = %+v, want se with parent sp (not the pending node)", head)
	}
}

func TestCallWithoutCheckpointWritesNoChainOrSession(t *testing.T) {
	dir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: streamWithInit("s-eph", "answer")}}}
	c := &Claude{runner: f, logDir: dir}
	if _, err := c.Call(context.Background(), ClaudeCall{Label: "answer-1", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, chainFile)); err == nil {
		t.Error("ephemeral call must not append to the session chain")
	}
	if _, err := os.Stat(filepath.Join(dir, sessionFile)); err == nil {
		t.Error("ephemeral call must not write the session file")
	}
}

// A resumed round of the SAME session at the SAME stage must not grow the
// chain — one session id, one checkpoint.
func TestCallCheckpointDedupesResumedSameSession(t *testing.T) {
	dir := t.TempDir()
	appendSessionNode(dir, SessionNode{ID: "s1", Kind: "feature", Stage: stageBrainstorm})
	f := &fakeRunner{queue: []rresp{{stdout: streamWithInit("s1", "round 2")}}}
	c := &Claude{runner: f, logDir: dir}
	_, err := c.Call(context.Background(), ClaudeCall{
		Label: "brainstorm-1", Prompt: "reply", Resume: "s1",
		Checkpoint: &CallCheckpoint{Kind: "feature", Stage: stageBrainstorm},
	})
	if err != nil {
		t.Fatal(err)
	}
	if chain := readSessionChain(dir); len(chain) != 1 {
		t.Errorf("chain = %+v, want a single node for a single session", chain)
	}
}

// A single-object payload (no streaming, no trailing newline) must still
// checkpoint — via the post-return flush rather than the mid-stream scan.
func TestCallCheckpointSingleObjectPayload(t *testing.T) {
	dir := t.TempDir()
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("ok", "s1")}}}
	c := &Claude{runner: f, logDir: dir}
	if _, err := c.Call(context.Background(), ClaudeCall{
		Label: "debug", Prompt: "x",
		Checkpoint: &CallCheckpoint{Kind: "bug", Stage: stageDebug},
	}); err != nil {
		t.Fatal(err)
	}
	head, ok := headSession(dir)
	if !ok || head.ID != "s1" || head.Kind != "bug" || head.Stage != stageDebug {
		t.Errorf("head = %+v ok=%v, want s1/bug/debug", head, ok)
	}
}

// CheckpointStage records a stage about to start whose session doesn't exist
// yet (the gap between a plan returning and execute spawning): a pending node
// with the artifact but no id, so a crash in the gap resumes as a fresh run
// of that stage on the artifact.
func TestCheckpointStageAppendsPendingNode(t *testing.T) {
	dir := t.TempDir()
	appendSessionNode(dir, SessionNode{ID: "sp", Kind: "feature", Stage: stagePlan})
	c := &Claude{logDir: dir}
	c.CheckpointStage("feature", stageExecute, "plans/x.md")

	node, ok := resumePoint(dir)
	if !ok || node.ID != "" || node.Stage != stageExecute || node.Artifact != "plans/x.md" {
		t.Errorf("resumePoint = %+v ok=%v, want pending execute node on plans/x.md", node, ok)
	}
	if node.Parent != "sp" {
		t.Errorf("pending node parent = %q, want sp", node.Parent)
	}
}
