package main

import (
	"os"
	"path/filepath"
	"testing"
)

// --- domain: the session chain (linked list of primary Claude sessions) ---

func TestAppendAndReadSessionChain(t *testing.T) {
	dir := t.TempDir()
	appendSessionNode(dir, SessionNode{ID: "sa", Kind: "feature", Stage: stageBrainstorm})
	appendSessionNode(dir, SessionNode{ID: "sb", Parent: "sa", Kind: "feature", Stage: stagePlan, Artifact: "specs/x.md"})
	appendSessionNode(dir, SessionNode{ID: "sc", Parent: "sb", Kind: "feature", Stage: stageExecute, Artifact: "plans/x.md"})

	chain := readSessionChain(dir)
	if len(chain) != 3 {
		t.Fatalf("chain length = %d, want 3", len(chain))
	}
	if chain[0].ID != "sa" || chain[0].Stage != stageBrainstorm || chain[0].Parent != "" {
		t.Errorf("chain[0] = %+v", chain[0])
	}
	if chain[1].ID != "sb" || chain[1].Parent != "sa" || chain[1].Artifact != "specs/x.md" {
		t.Errorf("chain[1] = %+v", chain[1])
	}
	if chain[2].ID != "sc" || chain[2].Parent != "sb" || chain[2].Stage != stageExecute {
		t.Errorf("chain[2] = %+v", chain[2])
	}
}

func TestReadSessionChainSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	appendSessionNode(dir, SessionNode{ID: "sa", Kind: "bug", Stage: stageDebug})
	f, err := os.OpenFile(filepath.Join(dir, chainFile), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	appendSessionNode(dir, SessionNode{ID: "sb", Parent: "sa", Kind: "bug", Stage: stageDebug})

	chain := readSessionChain(dir)
	if len(chain) != 2 || chain[0].ID != "sa" || chain[1].ID != "sb" {
		t.Errorf("chain = %+v, want the two good nodes", chain)
	}
}

func TestReadSessionChainMissingIsEmpty(t *testing.T) {
	if chain := readSessionChain(t.TempDir()); len(chain) != 0 {
		t.Errorf("chain = %+v, want empty", chain)
	}
}

func TestAppendSessionNodeEmptyLogDirIsNoop(t *testing.T) {
	appendSessionNode("", SessionNode{ID: "sa"}) // must not panic or write anywhere
}

func TestHeadSessionReturnsNewestNode(t *testing.T) {
	dir := t.TempDir()
	appendSessionNode(dir, SessionNode{ID: "sa", Kind: "feature", Stage: stageBrainstorm})
	appendSessionNode(dir, SessionNode{ID: "sb", Parent: "sa", Kind: "feature", Stage: stagePlan})

	head, ok := headSession(dir)
	if !ok || head.ID != "sb" || head.Stage != stagePlan {
		t.Errorf("head = %+v ok=%v, want sb/plan", head, ok)
	}
}

func TestHeadSessionEmpty(t *testing.T) {
	if _, ok := headSession(t.TempDir()); ok {
		t.Error("want ok=false on an empty chain")
	}
}

// --- resume point: chain head first, legacy session file as fallback ---

func TestResumePointPrefersChainHead(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordCheckpoint(SessionInfo{SessionID: "legacy", Kind: "feature", Stage: stageBrainstorm})
	appendSessionNode(dir, SessionNode{ID: "sc", Kind: "feature", Stage: stageExecute, Artifact: "plans/x.md"})

	node, ok := resumePoint(dir)
	if !ok || node.ID != "sc" || node.Stage != stageExecute {
		t.Errorf("node = %+v ok=%v, want the chain head sc/execute", node, ok)
	}
}

// A legacy post-call session record (no artifact) maps to a resumable node:
// the recorded id belongs to the recorded stage.
func TestResumePointLegacySessionFile(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordCheckpoint(SessionInfo{SessionID: "sess-1", Kind: "bug", Stage: stageDebug})

	node, ok := resumePoint(dir)
	if !ok || node.ID != "sess-1" || node.Kind != "bug" || node.Stage != stageDebug {
		t.Errorf("node = %+v ok=%v, want sess-1/bug/debug", node, ok)
	}
}

// A legacy PRE-CALL checkpoint (stagePlan+SpecPath / stageExecute+PlanPath)
// carries the PREVIOUS stage's session id, so the mapped node must NOT offer
// that id for resume: ID stays empty and the artifact drives a fresh re-run.
func TestResumePointLegacyPreCallCheckpointHasNoResumableID(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{logDir: dir}
	c.RecordCheckpoint(SessionInfo{SessionID: "architect-id", Kind: "feature", Stage: stagePlan, SpecPath: "specs/x.md"})

	node, ok := resumePoint(dir)
	if !ok {
		t.Fatal("want a resume point from the legacy checkpoint")
	}
	if node.ID != "" {
		t.Errorf("node.ID = %q, want empty (the recorded id belongs to the previous stage)", node.ID)
	}
	if node.Stage != stagePlan || node.Artifact != "specs/x.md" {
		t.Errorf("node = %+v, want plan stage from specs/x.md", node)
	}

	c.RecordCheckpoint(SessionInfo{SessionID: "plan-id", Kind: "feature", Stage: stageExecute, PlanPath: "plans/y.md"})
	node, ok = resumePoint(dir)
	if !ok || node.ID != "" || node.Stage != stageExecute || node.Artifact != "plans/y.md" {
		t.Errorf("node = %+v ok=%v, want execute stage from plans/y.md with no id", node, ok)
	}
}

func TestResumePointNothingRecorded(t *testing.T) {
	if _, ok := resumePoint(t.TempDir()); ok {
		t.Error("want ok=false when neither chain nor session file exists")
	}
}
