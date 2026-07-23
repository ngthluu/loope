package main

import (
	"strings"
	"testing"
)

// Representative data for every renderable template in the embedded FS. A new
// prompt file or {{define}} block with no entry here fails TestEveryTemplateRenders —
// which is the point: it catches a prompt that was added but never wired up.
var promptTestData = map[string]map[string]any{
	"brainstorm.md.tmpl":   {"Issue": "I", "Threshold": 70},
	"answerer.md.tmpl":     {"Issue": "I", "Persona": "P", "ArchitectMsg": "A"},
	"done-confirm.md.tmpl": {"Issue": "I", "Persona": "P", "Reason": "R"},
	"plan.md.tmpl":         {"SpecPath": "docs/spec.md"},
	"execute.md.tmpl":      {"PlanPath": "docs/plan.md"},
	"debug.md.tmpl":        {"Issue": "I"},
	"rework.md.tmpl":       {},
	"triage.md.tmpl":       {"List": "[]"},
	"pickup":               {"Kind": "feature", "Branch": "b"},
	"already-done":         {"Reason": "R"},
	"needs-info":           {"Score": 1, "Label": "l", "Feedback": "F"},
	"park":                 {"Number": 1, "Guidance": "G", "Error": "E"},
	"pr-comment":           {"URL": "u"},
	"pr-title":             {"Title": "T", "Number": 1},
	"pr-body":              {"Number": 1, "Kind": "bug"},
	"guidance-usage-limit": {},
	"guidance-budget":      {},
	"guidance-interrupted": {},
	"guidance-network":     {},
}

// skipTemplates are the two names in the set that are not prompts: the root
// template ParseFS was seeded with, and the container file whose own body is
// just the whitespace between its {{define}} blocks.
var skipTemplates = map[string]bool{"prompts": true, "comments.md.tmpl": true}

func TestEveryTemplateRenders(t *testing.T) {
	for _, tmpl := range prompts.Templates() {
		name := tmpl.Name()
		if skipTemplates[name] {
			continue
		}
		data, ok := promptTestData[name]
		if !ok {
			t.Errorf("template %q has no entry in promptTestData — add one (a prompt with no test data is a prompt nobody renders)", name)
			continue
		}
		d := promptData()
		for k, v := range data {
			d[k] = v
		}
		got := mustRender(name, d)
		if strings.TrimSpace(got) == "" {
			t.Errorf("template %q rendered empty", name)
		}
		if strings.Contains(got, "<no value>") {
			t.Errorf("template %q rendered a <no value> placeholder:\n%s", name, got)
		}
		if strings.HasSuffix(got, "\n") {
			t.Errorf("template %q kept its trailing newline; mustRender must trim it", name)
		}
	}
}

// Every .md.tmpl file on disk must have made it into the binary.
func TestEveryPromptFileIsEmbedded(t *testing.T) {
	entries, err := promptFS.ReadDir("ai/prompts")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("ai/prompts embedded empty")
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md.tmpl") {
			t.Errorf("unexpected file in ai/prompts: %s (only .md.tmpl files are parsed)", e.Name())
		}
	}
}

// Sentinels come from the Go constants, never from literal text in a template.
func TestNoSentinelIsHardcodedInATemplate(t *testing.T) {
	entries, err := promptFS.ReadDir("ai/prompts")
	if err != nil {
		t.Fatal(err)
	}
	sentinels := []string{confidenceSentinel, specReadySentinel, readySentinel, alreadyDoneSentinel, doneConfirmSentinel}
	for _, e := range entries {
		b, err := promptFS.ReadFile("ai/prompts/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sentinels {
			if strings.Contains(string(b), s) {
				t.Errorf("%s hardcodes the sentinel %q — inject it via promptData() instead", e.Name(), s)
			}
		}
	}
}
