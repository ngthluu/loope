package engine

import (
	"fmt"
	"strconv"
	"strings"
)

// confidenceSentinel prefixes the score the architect prints on the first line
// of its opening brainstorm turn: "CONFIDENCE: <0-100>".
const confidenceSentinel = "CONFIDENCE:"

// noConfidenceScore marks a lowConfidenceError raised without any parsed
// score: a debug session that stopped to ask questions without printing a
// sentinel or committing a fix (see afterDebug). The needs-info comment
// renders a different lead line for it instead of a bogus "confidence
// -1/100".
const noConfidenceScore = -1

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

// parseSentinelLine finds the first line whose trimmed text starts with
// sentinel and returns the trimmed remainder of that line. The sentinel must
// lead its own line (per every sentinel's prompt contract): a session's prose
// can otherwise quote a sentinel name mid-sentence (e.g. "...gated on
// `CONFIDENCE:`/`PIPELINE_ALREADY_DONE:` sentinels..." while describing the
// gate it just fixed), which a bare substring search would misread as the
// terminal signal (see issue #73/#76).
func parseSentinelLine(s, sentinel string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), sentinel); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// parseConfidence finds a line leading with confidenceSentinel and parses the
// integer following it. ok is false when the sentinel is absent or no leading
// integer follows it (e.g. "CONFIDENCE: high").
func parseConfidence(s string) (int, bool) {
	rest, ok := parseSentinelLine(s, confidenceSentinel)
	if !ok {
		return 0, false
	}
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

// sanitizeFeedback returns s with every control-sentinel line removed,
// trimmed. When no sentinel is present it returns s trimmed.
//
// The result is posted verbatim as the public needs-info comment, so it must
// carry only the architect's prose. Two sentinels can reach it: the confidence
// score itself, which is machine state rather than feedback; and an
// already-done claim from a session that scored low, which the gate outranks on
// purpose — pasting it into the comment would tell the author the issue was
// closed as implemented when it was in fact escalated as under-specified.
func sanitizeFeedback(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if strings.Contains(ln, confidenceSentinel) || strings.Contains(ln, alreadyDoneSentinel) {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
