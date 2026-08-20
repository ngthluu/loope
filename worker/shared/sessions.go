package shared

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the session-lineage domain: every primary Claude session an
// issue spawns is one node of an append-only chain (a linked list on disk),
// recorded the moment the session starts. All resume triggers — rework label
// removed, needs-info answered, dashboard Continue, orphan sweep — resolve
// the SAME way: read the chain head and continue inside that session (or, for
// a head with no id, re-run its stage fresh from the recorded artifact).

// SessionNode is one primary Claude session in an issue's lineage. Parent
// links it to the session that spawned it (brainstorm -> plan -> execute ->
// codereview rounds), so the chain reads as the pipeline's history.
//
// An empty ID is a stage that was checkpointed before its session started
// (or a legacy pre-call checkpoint): there is nothing to --resume, so the
// stage re-runs fresh from Artifact.
type SessionNode struct {
	ID       string    `json:"id,omitempty"`
	Parent   string    `json:"parent,omitempty"`
	Kind     string    `json:"kind"`               // pipeline kind: "feature" | "bug"
	Stage    string    `json:"stage"`              // StageBrainstorm | StagePlan | StageExecute | StageDebug | StageCodeReview
	Artifact string    `json:"artifact,omitempty"` // worktree-relative spec/plan the stage builds from
	At       time.Time `json:"at,omitempty"`
}

// chainFile holds the issue's session chain, one JSON node per line.
// Append-only: a crash can at worst lose the line being written, never the
// history before it.
const ChainFile = "sessions.jsonl"

// appendSessionNode appends one node to <logDir>/sessions.jsonl. Best-effort,
// like the other log-writers: a no-op on an empty logDir, errors swallowed —
// a failed lineage write must never derail the call it is only mirroring.
func AppendSessionNode(logDir string, n SessionNode) {
	if logDir == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	b, err := json.Marshal(n)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(logDir, ChainFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// readSessionChain returns the chain in append order. Corrupt lines (a torn
// write from a crash) are skipped, and a missing file is an empty chain.
func ReadSessionChain(logDir string) []SessionNode {
	data, err := os.ReadFile(filepath.Join(logDir, ChainFile))
	if err != nil {
		return nil
	}
	var chain []SessionNode
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var n SessionNode
		if json.Unmarshal([]byte(line), &n) != nil {
			continue
		}
		chain = append(chain, n)
	}
	return chain
}

// headSession returns the newest node — the resume point — or ok=false on an
// empty or missing chain.
func HeadSession(logDir string) (SessionNode, bool) {
	chain := ReadSessionChain(logDir)
	if len(chain) == 0 {
		return SessionNode{}, false
	}
	return chain[len(chain)-1], true
}

// resumePoint resolves where a re-entry continues: the head of the session
// chain when one exists, else the legacy single-record session file mapped
// into node form. ok=false means there is nothing to resume (first attempt).
//
// Legacy mapping: a plain record's id belongs to its recorded stage, so it
// maps to a resumable node. A legacy PRE-CALL checkpoint (StagePlan+SpecPath
// or StageExecute+PlanPath) carries the PREVIOUS stage's session id, so the
// node keeps the stage and artifact but drops the id — resuming it would
// re-enter a completed session (the issue-5 incident).
func ResumePoint(logDir string) (SessionNode, bool) {
	if head, ok := HeadSession(logDir); ok {
		return head, true
	}
	si, ok := LoadResumableSession(logDir)
	if !ok {
		return SessionNode{}, false
	}
	switch {
	case si.Stage == StagePlan && si.SpecPath != "":
		return SessionNode{Kind: si.Kind, Stage: StagePlan, Artifact: si.SpecPath}, true
	case si.Stage == StageExecute && si.PlanPath != "":
		return SessionNode{Kind: si.Kind, Stage: StageExecute, Artifact: si.PlanPath}, true
	}
	return SessionNode{ID: si.SessionID, Kind: si.Kind, Stage: si.Stage}, true
}

// LastRealSessionID walks the chain backwards for the newest node with a real
// session id — the parent a freshly spawned session links to. Pending nodes
// (no id) are skipped: they mark intent, not a session.
func LastRealSessionID(logDir string) string {
	chain := ReadSessionChain(logDir)
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].ID != "" {
			return chain[i].ID
		}
	}
	return ""
}
