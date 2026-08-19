package main

import (
	"github.com/ngthluu/loope/shared"

	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// claudeStatusLineInput is the subset of the JSON Claude Code feeds to a
// configured statusLine command that this hook cares about; other fields
// (workspace, model, etc.) are ignored.
type claudeStatusLineInput struct {
	RateLimits struct {
		FiveHour struct {
			UsedPercentage float64 `json:"used_percentage"`
			ResetsAt       string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay struct {
			UsedPercentage float64 `json:"used_percentage"`
			ResetsAt       string  `json:"resets_at"`
		} `json:"seven_day"`
	} `json:"rate_limits"`
}

// usageHookFile is where `loope claude-usage-hook` writes the latest
// rate-limit snapshot, and where the telemetry exporter reads it back.
func usageHookFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "loope-usage.json"), nil
}

// parseClaudeStatusLine decodes the statusLine hook's JSON into a
// UsageSnapshot stamped with capturedAt. A reset timestamp that fails to
// parse is left zero rather than failing the whole snapshot — a partial
// usage read is still worth recording.
func parseClaudeStatusLine(data []byte, capturedAt time.Time) (shared.UsageSnapshot, error) {
	var in claudeStatusLineInput
	if err := json.Unmarshal(data, &in); err != nil {
		return shared.UsageSnapshot{}, err
	}
	u := shared.UsageSnapshot{
		FiveHourUsedPct: in.RateLimits.FiveHour.UsedPercentage,
		SevenDayUsedPct: in.RateLimits.SevenDay.UsedPercentage,
		CapturedAt:      capturedAt,
	}
	if t, err := time.Parse(time.RFC3339, in.RateLimits.FiveHour.ResetsAt); err == nil {
		u.FiveHourResetAt = t
	}
	if t, err := time.Parse(time.RFC3339, in.RateLimits.SevenDay.ResetsAt); err == nil {
		u.SevenDayResetAt = t
	}
	return u, nil
}

// runClaudeUsageHookCmd implements `loope claude-usage-hook`: it reads the
// statusLine JSON from stdin and overwrites ~/.claude/loope-usage.json with
// the parsed rate-limit snapshot. It prints nothing to stdout, so a user can
// tee the same stdin into it alongside their real statusLine command without
// affecting what Claude Code displays.
func runClaudeUsageHookCmd(stdin io.Reader) int {
	data, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: read stdin: %v\n", err)
		return 1
	}
	usage, err := parseClaudeStatusLine(data, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: parse: %v\n", err)
		return 1
	}
	path, err := usageHookFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	out, err := json.Marshal(usage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: write %s: %v\n", path, err)
		return 1
	}
	return 0
}
