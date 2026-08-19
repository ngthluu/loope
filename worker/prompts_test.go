package main

import (
	"io/fs"
	"path/filepath"
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
	"debug.md.tmpl":        {"Issue": "I", "Threshold": 70},
	"triage.md.tmpl":       {"List": "[]"},
	"pickup":               {"Kind": "feature", "Branch": "b"},
	"already-done":         {"Reason": "R"},
	"needs-info":           {"Score": 1, "Label": "l", "Feedback": "F"},
	"park":                 {"Label": "ai-rework", "Guidance": "G", "Error": "E"},
	"pr-comment":           {"URL": "u"},
	"plan-comment":         {"Path": "docs/superpowers/plans/2026-plan.md"},
	"pr-title":             {"Title": "T", "Number": 1},
	"pr-body":              {"Number": 1, "Kind": "bug"},
	"guidance-usage-limit": {},
	"guidance-budget":      {},
	"guidance-network":     {},
	"ask-format":           {},
	"uat-format":           {"UATCoverage": "C"},
	"uat-section":          {"Checklist": "- [ ] C"},
	"uat-feature.md.tmpl":  {"SpecPath": "docs/spec.md", "UATCoverage": "C"},
	"uat-bug.md.tmpl":      {"Issue": "I", "Base": "main", "UATCoverage": "C"},
	"codereview.md.tmpl":   {"Round": 1, "Rounds": 2, "Base": "main"},
	"codereview-comment":   {"Round": 1, "Rounds": 2, "Status": "fixed", "Summary": "S"},
}

// skipTemplates are the names in the set that are not prompts: the root
// template ParseFS was seeded with, and the container files whose own bodies
// are just the whitespace between their {{define}} blocks.
var skipTemplates = map[string]bool{
	"prompts":            true,
	"comments.md.tmpl":   true,
	"ask-format.md.tmpl": true,
	"uat-format.md.tmpl": true,
}

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

// The reply-format instruction lives in exactly one place. This asserts the
// block exists and still carries the rules that make the needs-info comment
// readable — a silent deletion of one bullet would otherwise pass every other
// test in this file.
func TestAskFormatBlockCarriesItsRules(t *testing.T) {
	got := mustRender("ask-format", promptData())
	for _, want := range []string{
		"numbered list of questions",
		"Write each question in short, plain sentences",
		"At most 5 questions",
		"MERGE related gaps",
		"Under 200 words",
		"no preamble",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ask-format block is missing %q:\n%s", want, got)
		}
	}
}

// Both routes must ask in the same shape, from the same source. This is what
// catches an edit made to one prompt that should have been an edit to the
// shared block — and, via the threshold=0 cases, that the instruction stays
// inside the gate's guard.
func TestBothRoutesShareTheAskFormatBlock(t *testing.T) {
	block := mustRender("ask-format", promptData())
	if !strings.Contains(brainstormPrompt("I", 70), block) {
		t.Error("brainstormPrompt(threshold=70) does not contain the ask-format block")
	}
	if !strings.Contains(bugPrompt("I", 70), block) {
		t.Error("bugPrompt(threshold=70) does not contain the ask-format block")
	}
	if strings.Contains(brainstormPrompt("I", 0), block) {
		t.Error("brainstormPrompt(threshold=0) contains the ask-format block; it must stay inside the threshold guard")
	}
	if strings.Contains(bugPrompt("I", 0), block) {
		t.Error("bugPrompt(threshold=0) contains the ask-format block; it must stay inside the threshold guard")
	}
}

// The checklist-format instruction lives in exactly one place. This asserts the
// block exists and still carries the rules that keep the published checklist
// scannable — a silent deletion of one bullet would otherwise pass every other
// test in this file.
func TestUATFormatBlockCarriesItsRules(t *testing.T) {
	d := promptData()
	d["UATCoverage"] = "every behavior the spec describes"
	got := mustRender("uat-format", d)
	for _, want := range []string{
		"### Happy path",
		"### Edge cases",
		"`Action → expected result`",
		"- [ ]",
		"15 words or fewer",
		"Compress wording, never coverage",
		"every behavior the spec describes",
		"Do not modify, create, or commit any file.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("uat-format block is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "under 20 items") {
		t.Errorf("uat-format block still caps the item count:\n%s", got)
	}
}

// Both routes must ask in the same shape, from the same source. This is what
// catches an edit made to one UAT prompt that should have been an edit to the
// shared block.
func TestBothRoutesShareTheUATFormatBlock(t *testing.T) {
	feature := promptData()
	feature["UATCoverage"] = "every behavior the spec describes"
	if block := mustRender("uat-format", feature); !strings.Contains(uatFeaturePrompt("docs/spec.md"), block) {
		t.Error("uatFeaturePrompt does not contain the uat-format block")
	}
	bug := promptData()
	bug["UATCoverage"] = "the reported bug and every behavior the fix touches"
	if block := mustRender("uat-format", bug); !strings.Contains(uatBugPrompt("I", "main"), block) {
		t.Error("uatBugPrompt does not contain the uat-format block")
	}
}

// Every .md.tmpl file on disk must have made it into the binary and been
// parsed. This walks the real directory, not promptFS: reading the embedded FS
// could only confirm what embed already put there, so it could never catch a
// file the //go:embed pattern or the ParseFS glob missed — which is exactly the
// failure worth catching, since it surfaces at runtime as a mustRender panic.
func TestEveryPromptFileOnDiskIsParsed(t *testing.T) {
	const dir = "ai/prompts"
	found := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md.tmpl") {
			return nil
		}
		found++
		if filepath.Dir(path) != filepath.Clean(dir) {
			t.Errorf("%s is nested; %s must stay flat (neither the embed pattern nor the ParseFS glob descends)", path, dir)
			return nil
		}
		if prompts.Lookup(d.Name()) == nil {
			t.Errorf("%s is on disk but was not parsed into the binary", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatalf("no .md.tmpl files found under %s", dir)
	}
}

// Sentinels come from the Go constants, never from literal text in a template.
func TestNoSentinelIsHardcodedInATemplate(t *testing.T) {
	entries, err := promptFS.ReadDir("ai/prompts")
	if err != nil {
		t.Fatal(err)
	}
	sentinels := []string{confidenceSentinel, specReadySentinel, readySentinel, alreadyDoneSentinel, doneConfirmSentinel,
		uatBeginSentinel, uatEndSentinel, uatMarker, codeReviewBeginSentinel, codeReviewEndSentinel}
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
