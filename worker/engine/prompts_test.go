package engine

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Representative data for every renderable template in the embedded FS. A new
// prompt file or {{define}} block with no entry here fails TestEveryTemplateRenders —
// which is the point: it catches a prompt that was added but never wired up.
var promptTestData = map[string]map[string]any{
	"entry.md.tmpl":             {"Issue": "I", "Threshold": 70},
	"entry-resume.md.tmpl":      {"Trigger": "T"},
	"brainstorm-resume.md.tmpl": {"Trigger": "T"},
	"answerer.md.tmpl":          {"Issue": "I", "Persona": "P", "ArchitectMsg": "A"},
	"qa-nudge.md.tmpl":          {},
	"done-confirm.md.tmpl":      {"Issue": "I", "Persona": "P", "Reason": "R"},
	"plan.md.tmpl":              {"SpecPath": "docs/spec.md"},
	"execute.md.tmpl":           {"PlanPath": "docs/plan.md"},
	"pickup":                    {"Branch": "b"},
	"already-done":              {"Reason": "R"},
	"needs-info":                {"Score": 1, "Label": "l", "Feedback": "F"},
	"park":                      {"Label": "ai-rework", "Guidance": "G", "Error": "E"},
	"pr-comment":                {"URL": "u"},
	"plan-comment":              {"Path": "docs/superpowers/plans/2026-plan.md"},
	"pr-title":                  {"Title": "T", "Number": 1},
	"pr-body":                   {"Number": 1, "Kind": "bug"},
	"guidance-usage-limit":      {},
	"guidance-budget":           {},
	"guidance-network":          {},
	"ask-format":                {},
	"entry-outcomes":            {},
	"po-preamble":               {"Issue": "I", "Persona": "P"},
	"uat-format":                {"UATCoverage": "C"},
	"uat-section":               {"Checklist": "- [ ] C"},
	"uat-feature.md.tmpl":       {"SpecPath": "docs/spec.md", "UATCoverage": "C"},
	"uat-bug.md.tmpl":           {"Issue": "I", "Base": "main", "UATCoverage": "C"},
	"codereview.md.tmpl":        {"Round": 1, "Rounds": 2, "Base": "main"},
	"codereview-comment":        {"Round": 1, "Rounds": 2, "Status": "fixed", "Summary": "S"},
	"mergeresolve.md.tmpl":      {"Base": "main"},
	"mergeresolve-pickup":       {"Base": "main", "Branch": "b"},
	"mergeresolve-done":         {"Base": "main", "Branch": "b", "Summary": "S"},
	"mergeresolve-park":         {"Label": "ai-rework", "TriggerLabel": "ai-resolve-merge", "Guidance": "G", "Error": "E"},
}

// skipTemplates are the names in the set that are not prompts: the root
// template ParseFS was seeded with, and the container files whose own bodies
// are just the whitespace between their {{define}} blocks.
var skipTemplates = map[string]bool{
	"prompts":                true,
	"comments.md.tmpl":       true,
	"ask-format.md.tmpl":     true,
	"uat-format.md.tmpl":     true,
	"entry-outcomes.md.tmpl": true,
	"po-preamble.md.tmpl":    true,
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

// The entry prompt asks in the shared shape, and — via the threshold=0 case —
// the instruction stays inside the confidence gate's guard.
func TestEntryPromptSharesTheAskFormatBlock(t *testing.T) {
	block := mustRender("ask-format", promptData())
	if !strings.Contains(entryPrompt("I", 70), block) {
		t.Error("entryPrompt(threshold=70) does not contain the ask-format block")
	}
	if strings.Contains(entryPrompt("I", 0), block) {
		t.Error("entryPrompt(threshold=0) contains the ask-format block; it must stay inside the threshold guard")
	}
}

// The outcome contract lives in exactly one place. Every prompt that opens or
// resumes an entry-stage turn must carry the rendered block verbatim — the
// hand-copied lists it replaced had drifted apart — and the block itself must
// still name every outcome the entry loop dispatches on, worded against the
// schema descriptions in pipeline_entry.go.
func TestEntryOutcomesBlockIsSharedByEveryEntryPrompt(t *testing.T) {
	block := mustRender("entry-outcomes", promptData())
	for _, outcome := range []string{entryOutcomeFix, entryOutcomeSpec, entryOutcomeDone, entryOutcomeQuestion} {
		if !strings.Contains(block, `"`+outcome+`"`) {
			t.Errorf("entry-outcomes block is missing the %s contract:\n%s", outcome, block)
		}
	}
	for _, want := range []string{"spec_path", "do not invent work", "status update", "product owner"} {
		if !strings.Contains(block, want) {
			t.Errorf("entry-outcomes block is missing %q:\n%s", want, block)
		}
	}
	for _, tc := range []struct{ name, got string }{
		{"entryPrompt(threshold=70)", entryPrompt("I", 70)},
		{"entryPrompt(threshold=0)", entryPrompt("I", 0)},
		{"entryResumePrompt", entryResumePrompt("continue")},
		{"brainstormResumePrompt", brainstormResumePrompt("continue")},
		{"qaNudgePrompt", qaNudgePrompt()},
	} {
		if !strings.Contains(tc.got, block) {
			t.Errorf("%s does not contain the entry-outcomes block", tc.name)
		}
	}
}

// The fresh entry prompt must state the environment contract the pipeline
// relies on: the engine pushes and opens the PR itself (worktree.go), and the
// spec must land under a specs/ directory for resolveSpec's fallback to find it.
func TestEntryPromptStatesEnvironmentContract(t *testing.T) {
	got := entryPrompt("I", 70)
	for _, want := range []string{
		"dedicated git worktree",
		"Never push, open PRs, switch branches, rebase, amend, or force-push",
		"`specs/` directory",
		"spec_path",
		"needs-info comment",
		"product-owner proxy",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("entryPrompt is missing %q", want)
		}
	}
	if strings.Contains(entryPrompt("I", 0), "needs-info comment") {
		t.Error("entryPrompt(threshold=0) mentions the needs-info path; it must stay inside the threshold guard")
	}
}

// Both product-owner-proxy prompts open with the same preamble, from the same
// source, and that preamble declares the proxy read-only (the call sites also
// disallow the edit tools; the prose keeps the model from trying).
func TestPOPreambleIsSharedByProxyPrompts(t *testing.T) {
	d := promptData()
	d["Issue"] = "I"
	d["Persona"] = "P"
	block := mustRender("po-preamble", d)
	if !strings.Contains(block, "You are read-only: do not modify, create, or commit any file in the repository.") {
		t.Errorf("po-preamble block is missing the read-only rule:\n%s", block)
	}
	if !strings.Contains(block, "Product owner preferences (persona):\nP") {
		t.Errorf("po-preamble block is missing the persona section:\n%s", block)
	}
	for _, tc := range []struct{ name, got string }{
		{"answererPrompt", answererPrompt("I", "P", "A")},
		{"doneConfirmPrompt", doneConfirmPrompt("I", "P", "R")},
	} {
		if !strings.Contains(tc.got, block) {
			t.Errorf("%s does not contain the po-preamble block", tc.name)
		}
	}
	// An empty persona drops the heading rather than rendering it over nothing.
	d["Persona"] = ""
	if got := mustRender("po-preamble", d); strings.Contains(got, "persona") {
		t.Errorf("po-preamble renders the persona heading with no persona:\n%s", got)
	}
}

// Every schema a session call passes must be valid JSON naming the fields its
// parser reads — a typo would otherwise only surface as an off-contract
// session at runtime.
func TestSessionSchemasAreValidJSON(t *testing.T) {
	for name, schema := range map[string]string{
		"entry":        entryResultSchema,
		"plan":         planResultSchema,
		"answerer":     answererResultSchema,
		"doneConfirm":  doneConfirmSchema,
		"uat":          uatResultSchema,
		"codeReview":   codeReviewResultSchema,
		"mergeResolve": mergeResolveResultSchema,
	} {
		var doc struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal([]byte(schema), &doc); err != nil {
			t.Errorf("%s schema is not valid JSON: %v", name, err)
			continue
		}
		if doc.Type != "object" || len(doc.Properties) == 0 || len(doc.Required) == 0 {
			t.Errorf("%s schema must be an object with properties and required fields, got %+v", name, doc)
		}
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

// The old prose sentinels are gone: session outcomes are schema-enforced
// structured fields now, so no template may reintroduce one as literal text.
func TestNoLegacySentinelSurvivesInATemplate(t *testing.T) {
	entries, err := promptFS.ReadDir("ai/prompts")
	if err != nil {
		t.Fatal(err)
	}
	legacy := []string{"CONFIDENCE:", "SPEC_READY", "FIX_COMMITTED", "PIPELINE_READY", "PIPELINE_ALREADY_DONE",
		"DONE_CONFIRMED", "QA_NOTHING_TO_ANSWER", "UAT_BEGIN", "UAT_END", "CODEREVIEW_BEGIN", "CODEREVIEW_END",
		"MERGE_RESOLVE_STATUS"}
	for _, e := range entries {
		b, err := promptFS.ReadFile("ai/prompts/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range legacy {
			if strings.Contains(string(b), s) {
				t.Errorf("%s still carries the retired sentinel %q — report the outcome via the call's --json-schema instead", e.Name(), s)
			}
		}
	}
	// The one marker that IS still injected rather than hardcoded.
	if !strings.Contains(uatSection("- [ ] x"), uatMarker) {
		t.Error("uatSection must still carry the idempotency marker from promptData()")
	}
}
