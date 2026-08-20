package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ngthluu/loope/worker/shared"
)

// planStatusReady is the status value a plan session's structured output must
// carry for the stage to count as complete. The plan stage does NOT use a
// prose sentinel (the old PIPELINE_READY): every plan call passes
// planResultSchema as --json-schema, so the CLI itself forces the final output
// into this shape — on fresh runs and, crucially, on resumed turns, where a
// prompt-taught sentinel was sometimes forgotten.
const planStatusReady = "ready"

// planResultSchema is the --json-schema for every plan-stage call. The field
// descriptions carry the contract so even a resumed session triggered with a
// bare "continue" knows what to report.
const planResultSchema = `{
  "type": "object",
  "properties": {
    "status": {
      "type": "string",
      "enum": ["ready", "incomplete"],
      "description": "\"ready\" once the implementation plan file is written and committed into this branch; \"incomplete\" otherwise."
    },
    "plan_path": {
      "type": "string",
      "description": "Path of the committed plan file relative to the repository root. Required when status is \"ready\"."
    },
    "detail": {
      "type": "string",
      "description": "Brief explanation when status is not \"ready\"."
    }
  },
  "required": ["status"]
}`

// planResult mirrors planResultSchema.
type planResult struct {
	Status   string `json:"status"`
	PlanPath string `json:"plan_path"`
	Detail   string `json:"detail"`
}

// answererResultSchema is the --json-schema for the answerer (PO proxy) call.
// has_answer false marks a status update with nothing to answer — the loop
// then nudges the architect toward a terminal outcome instead of relaying an
// empty approval.
const answererResultSchema = `{
  "type": "object",
  "properties": {
    "has_answer": {
      "type": "boolean",
      "description": "true when the architect's message asked a question or requested approval and answer responds to it; false when the message asked for no answer and no approval (a status or progress update)."
    },
    "answer": {
      "type": "string",
      "description": "The reply to relay to the architect. Required when has_answer is true."
    }
  },
  "required": ["has_answer"]
}`

// answererResult mirrors answererResultSchema.
type answererResult struct {
	HasAnswer bool   `json:"has_answer"`
	Answer    string `json:"answer"`
}

// This file holds the feature outcome's plan/execute machinery, plus the
// product-owner-proxy prompt builders (answerer, done-confirm, qa-nudge) and
// the confidence gate that BOTH routes share via the entry loop. The pipeline
// entry itself — the merged entry session and its Q&A round loop — lives in
// pipeline_entry.go.

// confidenceGate judges an entry turn's confidence score, shared by the fresh
// entry-0 call and a resumed re-entry turn so a resumed session is held to the
// same threshold as a fresh one. A threshold <= 0 disables it, and an absent
// score fails open (as the old unparseable-sentinel case did).
func confidenceGate(cfg *shared.Config, er entryResult) error {
	if cfg.ConfidenceThreshold > 0 && er.Confidence != nil && *er.Confidence < cfg.ConfidenceThreshold {
		return &lowConfidenceError{score: *er.Confidence, feedback: er.Detail}
	}
	return nil
}

// runPlanThenExecute runs the plan session (session B) that turns the approved
// spec into a committed plan, then executes it (session C). Both are fresh
// sessions on a first pass — the plan session must not carry brainstorm
// context — but the SAME entry point serves a resumed plan session too: resume
// is "" on a fresh call (prompt is planPrompt(specPath)) and the chain node's
// own plan-session id when re-entering (prompt is the trigger prompt instead).
// specRel is the worktree-relative spec the stage builds from; it rides on the
// chain node so a dead resume can fall back to a fresh run on the spec.
func runPlanThenExecute(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, specRel, prompt, resume string, start time.Time, gh shared.CodeHost, wt shared.Workspace, branch string, n int) error {
	res, err := c.Call(ctx, shared.ClaudeCall{
		Dir: wtPath, Label: "plan", Prompt: prompt, Resume: resume,
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
		JSONSchema:      planResultSchema,
		Checkpoint:      &shared.CallCheckpoint{Kind: "feature", Stage: shared.StagePlan, Artifact: specRel},
	})
	if err != nil {
		// A resume that died before its session ever started (no salvaged id)
		// means the session is gone from this machine — with the spec still on
		// disk, re-run the stage fresh on it instead of failing. A mid-run
		// failure (res carries the salvaged id) propagates: that session is
		// checkpointed and the next human-triggered re-entry resumes it.
		if resume != "" && res == nil {
			if spec, ok := checkpointArtifact(wtPath, specRel); ok {
				log.Printf("issue #%d: plan session %s not resumable, re-running fresh from %s", n, resume, specRel)
				return runPlanThenExecute(ctx, c, cfg, wtPath, specRel, planPrompt(spec), "", start, gh, wt, branch, n)
			}
		}
		return err
	}
	var pr planResult
	if uerr := json.Unmarshal(res.StructuredOutput, &pr); uerr != nil {
		return fmt.Errorf("feature pipeline: plan session returned no structured status: %v", uerr)
	}
	if pr.Status != planStatusReady {
		return fmt.Errorf("feature pipeline: plan session ended %q: %s", pr.Status, pr.Detail)
	}
	plan, ok := resolvePlan(wtPath, pr.PlanPath, start)
	if !ok {
		return fmt.Errorf("feature pipeline: plan session reported ready but no plan file found (plan_path=%q)", pr.PlanPath)
	}
	// Plan committed: append a PENDING execute node BEFORE the execute call
	// starts, so a crash in the handoff (push, comment, execute spawn) resumes
	// as a fresh execute run on this plan instead of re-entering the completed
	// plan session.
	c.CheckpointStage("feature", shared.StageExecute, relToWorktree(wtPath, plan))
	// Plan complete (spec §1): push, then post the fixed "Updated plan: ..."
	// comment naming the plan file — before execute runs at all.
	pushPlanUpdate(ctx, gh, wt, wtPath, branch, n, plan)
	if err := executePlan(ctx, c, cfg, wtPath, plan); err != nil {
		return err
	}
	// Execute complete (spec §1): push once, best-effort — ship's own push at
	// the end of a successful run is the backstop.
	if perr := wt.Push(ctx, wtPath, branch); perr != nil {
		log.Printf("issue #%d: execute-stage push failed: %v", n, perr)
	}
	return nil
}

// pushSpecPR runs the spec-complete push point (spec §1): push the branch,
// open (or recover) its PR, comment the URL, and record it for the
// dashboard. Best-effort — decision 5: any failure here is logged and
// swallowed, never turning a completed spec stage into a pipeline error.
// Worktree.Push and GitHub.CreatePR are both idempotent (see worktree.go,
// github.go), so ship's own push/CreatePR at the very end of a successful
// run — and any later push/PR-create from the plan or execute stage — safely
// repeats whatever this call already did.
func pushSpecPR(ctx context.Context, gh shared.CodeHost, wt shared.Workspace, wtPath, branch, title string, n int, logDir string) {
	if err := wt.Push(ctx, wtPath, branch); err != nil {
		log.Printf("issue #%d: spec-stage push failed: %v", n, err)
		return
	}
	url, err := gh.CreatePR(ctx, branch, prTitle(title, n), prBody(n, "feature"))
	if err != nil {
		log.Printf("issue #%d: spec-stage PR create failed: %v", n, err)
		return
	}
	if err := gh.Comment(ctx, n, prComment(url)); err != nil {
		log.Printf("issue #%d: spec-stage PR comment failed: %v", n, err)
	}
	shared.RecordPR(logDir, url)
}

// pushPlanUpdate runs the plan-complete push point (spec §1): push the
// branch, then post the fixed "Updated plan: ..." comment naming the plan
// file relative to the worktree root. Best-effort, same as pushSpecPR — no
// PR is created here, the spec stage already created (or ship's own backstop
// is about to create) the one PR this branch ever gets.
func pushPlanUpdate(ctx context.Context, gh shared.CodeHost, wt shared.Workspace, wtPath, branch string, n int, planPath string) {
	if err := wt.Push(ctx, wtPath, branch); err != nil {
		log.Printf("issue #%d: plan-stage push failed: %v", n, err)
		return
	}
	rel, err := filepath.Rel(wtPath, planPath)
	if err != nil {
		rel = planPath
	}
	if err := gh.Comment(ctx, n, planComment(rel)); err != nil {
		log.Printf("issue #%d: plan-stage comment failed: %v", n, err)
	}
}

// executePlan runs the execute session (session C) fresh, immediately after
// the plan file is written.
func executePlan(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, planPath string) error {
	_, err := c.Call(ctx, shared.ClaudeCall{
		Dir: wtPath, Label: "execute", Prompt: executePrompt(planPath),
		Model:           cfg.Models.ExecuteConfig(),
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
		Checkpoint:      &shared.CallCheckpoint{Kind: "feature", Stage: shared.StageExecute, Artifact: relToWorktree(wtPath, planPath)},
	})
	return err
}

// resumeExecutePlan re-enters a persisted execute session (session C) at
// exactly the chain node's session, with --resume and the trigger prompt —
// used by ResumeFeaturePipeline. planRel is the node's artifact: a resume
// that died before its session started falls back to a fresh execute run on
// the plan, mirroring runPlanThenExecute's dead-resume fallback.
func resumeExecutePlan(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, prompt, resume, planRel string) error {
	res, err := c.Call(ctx, shared.ClaudeCall{
		Dir: wtPath, Label: "execute", Prompt: prompt, Resume: resume,
		Model:           cfg.Models.ExecuteConfig(),
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
		Checkpoint:      &shared.CallCheckpoint{Kind: "feature", Stage: shared.StageExecute, Artifact: planRel},
	})
	if err != nil && res == nil {
		if plan, ok := checkpointArtifact(wtPath, planRel); ok {
			return executePlan(ctx, c, cfg, wtPath, plan)
		}
	}
	return err
}

// relToWorktree returns p relative to wtPath, or p unchanged when Rel fails —
// the checkpoint format (SessionInfo.SpecPath/PlanPath) stores paths relative
// to the worktree so a moved workspace doesn't invalidate them.
func relToWorktree(wtPath, p string) string {
	if rel, err := filepath.Rel(wtPath, p); err == nil {
		return rel
	}
	return p
}

// checkpointArtifact resolves a checkpointed worktree-relative artifact path
// (SessionInfo.SpecPath/PlanPath) to an existing file. Deliberately no
// newest-file fallback: a checkpoint names ONE artifact, and guessing another
// spec/plan in the tree could resume the wrong issue's work.
func checkpointArtifact(wtPath, rel string) (string, bool) {
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(wtPath, rel)
	}
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p, true
	}
	return "", false
}

// findSpecFile returns the newest *.md under any specs/ directory in root
// modified after since (mirrors findPlanFile).
func findSpecFile(root string, since time.Time) (string, bool) {
	var newest string
	var newestMod time.Time
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") || !strings.Contains(filepath.ToSlash(path), "/specs/") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(since) && info.ModTime().After(newestMod) {
			newest, newestMod = path, info.ModTime()
		}
		return nil
	})
	return newest, newest != ""
}

// resolveSpec turns the architect's reported spec_path into an existing spec
// file. An explicit path (absolute, or relative to wtPath) is preferred;
// otherwise it falls back to the newest spec under a specs/ dir modified
// after since.
func resolveSpec(wtPath, rel string, since time.Time) (string, bool) {
	if rel != "" {
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(wtPath, rel)
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
	}
	return findSpecFile(wtPath, since)
}

// resolvePlan turns the plan session's reported plan_path into an existing
// plan file. The explicit path (absolute, or relative to wtPath) is preferred —
// it survives a resume, where a plan committed before the resume predates
// since — otherwise it falls back to the newest plan under a plans/ dir
// modified after since (mirrors resolveSpec).
func resolvePlan(wtPath, rel string, since time.Time) (string, bool) {
	if rel != "" {
		if p, ok := checkpointArtifact(wtPath, rel); ok {
			return p, true
		}
	}
	return findPlanFile(wtPath, since)
}

// findPlanFile returns the newest *.md under any plans/ directory in root
// modified after since.
func findPlanFile(root string, since time.Time) (string, bool) {
	var newest string
	var newestMod time.Time
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") || !strings.Contains(filepath.ToSlash(path), "/plans/") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(since) && info.ModTime().After(newestMod) {
			newest, newestMod = path, info.ModTime()
		}
		return nil
	})
	return newest, newest != ""
}

func readPersona(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// brainstormResumePrompt wraps a resumed LEGACY brainstorm session's trigger
// prompt with a restatement of the old feature-route contract (spec_ready /
// already_done — such a session was never taught fix_committed). Kept only for
// chains checkpointed at StageBrainstorm before the merged entry stage
// existed; fresh sessions use entryResumePrompt. The entry schema is enforced
// on the resumed turn either way.
func brainstormResumePrompt(trigger string) string {
	d := promptData()
	d["Trigger"] = trigger
	return mustRender("brainstorm-resume.md.tmpl", d)
}

// qaNudgePrompt is the reply sent to the architect when the answerer signaled
// there was nothing to answer: it pushes toward a terminal sentinel instead of
// relaying an empty approval that would only invite another status update.
func qaNudgePrompt() string {
	return mustRender("qa-nudge.md.tmpl", promptData())
}

func answererPrompt(issue, persona, architectMsg string) string {
	d := promptData()
	d["Issue"] = issue
	d["Persona"] = persona
	d["ArchitectMsg"] = architectMsg
	return mustRender("answerer.md.tmpl", d)
}

// doneConfirmSchema is the --json-schema for the done-confirm (PO proxy) call
// that judges an architect's already-implemented claim.
const doneConfirmSchema = `{
  "type": "object",
  "properties": {
    "confirmed": {
      "type": "boolean",
      "description": "true when you agree the issue's work is already fully implemented."
    },
    "objection": {
      "type": "string",
      "description": "One concise sentence telling the architect what is still missing or must be designed. Required when confirmed is false."
    }
  },
  "required": ["confirmed"]
}`

// doneConfirmResult mirrors doneConfirmSchema.
type doneConfirmResult struct {
	Confirmed bool   `json:"confirmed"`
	Objection string `json:"objection"`
}

func doneConfirmPrompt(issue, persona, reason string) string {
	d := promptData()
	d["Issue"] = issue
	d["Persona"] = persona
	d["Reason"] = reason
	return mustRender("done-confirm.md.tmpl", d)
}

func planPrompt(specPath string) string {
	d := promptData()
	d["SpecPath"] = specPath
	return mustRender("plan.md.tmpl", d)
}

func executePrompt(planPath string) string {
	d := promptData()
	d["PlanPath"] = planPath
	return mustRender("execute.md.tmpl", d)
}
