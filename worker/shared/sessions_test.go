package shared

import (
	"os"
	"path/filepath"
	"testing"
)

// --- domain: the session chain (linked list of primary Claude sessions) ---

func TestAppendAndReadSessionChain(t *testing.T) {
	dir := t.TempDir()
	AppendSessionNode(dir, SessionNode{ID: "sa", Kind: "feature", Stage: StageBrainstorm})
	AppendSessionNode(dir, SessionNode{ID: "sb", Parent: "sa", Kind: "feature", Stage: StagePlan, Artifact: "specs/x.md"})
	AppendSessionNode(dir, SessionNode{ID: "sc", Parent: "sb", Kind: "feature", Stage: StageExecute, Artifact: "plans/x.md"})

	chain := ReadSessionChain(dir)
	if len(chain) != 3 {
		t.Fatalf("chain length = %d, want 3", len(chain))
	}
	if chain[0].ID != "sa" || chain[0].Stage != StageBrainstorm || chain[0].Parent != "" {
		t.Errorf("chain[0] = %+v", chain[0])
	}
	if chain[1].ID != "sb" || chain[1].Parent != "sa" || chain[1].Artifact != "specs/x.md" {
		t.Errorf("chain[1] = %+v", chain[1])
	}
	if chain[2].ID != "sc" || chain[2].Parent != "sb" || chain[2].Stage != StageExecute {
		t.Errorf("chain[2] = %+v", chain[2])
	}
}

func TestReadSessionChainSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	AppendSessionNode(dir, SessionNode{ID: "sa", Kind: "bug", Stage: StageDebug})
	f, err := os.OpenFile(filepath.Join(dir, ChainFile), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	AppendSessionNode(dir, SessionNode{ID: "sb", Parent: "sa", Kind: "bug", Stage: StageDebug})

	chain := ReadSessionChain(dir)
	if len(chain) != 2 || chain[0].ID != "sa" || chain[1].ID != "sb" {
		t.Errorf("chain = %+v, want the two good nodes", chain)
	}
}

func TestReadSessionChainMissingIsEmpty(t *testing.T) {
	if chain := ReadSessionChain(t.TempDir()); len(chain) != 0 {
		t.Errorf("chain = %+v, want empty", chain)
	}
}

func TestAppendSessionNodeEmptyLogDirIsNoop(t *testing.T) {
	AppendSessionNode("", SessionNode{ID: "sa"}) // must not panic or write anywhere
}

func TestHeadSessionReturnsNewestNode(t *testing.T) {
	dir := t.TempDir()
	AppendSessionNode(dir, SessionNode{ID: "sa", Kind: "feature", Stage: StageBrainstorm})
	AppendSessionNode(dir, SessionNode{ID: "sb", Parent: "sa", Kind: "feature", Stage: StagePlan})

	head, ok := HeadSession(dir)
	if !ok || head.ID != "sb" || head.Stage != StagePlan {
		t.Errorf("head = %+v ok=%v, want sb/plan", head, ok)
	}
}

func TestHeadSessionEmpty(t *testing.T) {
	if _, ok := HeadSession(t.TempDir()); ok {
		t.Error("want ok=false on an empty chain")
	}
}

// --- resume point: chain head first, legacy session file as fallback ---

func TestResumePointPrefersChainHead(t *testing.T) {
	dir := t.TempDir()
	RecordCheckpoint(dir, SessionInfo{SessionID: "legacy", Kind: "feature", Stage: StageBrainstorm})
	AppendSessionNode(dir, SessionNode{ID: "sc", Kind: "feature", Stage: StageExecute, Artifact: "plans/x.md"})

	node, ok := ResumePoint(dir)
	if !ok || node.ID != "sc" || node.Stage != StageExecute {
		t.Errorf("node = %+v ok=%v, want the chain head sc/execute", node, ok)
	}
}

// A legacy post-call session record (no artifact) maps to a resumable node:
// the recorded id belongs to the recorded stage.
func TestResumePointLegacySessionFile(t *testing.T) {
	dir := t.TempDir()
	RecordCheckpoint(dir, SessionInfo{SessionID: "sess-1", Kind: "bug", Stage: StageDebug})

	node, ok := ResumePoint(dir)
	if !ok || node.ID != "sess-1" || node.Kind != "bug" || node.Stage != StageDebug {
		t.Errorf("node = %+v ok=%v, want sess-1/bug/debug", node, ok)
	}
}

// A legacy PRE-CALL checkpoint (StagePlan+SpecPath / StageExecute+PlanPath)
// carries the PREVIOUS stage's session id, so the mapped node must NOT offer
// that id for resume: ID stays empty and the artifact drives a fresh re-run.
func TestResumePointLegacyPreCallCheckpointHasNoResumableID(t *testing.T) {
	dir := t.TempDir()
	RecordCheckpoint(dir, SessionInfo{SessionID: "architect-id", Kind: "feature", Stage: StagePlan, SpecPath: "specs/x.md"})

	node, ok := ResumePoint(dir)
	if !ok {
		t.Fatal("want a resume point from the legacy checkpoint")
	}
	if node.ID != "" {
		t.Errorf("node.ID = %q, want empty (the recorded id belongs to the previous stage)", node.ID)
	}
	if node.Stage != StagePlan || node.Artifact != "specs/x.md" {
		t.Errorf("node = %+v, want plan stage from specs/x.md", node)
	}

	RecordCheckpoint(dir, SessionInfo{SessionID: "plan-id", Kind: "feature", Stage: StageExecute, PlanPath: "plans/y.md"})
	node, ok = ResumePoint(dir)
	if !ok || node.ID != "" || node.Stage != StageExecute || node.Artifact != "plans/y.md" {
		t.Errorf("node = %+v ok=%v, want execute stage from plans/y.md with no id", node, ok)
	}
}

func TestResumePointNothingRecorded(t *testing.T) {
	if _, ok := ResumePoint(t.TempDir()); ok {
		t.Error("want ok=false when neither chain nor session file exists")
	}
}

// SetHeadKind rewrites only the head node's kind, keeps the rest of the chain
// byte-identical, and mirrors the stamp into the legacy session file.
func TestSetHeadKindStampsHeadOnly(t *testing.T) {
	dir := t.TempDir()
	AppendSessionNode(dir, SessionNode{ID: "a", Kind: "feature", Stage: StagePlan})
	AppendSessionNode(dir, SessionNode{ID: "b", Parent: "a", Kind: "", Stage: StageEntry, Artifact: "x.md"})

	SetHeadKind(dir, "bug")

	chain := ReadSessionChain(dir)
	if len(chain) != 2 {
		t.Fatalf("chain = %+v, want 2 nodes", chain)
	}
	if chain[0].Kind != "feature" || chain[0].ID != "a" {
		t.Errorf("chain[0] = %+v, want the earlier node untouched", chain[0])
	}
	if chain[1].Kind != "bug" || chain[1].ID != "b" || chain[1].Artifact != "x.md" {
		t.Errorf("chain[1] = %+v, want kind bug with every other field preserved", chain[1])
	}
	si, err := ReadSession(dir)
	if err != nil || si.SessionID != "b" || si.Kind != "bug" || si.Stage != StageEntry {
		t.Errorf("legacy session file = %+v err=%v, want it kept in step with the stamp", si, err)
	}
}

// SetHeadKind on an empty or missing chain is a no-op, like every other
// best-effort lineage writer.
func TestSetHeadKindNoChainIsNoop(t *testing.T) {
	dir := t.TempDir()
	SetHeadKind(dir, "bug")
	if chain := ReadSessionChain(dir); chain != nil {
		t.Errorf("chain = %+v, want none created", chain)
	}
}

func TestResolvedKind(t *testing.T) {
	dir := t.TempDir()
	if got := ResolvedKind(dir); got != "bug" {
		t.Errorf("ResolvedKind(empty) = %q, want the bug fallback", got)
	}
	AppendSessionNode(dir, SessionNode{ID: "e", Stage: StageEntry})
	if got := ResolvedKind(dir); got != "bug" {
		t.Errorf("ResolvedKind(unstamped entry) = %q, want the bug fallback", got)
	}
	SetHeadKind(dir, "feature")
	if got := ResolvedKind(dir); got != "feature" {
		t.Errorf("ResolvedKind = %q, want the stamped feature", got)
	}
}
