package engine

import (
	"testing"

	"github.com/ngthluu/loope/worker/shared"
)

func TestConfidenceGate(t *testing.T) {
	score := func(n int) *int { return &n }
	cases := []struct {
		name      string
		threshold int
		er        entryResult
		wantLow   bool
	}{
		{"below threshold", 50, entryResult{Confidence: score(30), Detail: "what format?"}, true},
		{"at threshold passes", 50, entryResult{Confidence: score(50)}, false},
		{"above threshold passes", 50, entryResult{Confidence: score(85)}, false},
		{"absent score fails open", 50, entryResult{}, false},
		{"disabled gate ignores score", 0, entryResult{Confidence: score(1)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &shared.Config{ConfidenceThreshold: tc.threshold}
			err := confidenceGate(cfg, tc.er)
			if got := err != nil; got != tc.wantLow {
				t.Fatalf("confidenceGate = %v, want low=%v", err, tc.wantLow)
			}
			if tc.wantLow {
				lc, ok := err.(*lowConfidenceError)
				if !ok {
					t.Fatalf("error type = %T, want *lowConfidenceError", err)
				}
				if lc.score != *tc.er.Confidence || lc.feedback != tc.er.Detail {
					t.Errorf("lowConfidenceError = (%d, %q), want (%d, %q)", lc.score, lc.feedback, *tc.er.Confidence, tc.er.Detail)
				}
			}
		})
	}
}

func TestLowConfidenceErrorMessage(t *testing.T) {
	e := &lowConfidenceError{score: 30, feedback: "needs acceptance criteria"}
	if e.Error() != "low confidence (30): needs acceptance criteria" {
		t.Errorf("Error() = %q", e.Error())
	}
}
