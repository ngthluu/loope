package shared

import (
	"fmt"
	"strconv"
)

// Tail returns the last n bytes of s.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// Clip shortens s to at most max runes while keeping BOTH ends, noting how much
// was dropped. A failure's head names the failing step and its tail carries the
// API's own message, so Tail() alone threw away half the diagnosis — which is
// how a parked issue ended up commented with a generic, contextless snippet.
func Clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max || max <= 0 {
		return s
	}
	head := max / 2
	tailN := max - head
	return string(r[:head]) + fmt.Sprintf("\n…[%d chars omitted]…\n", len(r)-max) + string(r[len(r)-tailN:])
}

// Duration renders a millisecond span compactly: "45s", "3m26s", "1h02m". Zero
// or negative reads as an em dash.
func Duration(ms int) string {
	if ms <= 0 {
		return "—"
	}
	s := ms / 1000
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	m := s / 60
	s %= 60
	if m < 60 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := m / 60
	m %= 60
	return fmt.Sprintf("%dh%02dm", h, m)
}
