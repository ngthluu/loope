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

func TestSanitizeFeedback(t *testing.T) {
	in := "CONFIDENCE: 30\nMissing acceptance criteria.\nWhat is the target format?"
	got := sanitizeFeedback(in)
	want := "Missing acceptance criteria.\nWhat is the target format?"
	if got != want {
		t.Errorf("sanitizeFeedback = %q, want %q", got, want)
	}
	// No sentinel: returned trimmed but otherwise unchanged.
	if got := sanitizeFeedback("  hello  "); got != "hello" {
		t.Errorf("sanitizeFeedback(no sentinel) = %q", got)
	}
	// A low-confidence reply that also claims already-done: the gate deliberately
	// ignored that claim, so the sentinel must not reach the public needs-info
	// comment either.
	in = "CONFIDENCE: 20\nI cannot tell what behavior is wrong.\nPIPELINE_ALREADY_DONE: looks fine to me"
	want = "I cannot tell what behavior is wrong."
	if got := sanitizeFeedback(in); got != want {
		t.Errorf("sanitizeFeedback = %q, want %q", got, want)
	}
}

func TestLowConfidenceErrorMessage(t *testing.T) {
	e := &lowConfidenceError{score: 30, feedback: "needs acceptance criteria"}
	if e.Error() != "low confidence (30): needs acceptance criteria" {
		t.Errorf("Error() = %q", e.Error())
	}
}
