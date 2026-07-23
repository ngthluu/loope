package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type TriageDecision struct {
	IssueNumber int    `json:"issueNumber"`
	Kind        string `json:"kind"`
	Reason      string `json:"reason"`
}

func Triage(ctx context.Context, c *Claude, mc ModelConfig, repoPath string, issues []Issue) (*TriageDecision, error) {
	list, err := json.MarshalIndent(issues, "", "  ")
	if err != nil {
		return nil, err
	}
	prompt := triagePrompt(string(list))

	res, err := c.Call(ctx, ClaudeCall{Dir: repoPath, Label: "triage", Prompt: prompt, Model: mc})
	if err != nil {
		return nil, err
	}
	dec, err := parseTriage(res.Result)
	if err != nil {
		return nil, fmt.Errorf("triage: %w (output: %s)", err, tail(res.Result, 300))
	}
	if dec.Kind != "bug" && dec.Kind != "feature" {
		return nil, fmt.Errorf("triage: invalid kind %q", dec.Kind)
	}
	for _, is := range issues {
		if is.Number == dec.IssueNumber {
			return dec, nil
		}
	}
	return nil, fmt.Errorf("triage: picked unknown issue #%d", dec.IssueNumber)
}

func parseTriage(s string) (*TriageDecision, error) {
	start := strings.Index(s, "{")
	if start < 0 {
		return nil, errors.New("no JSON object in output")
	}
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	var d TriageDecision
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("parse decision: %w", err)
	}
	return &d, nil
}

func triagePrompt(list string) string {
	return fmt.Sprintf(`You are a triage agent for an automated development pipeline.

Open eligible issues:
%s

Decide from the issue text alone — do NOT read the repository. Pick the single
best issue to work on next and classify it:
- "bug": a small, well-scoped defect that can be fixed by reproducing and debugging
- "feature": anything that needs design work (new functionality, refactors, unclear scope)

Respond with ONLY a JSON object, no other text:
{"issueNumber": <int>, "kind": "bug" or "feature", "reason": "<one sentence>"}`, list)
}
