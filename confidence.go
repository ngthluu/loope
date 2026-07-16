package main

import (
	"fmt"
	"strconv"
	"strings"
)

// confidenceSentinel prefixes the score the architect prints on the first line
// of its opening brainstorm turn: "CONFIDENCE: <0-100>".
const confidenceSentinel = "CONFIDENCE:"

// lowConfidenceError signals that the architect judged an issue too
// under-specified to implement. The orchestrator comments the feedback and
// applies the needs-info label instead of shipping or parking.
type lowConfidenceError struct {
	score    int
	feedback string
}

func (e *lowConfidenceError) Error() string {
	return fmt.Sprintf("low confidence (%d): %s", e.score, e.feedback)
}

// parseConfidence finds confidenceSentinel and parses the integer following it
// on the same line. ok is false when the sentinel is absent or no leading
// integer follows it (e.g. "CONFIDENCE: high").
func parseConfidence(s string) (int, bool) {
	i := strings.Index(s, confidenceSentinel)
	if i < 0 {
		return 0, false
	}
	rest := s[i+len(confidenceSentinel):]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	rest = strings.TrimSpace(rest)
	// Take leading digits only, so "12/100" or "40." parse to the integer.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// stripConfidenceLine returns s with the line containing confidenceSentinel
// removed, trimmed. When the sentinel is absent it returns s trimmed.
func stripConfidenceLine(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if strings.Contains(ln, confidenceSentinel) {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
