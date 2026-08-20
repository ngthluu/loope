package engine

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ngthluu/loope/worker/shared"
)

const readySentinel = "PIPELINE_READY"

const specReadySentinel = "SPEC_READY:"

// nothingToAnswerSentinel is printed by the answerer (PO proxy) when the
// architect's message asked no question and requested no approval — a status
// update. The loop then nudges the architect toward a terminal sentinel
// instead of relaying an empty approval.
const nothingToAnswerSentinel = "QA_NOTHING_TO_ANSWER"

// RunFeaturePipeline drives three sessions: an architect brainstorm session
// (session A) that scores its confidence up front and, above the threshold,
// works with a sonnet product-owner proxy to a committed spec (SPEC_READY); a
// fresh plan session (session B) that turns the spec into a committed plan
// (PIPELINE_READY); and a fresh execute session (session C) that implements it.
// Below the confidence threshold it returns *lowConfidenceError without
// designing anything.
// Immediately after the spec is committed — before plan and execute — the
// non-blocking UAT step publishes a human-verifiable checklist onto the issue.
func RunFeaturePipeline(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, issueContent, persona string, uat *UAT, gh shared.CodeHost, wt shared.Workspace, branch, title string, n int) error {
	start := time.Now()
	// Snapshot before the call: the content is already fetched, and a session
	// killed at any point still leaves the diff base the next resume needs.
	c.RecordSnapshot(issueContent)
	res, err := architectCall(ctx, c, cfg, wtPath, "brainstorm-0", brainstormPrompt(issueContent, cfg.ConfidenceThreshold), "")
	if err != nil {
		return err
	}

	if err := confidenceGate(cfg, res.Result); err != nil {
		return err
	}
	return brainstormLoop(ctx, c, cfg, wtPath, issueContent, persona, uat, res.SessionID, res.Result, start, gh, wt, branch, title, n)
}

// ResumeFeaturePipeline re-enters the feature pipeline at the chain node a
// re-entry resolved (resumePoint): resume the node's own session with the
// trigger prompt, or — for a pending node (no session id) — re-run its stage
// fresh from the recorded artifact. An unrecognized stage, or a pending node
// whose artifact is gone, falls back to a fully fresh RunFeaturePipeline as a
// safety net.
func ResumeFeaturePipeline(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, issueContent, persona string, uat *UAT, node shared.SessionNode, prompt string, gh shared.CodeHost, wt shared.Workspace, branch, title string, n int) error {
	start := time.Now()
	fresh := func() error {
		return RunFeaturePipeline(ctx, c, cfg, wtPath, issueContent, persona, uat, gh, wt, branch, title, n)
	}
	switch node.Stage {
	case shared.StagePlan:
		if node.ID == "" {
			// Pending node: the plan session never started (or a legacy
			// pre-call checkpoint). Re-run plan fresh from the committed spec.
			if spec, ok := checkpointArtifact(wtPath, node.Artifact); ok {
				return runPlanThenExecute(ctx, c, cfg, wtPath, node.Artifact, planPrompt(spec), "", start, gh, wt, branch, n)
			}
			return fresh()
		}
		return runPlanThenExecute(ctx, c, cfg, wtPath, node.Artifact, prompt, node.ID, start, gh, wt, branch, n)
	case shared.StageExecute:
		if node.ID == "" {
			// Pending node: execute never started. Re-run it fresh on the
			// committed plan — the executing-plans skill picks up from
			// whatever steps are already done.
			if plan, ok := checkpointArtifact(wtPath, node.Artifact); ok {
				if err := executePlan(ctx, c, cfg, wtPath, plan); err != nil {
					return err
				}
				if perr := wt.Push(ctx, wtPath, branch); perr != nil {
					log.Printf("issue #%d: execute-stage push failed: %v", n, perr)
				}
				return nil
			}
			return fresh()
		}
		if err := resumeExecutePlan(ctx, c, cfg, wtPath, prompt, node.ID, node.Artifact); err != nil {
			return err
		}
		// Execute complete (spec §1): push once, best-effort — ship's own push at
		// the end of a successful run is the backstop, so a failure here is
		// logged and swallowed rather than failing an otherwise-successful resume.
		if perr := wt.Push(ctx, wtPath, branch); perr != nil {
			log.Printf("issue #%d: execute-stage push failed: %v", n, perr)
		}
		return nil
	case shared.StageBrainstorm:
		if node.ID == "" {
			return fresh()
		}
		c.RecordSnapshot(issueContent)
		// The trigger alone teaches a resumed session nothing about the
		// sentinel contract; restate it so a session re-entered past its spec
		// can still terminate (SPEC_READY / PIPELINE_ALREADY_DONE) instead of
		// narrating status until the round cap parks it.
		res, err := architectCall(ctx, c, cfg, wtPath, "brainstorm-resume", brainstormResumePrompt(prompt), node.ID)
		if err != nil {
			return err
		}
		if err := confidenceGate(cfg, res.Result); err != nil {
			return err
		}
		return brainstormLoop(ctx, c, cfg, wtPath, issueContent, persona, uat, res.SessionID, res.Result, start, gh, wt, branch, title, n)
	default:
		return fresh()
	}
}

// confidenceGate judges an architect turn's confidence score, shared by the
// fresh brainstorm-0 call and a resumed brainstorm-resume turn so a resumed
// session is held to the same threshold as a fresh one. A threshold <= 0
// disables it, and an unparseable score fails open.
func confidenceGate(cfg *shared.Config, output string) error {
	if cfg.ConfidenceThreshold > 0 {
		if score, ok := parseConfidence(output); ok && score < cfg.ConfidenceThreshold {
			return &lowConfidenceError{score: score, feedback: sanitizeFeedback(output)}
		}
	}
	return nil
}

// architectCall runs one architect-model turn: the brainstorm-0/brainstorm-N
// call when resume is "", or a --resume turn (round call or resumed re-entry)
// when it isn't. Every architect turn is a primary brainstorm-stage session,
// so the checkpoint is baked in here.
func architectCall(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, label, prompt, resume string) (*shared.ClaudeResult, error) {
	return c.Call(ctx, shared.ClaudeCall{
		Dir: wtPath, Label: label, Prompt: prompt, Resume: resume,
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
		Checkpoint:      &shared.CallCheckpoint{Kind: "feature", Stage: shared.StageBrainstorm},
	})
}

// brainstormLoop is the architect Q&A round loop shared by a fresh brainstorm-0
// call and a resumed brainstorm session: sessionID/output are the id and result
// text of whichever call preceded it (brainstorm-0, or the resume turn).
func brainstormLoop(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, issueContent, persona string, uat *UAT, sessionID, output string, start time.Time, gh shared.CodeHost, wt shared.Workspace, branch, title string, n int) error {
	for round := 1; ; round++ {
		// The architect signals a committed spec: hand off to the fresh plan
		// session, then execute. If it claims a spec but none is on disk, fall
		// through and keep prodding (mirrors the plan-file behavior).
		if rel, ok := parseSpecReady(output); ok {
			if specPath, ok := resolveSpec(wtPath, rel, start); ok {
				// Spec committed: append a PENDING plan node BEFORE the plan
				// call starts, so a crash anywhere in the handoff (push, PR,
				// comment, plan spawn) resumes as a fresh plan run on this
				// spec instead of re-entering this loop.
				specRel := relToWorktree(wtPath, specPath)
				c.CheckpointStage("feature", shared.StagePlan, specRel)
				// The UAT session runs alongside plan/execute — it only reads
				// the committed spec, so nothing downstream waits on it. The
				// wait runs on every exit path (including a failed plan): the
				// session must not outlive the pipeline, whose worktree and
				// context the caller tears down on return.
				wait := uat.StartFeature(ctx, c, cfg, wtPath, specPath)
				defer wait()
				// Spec complete (spec §1): push, open (or recover) the PR,
				// comment the URL, and record it — before plan/execute run at
				// all. Best-effort: pushSpecPR logs and swallows its own
				// failures rather than turning a completed spec into an error.
				pushSpecPR(ctx, gh, wt, wtPath, branch, title, n, c.LogDir())
				return runPlanThenExecute(ctx, c, cfg, wtPath, specRel, planPrompt(specPath), "", start, gh, wt, branch, n)
			}
		}

		var reply string
		donePushback := false
		if reason, ok := parseAlreadyDone(output); ok {
			// Architect claims already implemented — the answerer (PO proxy)
			// must confirm before we close. This confirmation is terminal, not a
			// bounded round.
			confirm, err := c.Call(ctx, shared.ClaudeCall{
				Dir: wtPath, Label: fmt.Sprintf("done-confirm-%d", round),
				Prompt:          doneConfirmPrompt(issueContent, persona, reason),
				Model:           cfg.Models.Answerer,
				SkipPermissions: true,
			})
			if err != nil {
				return err
			}
			if strings.Contains(confirm.Result, doneConfirmSentinel) {
				return &alreadyDoneError{reason: reason}
			}
			reply = confirm.Result // objection; hand it back to the architect
			donePushback = true
		}

		// Sending a reply to the architect is a bounded Q&A round.
		if round > cfg.MaxQARounds {
			return fmt.Errorf("feature pipeline: exceeded %d Q&A rounds without a completed spec", cfg.MaxQARounds)
		}
		if !donePushback {
			ans, err := c.Call(ctx, shared.ClaudeCall{
				Dir: wtPath, Label: fmt.Sprintf("answer-%d", round),
				Prompt:          answererPrompt(issueContent, persona, output),
				Model:           cfg.Models.Answerer,
				SkipPermissions: true,
			})
			if err != nil {
				return err
			}
			reply = ans.Result
			if strings.Contains(reply, nothingToAnswerSentinel) {
				// A status update, not a question: an "Approved" relay would
				// only invite the next status update (issue-5 burned every
				// round this way). Push the architect toward a terminal
				// sentinel instead.
				reply = qaNudgePrompt()
			}
		}

		res, err := architectCall(ctx, c, cfg, wtPath, fmt.Sprintf("brainstorm-%d", round), reply, sessionID)
		if err != nil {
			return err
		}
		output = res.Result
	}
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
	if !strings.Contains(res.Result, readySentinel) {
		return fmt.Errorf("feature pipeline: plan session did not signal %s", readySentinel)
	}
	plan, ok := findPlanFile(wtPath, start)
	if !ok {
		return fmt.Errorf("feature pipeline: plan session signaled %s but wrote no plan file", readySentinel)
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

// parseSpecReady extracts the spec path following specReadySentinel. ok is
// false only when no line leads with the sentinel (line-anchored via
// parseSentinelLine, same as the other sentinels); an empty path still
// counts.
func parseSpecReady(s string) (string, bool) {
	return parseSentinelLine(s, specReadySentinel)
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

// resolveSpec turns the architect's SPEC_READY path into an existing spec file.
// An explicit path (absolute, or relative to wtPath) is preferred; otherwise it
// falls back to the newest spec under a specs/ dir modified after since.
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

func brainstormPrompt(issue string, threshold int) string {
	d := promptData()
	d["Issue"] = issue
	d["Threshold"] = threshold
	return mustRender("brainstorm.md.tmpl", d)
}

// brainstormResumePrompt wraps a resumed brainstorm session's trigger prompt
// with a restatement of the sentinel contract. A bare trigger taught the
// resumed architect nothing about SPEC_READY/PIPELINE_ALREADY_DONE, so a
// session re-entered past its spec reported completion in prose the loop
// could not parse and burned every Q&A round (issue-5 incident).
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

const doneConfirmSentinel = "DONE_CONFIRMED"

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
