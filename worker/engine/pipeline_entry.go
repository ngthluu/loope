package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ngthluu/loope/worker/shared"
)

// This file is the unified pipeline: one merged entry session investigates the
// issue and decides its own route — fix a defect directly (bug outcome,
// fix_committed) or design first (feature outcome, spec_ready into plan and
// execute). The kind is DERIVED from that outcome and stamped onto the session
// chain (Agent.SetKind), not decided up front: the pre-pipeline triage
// classifier this replaced kept routing design-shaped "fixes" into the one-shot
// debug session.

// Entry outcome values — the enum an entry turn's structured output must pick
// from. Terminal outcomes derive the pipeline kind (fix → bug, spec →
// feature); "question" keeps the Q&A loop going.
const (
	entryOutcomeFix      = "fix_committed"
	entryOutcomeSpec     = "spec_ready"
	entryOutcomeDone     = "already_done"
	entryOutcomeQuestion = "question"
)

// entryResultSchema is the --json-schema for every entry-stage turn (fresh,
// round, and resumed — including legacy brainstorm/debug resumes). The CLI
// enforces the shape on every turn, so a resumed session cannot "forget" the
// contract the way prompt-taught sentinels were forgotten (the issue-5
// incident); the field descriptions carry the semantics for turns whose
// prompt is a bare trigger.
const entryResultSchema = `{
  "type": "object",
  "properties": {
    "outcome": {
      "type": "string",
      "enum": ["fix_committed", "spec_ready", "already_done", "question"],
      "description": "fix_committed: a fix for this issue is committed on this branch. spec_ready: the spec document for this issue is written and committed on this branch. already_done: the issue's work is already fully implemented in this codebase. question: you need an answer or approval from the product owner before reaching a terminal outcome (also used for a status update)."
    },
    "confidence": {
      "type": "integer",
      "minimum": 0,
      "maximum": 100,
      "description": "How confidently the issue can be handled as written, 0-100. Report it on the opening turn of the session; omit on later turns."
    },
    "spec_path": {
      "type": "string",
      "description": "Committed spec file path relative to the repository root. Required when outcome is spec_ready."
    },
    "detail": {
      "type": "string",
      "description": "fix_committed: one-sentence summary of the fix. spec_ready: one-sentence summary of the spec. already_done: one-sentence reason. question: the full question or message for the product owner."
    }
  },
  "required": ["outcome", "detail"]
}`

// entryResult mirrors entryResultSchema. Confidence is a pointer so an absent
// score is distinguishable from 0 — the gate fails open on absence, exactly
// as the old parser did on an unparseable sentinel.
type entryResult struct {
	Outcome    string `json:"outcome"`
	Confidence *int   `json:"confidence"`
	SpecPath   string `json:"spec_path"`
	Detail     string `json:"detail"`
}

// parseEntry decodes an entry turn's schema-validated structured output. An
// undecodable result is off-contract (only possible when the CLI failed to
// enforce the schema) and surfaces as a hard error rather than a guess.
func parseEntry(res *shared.ClaudeResult) (entryResult, error) {
	var er entryResult
	if err := json.Unmarshal(res.StructuredOutput, &er); err != nil {
		return er, fmt.Errorf("entry session returned no structured outcome: %v", err)
	}
	return er, nil
}

// RunPipeline drives the merged entry session for a fresh issue: score
// confidence, then loop the entry session against the product-owner proxy
// until it terminates with a fix (afterFix's gates), a committed spec (plan
// and execute follow, unchanged), or an already-done claim.
func RunPipeline(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, issueContent, persona, base string, uat *UAT, gh shared.CodeHost, wt shared.Workspace, branch, title string, n int) error {
	start := time.Now()
	res, er, err := openEntry(ctx, c, cfg, issueContent, entryCallSpec(cfg, wtPath, "entry-0", entryPrompt(issueContent, cfg.ConfidenceThreshold), ""))
	if err != nil {
		return err
	}
	return entryLoop(ctx, c, cfg, wtPath, issueContent, persona, base, uat, res.SessionID, er, start, gh, wt, branch, title, n)
}

// openEntry runs the turn that opens (or re-opens) an entry-stage session and
// applies the shared prefix every opener needs: snapshot the issue content,
// make the call, decode the outcome and judge its confidence. Shared by the
// fresh entry-0 call and every resumed opener (entry, legacy brainstorm,
// legacy debug) so a resumed session is held to the same gate as a fresh one.
// The snapshot is taken BEFORE the call: the content is already fetched, and a
// session killed at any point still leaves the diff base the next resume needs.
func openEntry(ctx context.Context, c shared.Agent, cfg *shared.Config, issueContent string, call shared.ClaudeCall) (*shared.ClaudeResult, entryResult, error) {
	c.RecordSnapshot(issueContent)
	res, err := c.Call(ctx, call)
	if err != nil {
		return nil, entryResult{}, err
	}
	er, err := parseEntry(res)
	if err != nil {
		return nil, er, err
	}
	if err := confidenceGate(cfg, er); err != nil {
		return nil, er, err
	}
	return res, er, nil
}

// ResumePipeline re-enters the pipeline at the chain node a re-entry resolved
// (shared.ResumePoint): every trigger — rework label removed, needs-info
// answered, dashboard Continue, orphan sweep — converges here. New chains head
// at StageEntry/StagePlan/StageExecute; chains checkpointed before the merged
// entry existed head at the legacy StageDebug/StageBrainstorm and resume on
// their ORIGINAL route (per the project's continue-not-reset rule — the
// resumed session was never taught the merged contract). A node that can't be
// mapped falls back to a fully fresh RunPipeline as a safety net.
func ResumePipeline(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, issueContent, persona, base string, uat *UAT, node shared.SessionNode, prompt string, gh shared.CodeHost, wt shared.Workspace, branch, title string, n int) error {
	// Zero `since`: a resumed session's spec/plan was typically committed
	// BEFORE this resume, so the newest-artifact fallback must not filter on
	// the resume's start time the way a fresh run filters on its own.
	var since time.Time
	fresh := func() error {
		return RunPipeline(ctx, c, cfg, wtPath, issueContent, persona, base, uat, gh, wt, branch, title, n)
	}
	switch node.Stage {
	case shared.StageEntry:
		if node.ID == "" {
			return fresh()
		}
		// The schema enforces the outcome contract on the resumed turn; the
		// resume prompt still restates it in prose so the session reaches the
		// right terminal outcome by intent, not by forced guess (issue-5
		// incident).
		res, er, err := openEntry(ctx, c, cfg, issueContent, entryCallSpec(cfg, wtPath, "entry-resume", entryResumePrompt(prompt), node.ID))
		if err != nil {
			return err
		}
		return entryLoop(ctx, c, cfg, wtPath, issueContent, persona, base, uat, res.SessionID, er, since, gh, wt, branch, title, n)
	case shared.StagePlan:
		if node.ID == "" {
			// Pending node: the plan session never started (or a legacy
			// pre-call checkpoint). Re-run plan fresh from the committed spec.
			if spec, ok := checkpointArtifact(wtPath, node.Artifact); ok {
				return runPlanThenExecute(ctx, c, cfg, wtPath, node.Artifact, planPrompt(spec), "", since, gh, wt, branch, n)
			}
			return fresh()
		}
		return runPlanThenExecute(ctx, c, cfg, wtPath, node.Artifact, prompt, node.ID, since, gh, wt, branch, n)
	case shared.StageExecute:
		if node.ID == "" {
			// Pending node: execute never started. Re-run it fresh on the
			// committed plan — the executing-plans skill picks up from
			// whatever steps are already done.
			if plan, ok := checkpointArtifact(wtPath, node.Artifact); ok {
				return executeAndPush(ctx, c, cfg, wtPath, plan, wt, branch, n)
			}
			return fresh()
		}
		if err := resumeExecutePlan(ctx, c, cfg, wtPath, prompt, node.ID, node.Artifact); err != nil {
			return err
		}
		pushAfterExecute(ctx, wt, wtPath, branch, n)
		return nil
	case shared.StageBrainstorm:
		// LEGACY: a design session from before the merged entry. Resume it on
		// the shared entry-outcomes contract (brainstormResumePrompt includes the
		// same outcome block as entry-resume) and continue through entryLoop.
		if node.ID == "" {
			return fresh()
		}
		res, er, err := openEntry(ctx, c, cfg, issueContent, shared.ClaudeCall{
			Dir: wtPath, Label: "brainstorm-resume", Prompt: brainstormResumePrompt(prompt), Resume: node.ID,
			Model:           cfg.Models.Architect,
			SkipPermissions: true,
			DisallowedTools: []string{"AskUserQuestion"},
			JSONSchema:      entryResultSchema,
			Checkpoint:      &shared.CallCheckpoint{Kind: shared.KindFeature, Stage: shared.StageBrainstorm},
		})
		if err != nil {
			return err
		}
		return entryLoop(ctx, c, cfg, wtPath, issueContent, persona, base, uat, res.SessionID, er, since, gh, wt, branch, title, n)
	default:
		// LEGACY: chains whose kind was decided up front by triage. A "bug"
		// chain resumes its debug session whatever stage string it carries
		// (the old dispatch keyed on kind alone, and debug is the only stage a
		// bug pipeline ever checkpointed); anything else falls back fresh. The
		// resumed turn continues through the shared entry loop, same as the
		// legacy brainstorm resume: only a fix_committed outcome reaches
		// afterFix's ship gates — a question or spec outcome on a branch that
		// already carries commits must not be shipped as if it were the fix.
		if node.Kind == shared.KindBug && node.ID != "" {
			res, er, err := openEntry(ctx, c, cfg, issueContent, shared.ClaudeCall{
				Dir: wtPath, Label: "debug-resume", Prompt: prompt, Resume: node.ID,
				Model:           cfg.Models.Architect,
				SkipPermissions: true,
				DisallowedTools: []string{"AskUserQuestion"},
				JSONSchema:      entryResultSchema,
				Checkpoint:      &shared.CallCheckpoint{Kind: shared.KindBug, Stage: shared.StageDebug},
			})
			if err != nil {
				return err
			}
			return entryLoop(ctx, c, cfg, wtPath, issueContent, persona, base, uat, res.SessionID, er, since, gh, wt, branch, title, n)
		}
		return fresh()
	}
}

// entryLoop is the entry session's Q&A round loop, shared by a fresh entry-0
// call, a resumed entry session, and a resumed legacy brainstorm session:
// sessionID/er are the id and structured outcome of whichever call preceded
// it. Each round it dispatches on the terminal outcomes, stamping the derived
// kind the moment one resolves, and otherwise relays a product-owner-proxy
// reply.
func entryLoop(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, issueContent, persona, base string, uat *UAT, sessionID string, er entryResult, start time.Time, gh shared.CodeHost, wt shared.Workspace, branch, title string, n int) error {
	specMisses := 0 // consecutive spec_ready outcomes with no spec file on disk
	for round := 1; ; round++ {
		// The session reports a committed spec: the feature outcome. Hand off to
		// the fresh plan session, then execute. If it claims a spec but none is
		// on disk, the first miss falls through to one more prod (a session
		// that wrote the file to an unexpected path gets one chance to point
		// at it); a second consecutive miss fails fast like the plan stage does
		// instead of burning every Q&A round behind a misleading park cause.
		if er.Outcome == entryOutcomeSpec {
			specPath, ok := resolveSpec(wtPath, er.SpecPath, start)
			if !ok {
				if specMisses++; specMisses >= 2 {
					return fmt.Errorf("pipeline: entry session reported spec_ready but no spec file found (spec_path=%q)", er.SpecPath)
				}
			} else {
				c.SetKind(shared.KindFeature)
				// Spec committed: append a PENDING plan node BEFORE the plan
				// call starts, so a crash anywhere in the handoff (push, PR,
				// comment, plan spawn) resumes as a fresh plan run on this
				// spec instead of re-entering this loop.
				specRel := relToWorktree(wtPath, specPath)
				c.CheckpointStage(shared.KindFeature, shared.StagePlan, specRel)
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
		} else {
			specMisses = 0
		}
		// The session reports a committed fix: the bug outcome. afterFix's gates
		// (confidence, already-done, zero-commit needs-info fallback, UAT) are
		// the same ones the old debug pipeline ran.
		if er.Outcome == entryOutcomeFix {
			c.SetKind(shared.KindBug)
			return afterFix(ctx, c, cfg, wtPath, issueContent, base, uat, wt, er)
		}

		var reply string
		donePushback := false
		if er.Outcome == entryOutcomeDone {
			// The session claims already implemented — the answerer (PO proxy)
			// must confirm before we close. This confirmation is terminal, not a
			// bounded round.
			confirm, err := c.Call(ctx, shared.ClaudeCall{
				Dir: wtPath, Label: fmt.Sprintf("done-confirm-%d", round),
				Prompt:          doneConfirmPrompt(issueContent, persona, er.Detail),
				Model:           cfg.Models.Answerer,
				SkipPermissions: true,
				DisallowedTools: poProxyDisallowedTools,
				JSONSchema:      doneConfirmSchema,
			})
			if err != nil {
				return err
			}
			var dc doneConfirmResult
			if derr := json.Unmarshal(confirm.StructuredOutput, &dc); derr != nil {
				return fmt.Errorf("done-confirm returned no structured verdict: %v", derr)
			}
			if dc.Confirmed {
				return &alreadyDoneError{reason: er.Detail}
			}
			reply = dc.Objection // objection; hand it back to the session
			donePushback = true
		}

		// Sending a reply to the session is a bounded Q&A round.
		if round > cfg.MaxQARounds {
			return fmt.Errorf("pipeline: exceeded %d Q&A rounds without a committed fix or spec", cfg.MaxQARounds)
		}
		if !donePushback {
			ans, err := c.Call(ctx, shared.ClaudeCall{
				Dir: wtPath, Label: fmt.Sprintf("answer-%d", round),
				Prompt:          answererPrompt(issueContent, persona, er.Detail),
				Model:           cfg.Models.Answerer,
				SkipPermissions: true,
				DisallowedTools: poProxyDisallowedTools,
				JSONSchema:      answererResultSchema,
			})
			if err != nil {
				return err
			}
			var ar answererResult
			if aerr := json.Unmarshal(ans.StructuredOutput, &ar); aerr != nil {
				return fmt.Errorf("answerer returned no structured reply: %v", aerr)
			}
			reply = ar.Answer
			if !ar.HasAnswer || strings.TrimSpace(reply) == "" {
				// A status update, not a question: an "Approved" relay would
				// only invite the next status update (issue-5 burned every
				// round this way). Push the session toward a terminal
				// outcome instead.
				reply = qaNudgePrompt()
			}
		}

		res, err := entryCall(ctx, c, cfg, wtPath, fmt.Sprintf("entry-%d", round), reply, sessionID)
		if err != nil {
			return err
		}
		if er, err = parseEntry(res); err != nil {
			return err
		}
	}
}

// afterFix runs the bug outcome's gates — confidence, already-done, the
// zero-commit needs-info fallback, and (if a fix was actually produced) the
// non-blocking UAT step. Shared by the merged entry's fix-committed branch and
// the legacy debug-session resume; the body is the old debug pipeline's
// afterDebug, unchanged.
func afterFix(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, issueContent, base string, uat *UAT, wt shared.Workspace, er entryResult) error {
	// Confidence gate, shared with the feature route: same threshold, field
	// and terminal outcome. It runs before the already-done check on purpose —
	// a session too unsure to fix the bug must not get to close the issue as
	// already implemented instead. An absent score fails open so a session
	// that omitted it but fixed the bug still ships.
	if err := confidenceGate(cfg, er); err != nil {
		return err
	}
	if er.Outcome == entryOutcomeDone {
		return &alreadyDoneError{reason: er.Detail}
	}
	// A session can claim a fix (or a resumed legacy debug session can end)
	// without one actually existing — e.g. it investigates and stops to ask a
	// clarifying question instead of committing (observed on issues #70 and
	// #83). Check the worktree directly: zero commits ahead of base means no
	// fix exists — letting that fall through to ship() parks the ticket as
	// "produced no commits" with the session's questions buried in the log.
	// Escalate to needs-info instead, with the session's output as the public
	// comment, so a human sees the questions and their answer resumes this
	// session. Fail open on a CommitCount error, same as the confidence gate.
	if wt != nil {
		if n, err := wt.CommitCount(ctx, wtPath, base); err == nil && n == 0 {
			return &lowConfidenceError{score: noConfidenceScore, feedback: er.Detail}
		}
	}
	if uat == nil || uat.Target == nil {
		return nil
	}
	// Only this outcome produced a fix — neither the needs-info nor the
	// already-done return above reaches here, so neither publishes a checklist.
	uat.RunBug(ctx, c, cfg, wtPath, issueContent, base)
	return nil
}

// poProxyDisallowedTools is the tool denylist for the product-owner-proxy
// calls (answerer, done-confirm): they run in the worktree with permissions
// skipped, and a judge must read, never edit, the work it judges (mirrors the
// UAT session's denylist).
var poProxyDisallowedTools = []string{"AskUserQuestion", "Write", "Edit", "NotebookEdit"}

// entryCall runs one entry-stage turn: the entry-0/entry-N call when resume is
// "", or a --resume turn (round call or resumed re-entry) when it isn't.
func entryCall(ctx context.Context, c shared.Agent, cfg *shared.Config, wtPath, label, prompt, resume string) (*shared.ClaudeResult, error) {
	return c.Call(ctx, entryCallSpec(cfg, wtPath, label, prompt, resume))
}

// entryCallSpec builds the ClaudeCall for an entry-stage turn. The kind is
// checkpointed EMPTY on purpose: at call time nothing has decided bug vs
// feature yet, and no other value would be accurate. entryLoop stamps the
// resolved kind onto the chain head (SetKind) the moment the outcome is
// parsed; resume dispatch never reads an entry node's kind, only its stage.
func entryCallSpec(cfg *shared.Config, wtPath, label, prompt, resume string) shared.ClaudeCall {
	return shared.ClaudeCall{
		Dir: wtPath, Label: label, Prompt: prompt, Resume: resume,
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
		JSONSchema:      entryResultSchema,
		Checkpoint:      &shared.CallCheckpoint{Kind: "", Stage: shared.StageEntry},
	}
}

func entryPrompt(issue string, threshold int) string {
	d := promptData()
	d["Issue"] = issue
	d["Threshold"] = threshold
	return mustRender("entry.md.tmpl", d)
}

// entryResumePrompt wraps a resumed entry session's trigger prompt with a
// restatement of the outcome contract. The schema already forces the shape of
// the final output, but the prose keeps the session aiming at the right
// terminal outcome instead of defaulting to another question (the issue-5
// incident, on the old brainstorm resume).
func entryResumePrompt(trigger string) string {
	d := promptData()
	d["Trigger"] = trigger
	return mustRender("entry-resume.md.tmpl", d)
}
