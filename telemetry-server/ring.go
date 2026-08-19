package main

// LogRingBuffer is a bounded FIFO of log lines: appending past capacity
// drops the oldest lines first. Not safe for concurrent use — callers
// serialize access (TelemetryServer guards it with its own mutex).
type LogRingBuffer struct {
	cap   int
	lines []string
}

// NewLogRingBuffer returns a buffer that holds at most cap lines.
func NewLogRingBuffer(cap int) *LogRingBuffer {
	return &LogRingBuffer{cap: cap}
}

// Add appends lines, dropping the oldest entries first if the total exceeds
// the buffer's capacity.
func (b *LogRingBuffer) Add(lines ...string) {
	b.lines = append(b.lines, lines...)
	if over := len(b.lines) - b.cap; over > 0 {
		b.lines = b.lines[over:]
	}
}

// Lines returns the buffered lines, oldest first. The returned slice is a
// fresh copy, safe for the caller to retain across further Add calls.
func (b *LogRingBuffer) Lines() []string {
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}
