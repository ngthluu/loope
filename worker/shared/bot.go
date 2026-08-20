package shared

import "strings"

// BotMarker tags the daemon's own status chatter — pickup, park, PR link,
// already-done, stopped. Like the UAT marker it is an HTML comment, so it is
// invisible on GitHub while staying greppable in the raw body, and it lets
// FetchIssueContent strip that chatter back out instead of feeding the model a
// transcript of its own past runs (see IsBotStatusComment).
//
// The needs-info comment is deliberately NOT tagged: it carries the numbered
// questions a human answers in the next comment, so removing it would orphan
// the answer.
const BotMarker = "<!-- loope:bot -->"

// legacyBotStatusPrefixes recognise status comments posted before BotMarker
// existed. They are the exact opening text of each tagged template, so nothing
// a human writes is mistaken for chatter, and needs-info ("🤖 Not confident
// enough…") is left alone here too.
var legacyBotStatusPrefixes = []string{
	"🤖 Picked up (",
	"🤖 Parked as `",
	"🤖 PR: ",
	"🤖 Already implemented — closing.",
	"⏸ Stopped by user.",
}

// IsBotStatusComment reports whether a comment is the daemon's own status
// chatter and so should be kept out of the issue content handed to Claude.
func IsBotStatusComment(body string) bool {
	if strings.Contains(body, BotMarker) {
		return true
	}
	trimmed := strings.TrimSpace(body)
	for _, p := range legacyBotStatusPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}
