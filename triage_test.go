package main

import (
	"context"
	"strings"
	"testing"
)

var triageIssues = []Issue{{Number: 5, Title: "Fix crash"}, {Number: 8, Title: "Add export"}}

func TestTriagePicksIssue(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		`{"issueNumber": 5, "kind": "bug", "reason": "small crash fix"}`, "s1")}}}
	c := &Claude{runner: f}
	dec, err := Triage(context.Background(), c, ModelConfig{Model: "sonnet"}, "/clone", triageIssues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 5 || dec.Kind != "bug" {
		t.Errorf("dec = %+v", dec)
	}
	call := f.calls[0]
	if call.dir != "/clone" {
		t.Errorf("dir = %q, want /clone", call.dir)
	}
	prompt := call.stdin
	if !strings.Contains(prompt, "Fix crash") || !strings.Contains(prompt, "Add export") {
		t.Errorf("prompt missing issues: %s", prompt)
	}
}

func TestTriageParsesJSONEmbeddedInText(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		"Here is my decision:\n{\"issueNumber\": 8, \"kind\": \"feature\", \"reason\": \"needs design\"}\nDone.", "s1")}}}
	c := &Claude{runner: f}
	dec, err := Triage(context.Background(), c, ModelConfig{}, "/clone", triageIssues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 8 || dec.Kind != "feature" {
		t.Errorf("dec = %+v", dec)
	}
}

func TestTriageRejectsBadKind(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		`{"issueNumber": 5, "kind": "chore", "reason": "x"}`, "s1")}}}
	c := &Claude{runner: f}
	if _, err := Triage(context.Background(), c, ModelConfig{}, "/clone", triageIssues); err == nil {
		t.Error("want error for kind=chore")
	}
}

func TestTriageRejectsUnknownIssue(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		`{"issueNumber": 99, "kind": "bug", "reason": "x"}`, "s1")}}}
	c := &Claude{runner: f}
	if _, err := Triage(context.Background(), c, ModelConfig{}, "/clone", triageIssues); err == nil {
		t.Error("want error for unknown issue number")
	}
}

func TestTriageParsesFirstBalancedObjectWithTrailingBrace(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		`{"issueNumber": 5, "kind": "bug", "reason": "small crash fix"} trailing text with a stray } here`, "s1")}}}
	c := &Claude{runner: f}
	dec, err := Triage(context.Background(), c, ModelConfig{}, "/clone", triageIssues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 5 || dec.Kind != "bug" {
		t.Errorf("dec = %+v", dec)
	}
}

func TestTriageParsesReasonContainingBraces(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		`{"issueNumber": 8, "kind": "feature", "reason": "looks like a {template} with {braces}"}`, "s1")}}}
	c := &Claude{runner: f}
	dec, err := Triage(context.Background(), c, ModelConfig{}, "/clone", triageIssues)
	if err != nil {
		t.Fatal(err)
	}
	if dec.IssueNumber != 8 || dec.Kind != "feature" || dec.Reason != "looks like a {template} with {braces}" {
		t.Errorf("dec = %+v", dec)
	}
}

func TestTriageRejectsNoJSON(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON("I cannot decide.", "s1")}}}
	c := &Claude{runner: f}
	if _, err := Triage(context.Background(), c, ModelConfig{}, "/clone", triageIssues); err == nil {
		t.Error("want error when no JSON present")
	}
}

func TestTriageRejectsDoneKind(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		`{"issueNumber": 8, "kind": "done", "reason": "already implemented"}`, "s1")}}}
	c := &Claude{runner: f}
	if _, err := Triage(context.Background(), c, ModelConfig{}, "/clone", triageIssues); err == nil {
		t.Error("want error: triage no longer classifies 'done'")
	}
}

func TestTriagePromptIsCodeBlind(t *testing.T) {
	f := &fakeRunner{queue: []rresp{{stdout: claudeJSON(
		`{"issueNumber": 5, "kind": "bug", "reason": "x"}`, "s1")}}}
	c := &Claude{runner: f}
	if _, err := Triage(context.Background(), c, ModelConfig{}, "/clone", triageIssues); err != nil {
		t.Fatal(err)
	}
	prompt := f.calls[0].stdin
	if strings.Contains(prompt, "reading the relevant code") || strings.Contains(prompt, `"done"`) {
		t.Errorf("triage prompt must not mention reading code or the done kind:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"bug"`) || !strings.Contains(prompt, `"feature"`) {
		t.Errorf("triage prompt must still offer bug and feature:\n%s", prompt)
	}
}
