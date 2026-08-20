package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

var triageIssues = []shared.Issue{{Number: 5, Title: "Fix crash"}, {Number: 8, Title: "Add export"}}

func TestTriagePicksIssue(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		`{"issueNumber": 5, "kind": "bug", "reason": "small crash fix"}`, "s1")}}}
	c := infra.NewClaude(f, "", "")
	dec, err := Triage(context.Background(), c, shared.ModelConfig{Model: "sonnet"}, "/clone", triageIssues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 5 || dec.Kind != "bug" {
		t.Errorf("dec = %+v", dec)
	}
	call := f.Calls[0]
	if call.Dir != "/clone" {
		t.Errorf("dir = %q, want /clone", call.Dir)
	}
	prompt := call.Stdin
	if !strings.Contains(prompt, "Fix crash") || !strings.Contains(prompt, "Add export") {
		t.Errorf("prompt missing issues: %s", prompt)
	}
}

func TestTriageParsesJSONEmbeddedInText(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		"Here is my decision:\n{\"issueNumber\": 8, \"kind\": \"feature\", \"reason\": \"needs design\"}\nDone.", "s1")}}}
	c := infra.NewClaude(f, "", "")
	dec, err := Triage(context.Background(), c, shared.ModelConfig{}, "/clone", triageIssues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 8 || dec.Kind != "feature" {
		t.Errorf("dec = %+v", dec)
	}
}

func TestTriageRejectsBadKind(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		`{"issueNumber": 5, "kind": "chore", "reason": "x"}`, "s1")}}}
	c := infra.NewClaude(f, "", "")
	if _, err := Triage(context.Background(), c, shared.ModelConfig{}, "/clone", triageIssues); err == nil {
		t.Error("want error for kind=chore")
	}
}

func TestTriageRejectsUnknownIssue(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		`{"issueNumber": 99, "kind": "bug", "reason": "x"}`, "s1")}}}
	c := infra.NewClaude(f, "", "")
	if _, err := Triage(context.Background(), c, shared.ModelConfig{}, "/clone", triageIssues); err == nil {
		t.Error("want error for unknown issue number")
	}
}

func TestTriageParsesFirstBalancedObjectWithTrailingBrace(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		`{"issueNumber": 5, "kind": "bug", "reason": "small crash fix"} trailing text with a stray } here`, "s1")}}}
	c := infra.NewClaude(f, "", "")
	dec, err := Triage(context.Background(), c, shared.ModelConfig{}, "/clone", triageIssues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 5 || dec.Kind != "bug" {
		t.Errorf("dec = %+v", dec)
	}
}

func TestTriageParsesReasonContainingBraces(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		`{"issueNumber": 8, "kind": "feature", "reason": "looks like a {template} with {braces}"}`, "s1")}}}
	c := infra.NewClaude(f, "", "")
	dec, err := Triage(context.Background(), c, shared.ModelConfig{}, "/clone", triageIssues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 8 || dec.Kind != "feature" || dec.Reason != "looks like a {template} with {braces}" {
		t.Errorf("dec = %+v", dec)
	}
}

func TestTriageRejectsNoJSON(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON("I cannot decide.", "s1")}}}
	c := infra.NewClaude(f, "", "")
	if _, err := Triage(context.Background(), c, shared.ModelConfig{}, "/clone", triageIssues); err == nil {
		t.Error("want error when no JSON present")
	}
}

func TestTriageRejectsDoneKind(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		`{"issueNumber": 8, "kind": "done", "reason": "already implemented"}`, "s1")}}}
	c := infra.NewClaude(f, "", "")
	if _, err := Triage(context.Background(), c, shared.ModelConfig{}, "/clone", triageIssues); err == nil {
		t.Error("want error: triage no longer classifies 'done'")
	}
}

func TestTriagePromptIsCodeBlind(t *testing.T) {
	f := &testkit.FakeRunner{Queue: []testkit.RResp{{Stdout: testkit.ClaudeJSON(
		`{"issueNumber": 5, "kind": "bug", "reason": "x"}`, "s1")}}}
	c := infra.NewClaude(f, "", "")
	if _, err := Triage(context.Background(), c, shared.ModelConfig{}, "/clone", triageIssues); err != nil {
		t.Fatal(err)
	}
	prompt := f.Calls[0].Stdin
	if strings.Contains(prompt, "reading the relevant code") || strings.Contains(prompt, `"done"`) {
		t.Errorf("triage prompt must not mention reading code or the done kind:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"bug"`) || !strings.Contains(prompt, `"feature"`) {
		t.Errorf("triage prompt must still offer bug and feature:\n%s", prompt)
	}
}
