package engine

// alreadyDoneError signals that a pipeline concluded the issue is already
// implemented (the already_done outcome in an entry turn's structured output,
// confirmed by the PO proxy). The orchestrator closes the issue instead of
// shipping a PR.
type alreadyDoneError struct{ reason string }

func (e *alreadyDoneError) Error() string { return "already implemented: " + e.reason }
