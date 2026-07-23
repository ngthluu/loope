package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const readySentinel = "PIPELINE_READY"

const specReadySentinel = "SPEC_READY:"

// RunFeaturePipeline drives three sessions: an architect brainstorm session
// (session A) that scores its confidence up front and, above the threshold,
// works with a sonnet product-owner proxy to a committed spec (SPEC_READY); a
// fresh plan session (session B) that turns the spec into a committed plan
// (PIPELINE_READY); and a fresh execute session (session C) that implements it.
// Below the confidence threshold it returns *lowConfidenceError without
// designing anything.
func RunFeaturePipeline(ctx context.Context, c *Claude, cfg *Config, wtPath, issueContent, persona string) error {
	start := time.Now()
	architect := func(label, prompt, resume string) (*ClaudeResult, error) {
		return c.Call(ctx, ClaudeCall{
			Dir: wtPath, Label: label, Prompt: prompt, Resume: resume,
			Model:           cfg.Models.Architect,
			SkipPermissions: true,
			DisallowedTools: []string{"AskUserQuestion"},
		})
	}

	res, err := architect("brainstorm-0", brainstormPrompt(issueContent, cfg.ConfidenceThreshold), "")
	// Record before the error check: an errored call (e.g. a 429 session limit)
	// still returns a session id, and preserving it lets `loop -rework` resume.
	if res != nil {
		c.RecordSession(res.SessionID, "feature")
	}
	if err != nil {
		return err
	}
	session := res.SessionID
	output := res.Result

	// Upfront confidence gate: judged once, on the first brainstorm turn only.
	// A threshold <= 0 disables it. Fail open on an unparseable score.
	if cfg.ConfidenceThreshold > 0 {
		if score, ok := parseConfidence(output); ok && score < cfg.ConfidenceThreshold {
			return &lowConfidenceError{score: score, feedback: stripConfidenceLine(output)}
		}
	}

	for round := 1; ; round++ {
		// The architect signals a committed spec: hand off to the fresh plan
		// session, then execute. If it claims a spec but none is on disk, fall
		// through and keep prodding (mirrors the plan-file behavior).
		if rel, ok := parseSpecReady(output); ok {
			if specPath, ok := resolveSpec(wtPath, rel, start); ok {
				return runPlanThenExecute(ctx, c, cfg, wtPath, specPath, start)
			}
		}

		var reply string
		donePushback := false
		if reason, ok := parseAlreadyDone(output); ok {
			// Architect claims already implemented — the answerer (PO proxy)
			// must confirm before we close. This confirmation is terminal, not a
			// bounded round.
			confirm, err := c.Call(ctx, ClaudeCall{
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
			ans, err := c.Call(ctx, ClaudeCall{
				Dir: wtPath, Label: fmt.Sprintf("answer-%d", round),
				Prompt:          answererPrompt(issueContent, persona, output),
				Model:           cfg.Models.Answerer,
				SkipPermissions: true,
			})
			if err != nil {
				return err
			}
			reply = ans.Result
		}

		res, err := architect(fmt.Sprintf("brainstorm-%d", round), reply, session)
		if res != nil {
			c.RecordSession(res.SessionID, "feature")
		}
		if err != nil {
			return err
		}
		output = res.Result
	}
}

// runPlanThenExecute runs the fresh plan session (session B) that turns the
// approved spec into a committed plan, then executes it (session C). Both are
// fresh sessions — the plan session must not carry brainstorm context.
func runPlanThenExecute(ctx context.Context, c *Claude, cfg *Config, wtPath, specPath string, start time.Time) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "plan", Prompt: planPrompt(specPath),
		Model:           cfg.Models.Architect,
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if res != nil {
		c.RecordSession(res.SessionID, "feature")
	}
	if err != nil {
		return err
	}
	if !strings.Contains(res.Result, readySentinel) {
		return fmt.Errorf("feature pipeline: plan session did not signal %s", readySentinel)
	}
	plan, ok := findPlanFile(wtPath, start)
	if !ok {
		return fmt.Errorf("feature pipeline: plan session signaled %s but wrote no plan file", readySentinel)
	}
	return executePlan(ctx, c, cfg, wtPath, plan)
}

func executePlan(ctx context.Context, c *Claude, cfg *Config, wtPath, planPath string) error {
	res, err := c.Call(ctx, ClaudeCall{
		Dir: wtPath, Label: "execute", Prompt: executePrompt(planPath),
		Model:           cfg.Models.executeConfig(),
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if res != nil {
		c.RecordSession(res.SessionID, "feature")
	}
	if err != nil {
		return err
	}
	return nil
}

// parseSpecReady extracts the spec path following specReadySentinel. ok is
// false only when the sentinel is absent; an empty path still counts.
func parseSpecReady(s string) (string, bool) {
	i := strings.Index(s, specReadySentinel)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(specReadySentinel):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.TrimSpace(rest), true
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
