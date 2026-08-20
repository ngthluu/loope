package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFeaturePipelineRecordsLineageChain pins the happy-path lineage:
// brainstorm -> plan -> execute, each session one chain node linked to the
// session that spawned it, with pending nodes marking each stage handoff.
func TestFeaturePipelineRecordsLineageChain(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case strings.Contains(c.stdin, "brainstorming"):
			writeSpecFile(t, wt)
			return claudeJSON("SPEC_READY: docs/superpowers/specs/2026-07-13-thing-design.md", "arch-sess"), "", nil
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-sess"), "", nil
		default:
			return claudeJSON("executed", "exec-sess"), "", nil
		}
	}}
	c := &Claude{runner: f, logDir: logDir}
	if err := RunFeaturePipeline(context.Background(), c, featureConfig(), wt, "ISSUE", "", nil, testGH(), testWT(), "ai/issue-1", "T", 1); err != nil {
		t.Fatal(err)
	}
	var real []SessionNode
	for _, n := range readSessionChain(logDir) {
		if n.ID != "" {
			real = append(real, n)
		}
	}
	if len(real) != 3 {
		t.Fatalf("real chain nodes = %+v, want brainstorm/plan/execute", real)
	}
	if real[0].ID != "arch-sess" || real[0].Stage != stageBrainstorm || real[0].Parent != "" {
		t.Errorf("node[0] = %+v, want arch-sess/brainstorm with no parent", real[0])
	}
	if real[1].ID != "plan-sess" || real[1].Stage != stagePlan || real[1].Parent != "arch-sess" {
		t.Errorf("node[1] = %+v, want plan-sess spawned by arch-sess", real[1])
	}
	if real[2].ID != "exec-sess" || real[2].Stage != stageExecute || real[2].Parent != "plan-sess" {
		t.Errorf("node[2] = %+v, want exec-sess spawned by plan-sess", real[2])
	}
	head, _ := headSession(logDir)
	if head.ID != "exec-sess" {
		t.Errorf("head = %+v, want the execute session as the resume point", head)
	}
}

// TestResumeFeaturePipelinePlanStageDeadResumeFallsBackToArtifact: the chain
// head names the plan session itself, but the local CLI can no longer resume
// it (the resume dies before any session id streams). With the spec artifact
// still on disk, the stage re-runs FRESH from the spec instead of failing.
func TestResumeFeaturePipelinePlanStageDeadResumeFallsBackToArtifact(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	writeSpecFile(t, wt)
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		switch {
		case argAfter(c.args, "--resume") == "plan-sess":
			return "", "No conversation found with session ID plan-sess", errors.New("exit 1")
		case strings.Contains(c.stdin, "writing-plans"):
			_ = os.MkdirAll(filepath.Join(wt, "plans"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "plans", "plan.md"), []byte("# plan"), 0o644)
			return claudeJSON("PIPELINE_READY", "plan-2"), "", nil
		default:
			return claudeJSON("executed", "exec-1"), "", nil
		}
	}}
	c := &Claude{runner: f, logDir: logDir}
	node := SessionNode{ID: "plan-sess", Kind: "feature", Stage: stagePlan,
		Artifact: "docs/superpowers/specs/2026-07-13-thing-design.md"}
	if err := ResumeFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", "", nil, node, "continue", testGH(), testWT(), "ai/issue-1", "T", 1); err != nil {
		t.Fatalf("dead resume with a live artifact must fall back fresh, got %v", err)
	}
	fresh := false
	for _, call := range f.calls {
		if strings.Contains(call.stdin, "2026-07-13-thing-design.md") && argAfter(call.args, "--resume") == "" {
			fresh = true
		}
	}
	if !fresh {
		t.Error("want a fresh plan call on the checkpointed spec after the dead resume")
	}
}

// A dead resume mid-run is different: once a session id streamed, the failure
// is the session's own (usage limit, crash mid-work) — re-running fresh would
// burn the stage's progress, so the error propagates and the issue parks with
// the SAME session still checkpointed for the next human-triggered resume.
func TestResumeFeaturePipelinePlanStageMidRunFailureParks(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	writeSpecFile(t, wt)
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if argAfter(c.args, "--resume") == "plan-sess" {
			partial := `{"type":"system","subtype":"init","session_id":"plan-sess"}` + "\n"
			return partial, "usage limit", errors.New("signal: killed")
		}
		t.Errorf("unexpected extra call after a mid-run failure: %+v", c)
		return "", "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	node := SessionNode{ID: "plan-sess", Kind: "feature", Stage: stagePlan,
		Artifact: "docs/superpowers/specs/2026-07-13-thing-design.md"}
	err := ResumeFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", "", nil, node, "continue", testGH(), testWT(), "ai/issue-1", "T", 1)
	if err == nil {
		t.Fatal("mid-run failure must propagate so the issue parks")
	}
	head, ok := headSession(logDir)
	if !ok || head.ID != "plan-sess" || head.Stage != stagePlan {
		t.Errorf("head = %+v ok=%v, want plan-sess still checkpointed for the next resume", head, ok)
	}
}

// TestResumeFeaturePipelineExecuteStageDeadResumeFallsBackToArtifact mirrors
// the plan fallback for execute: dead resume + live plan artifact = fresh
// execute run on the plan.
func TestResumeFeaturePipelineExecuteStageDeadResumeFallsBackToArtifact(t *testing.T) {
	logDir := t.TempDir()
	wt := t.TempDir()
	planPath := writePlanFile(t, wt)
	rel, _ := filepath.Rel(wt, planPath)
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if argAfter(c.args, "--resume") == "exec-sess" {
			return "", "No conversation found", errors.New("exit 1")
		}
		return claudeJSON("executed", "exec-2"), "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	node := SessionNode{ID: "exec-sess", Kind: "feature", Stage: stageExecute, Artifact: rel}
	if err := ResumeFeaturePipeline(context.Background(), c, featureConfig(), wt, "the issue", "", nil, node, "continue", testGH(), testWT(), "ai/issue-1", "T", 1); err != nil {
		t.Fatalf("dead execute resume with a live plan must fall back fresh, got %v", err)
	}
	fresh := false
	for _, call := range f.calls {
		if strings.Contains(call.stdin, rel) && argAfter(call.args, "--resume") == "" {
			fresh = true
		}
	}
	if !fresh {
		t.Error("want a fresh execute call on the checkpointed plan after the dead resume")
	}
}

// TestResumeBugPipelineResumesChainNode: the bug pipeline's single debug
// session resumes from the chain node.
func TestResumeBugPipelineResumesChainNode(t *testing.T) {
	logDir := t.TempDir()
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if argAfter(c.args, "--resume") == "debug-sess" {
			return claudeJSON("fixed", "debug-sess-2"), "", nil
		}
		t.Errorf("unexpected call: %+v", c)
		return "", "", nil
	}}
	c := &Claude{runner: f, logDir: logDir}
	node := SessionNode{ID: "debug-sess", Kind: "bug", Stage: stageDebug}
	if err := ResumeBugPipeline(context.Background(), c, featureConfig(), "/wt", "the issue", "main", nil, nil, node, "continue"); err != nil {
		t.Fatal(err)
	}
	head, ok := headSession(logDir)
	if !ok || head.Stage != stageDebug {
		t.Errorf("head = %+v ok=%v, want the resumed debug session checkpointed", head, ok)
	}
}
