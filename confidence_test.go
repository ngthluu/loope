package main

import "testing"

func TestParseConfidence(t *testing.T) {
	cases := []struct {
		in    string
		score int
		ok    bool
	}{
		{"CONFIDENCE: 85\nrest", 85, true},
		{"CONFIDENCE:40", 40, true},
		{"some preamble\nCONFIDENCE: 12/100\nmore", 12, true},
		{"CONFIDENCE: high", 0, false},
		{"no sentinel here", 0, false},
		{"CONFIDENCE:", 0, false},
	}
	for _, c := range cases {
		score, ok := parseConfidence(c.in)
		if ok != c.ok || (ok && score != c.score) {
			t.Errorf("parseConfidence(%q) = %d,%v; want %d,%v", c.in, score, ok, c.score, c.ok)
		}
	}
}

func TestStripConfidenceLine(t *testing.T) {
	in := "CONFIDENCE: 30\nMissing acceptance criteria.\nWhat is the target format?"
	got := stripConfidenceLine(in)
	want := "Missing acceptance criteria.\nWhat is the target format?"
	if got != want {
		t.Errorf("stripConfidenceLine = %q, want %q", got, want)
	}
	// No sentinel: returned trimmed but otherwise unchanged.
	if got := stripConfidenceLine("  hello  "); got != "hello" {
		t.Errorf("stripConfidenceLine(no sentinel) = %q", got)
	}
}

func TestLowConfidenceErrorMessage(t *testing.T) {
	e := &lowConfidenceError{score: 30, feedback: "needs acceptance criteria"}
	if e.Error() != "low confidence (30): needs acceptance criteria" {
		t.Errorf("Error() = %q", e.Error())
	}
}
