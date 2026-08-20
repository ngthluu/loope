package engine

import (
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/infra"
)

func TestDiffAddedLinesNewCommentAppended(t *testing.T) {
	old := "# Title (#7)\n\nbody\n\n## Comments\n\n@alice: first comment\n"
	new_ := old + "\n@bob: second comment\n"
	got := diffAddedLines(old, new_)
	if got != "\n@bob: second comment\n" && got != "@bob: second comment\n" {
		t.Errorf("diff = %q, want just the new comment", got)
	}
}

func TestDiffAddedLinesBodyEdited(t *testing.T) {
	old := "# Title (#7)\n\noriginal body\n"
	new_ := "# Title (#7)\n\nedited body with more detail\n"
	got := diffAddedLines(old, new_)
	if got == "" {
		t.Fatal("an edited body must produce a non-empty diff")
	}
	if !strings.Contains(got, "edited body with more detail") {
		t.Errorf("diff = %q, want it to contain the edited line", got)
	}
}

func TestDiffAddedLinesNothingChanged(t *testing.T) {
	text := "# Title (#7)\n\nbody\n"
	if got := diffAddedLines(text, text); got != "" {
		t.Errorf("diff = %q, want empty for identical content", got)
	}
}

func TestResumePromptDefaultsToContinue(t *testing.T) {
	logDir := t.TempDir()
	got := resumePrompt(logDir, "ai-rework", "ai-needs-info", "new content")
	if got != "continue" {
		t.Errorf("prompt = %q, want continue for a non-needs-info prior state", got)
	}
}

func TestResumePromptDiffsOnNeedsInfoReentry(t *testing.T) {
	logDir := t.TempDir()
	c := infra.NewClaude(nil, logDir, "")
	c.RecordSnapshot("# Title (#7)\n\nbody\n")
	newContent := "# Title (#7)\n\nbody\n\n## Comments\n\n@alice: an answer\n"
	got := resumePrompt(logDir, "ai-needs-info", "ai-needs-info", newContent)
	if got == "continue" || !strings.Contains(got, "an answer") {
		t.Errorf("prompt = %q, want the diffed new comment", got)
	}
}

func TestResumePromptFallsBackToContinueOnEmptyDiff(t *testing.T) {
	logDir := t.TempDir()
	c := infra.NewClaude(nil, logDir, "")
	c.RecordSnapshot("# Title (#7)\n\nbody\n")
	got := resumePrompt(logDir, "ai-needs-info", "ai-needs-info", "# Title (#7)\n\nbody\n")
	if got != "continue" {
		t.Errorf("prompt = %q, want continue when the label was removed with no new content", got)
	}
}

func TestResumePromptFallsBackToContinueOnMissingSnapshot(t *testing.T) {
	logDir := t.TempDir() // no snapshot recorded
	got := resumePrompt(logDir, "ai-needs-info", "ai-needs-info", "new content")
	if got != "continue" {
		t.Errorf("prompt = %q, want continue when there is no snapshot to diff against", got)
	}
}
