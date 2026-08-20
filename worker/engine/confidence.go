package engine

import (
	"fmt"
)

// noConfidenceScore marks a lowConfidenceError raised without any reported
// score: a session that stopped to ask questions without committing a fix
// (see afterFix). The needs-info comment renders a different lead line for it
// instead of a bogus "confidence -1/100".
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
