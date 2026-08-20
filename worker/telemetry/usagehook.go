package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	wire "github.com/ngthluu/loope/shared"
)

// claudeStatusLineInput is the subset of the JSON Claude Code feeds to a
// configured statusLine command that the usage hook cares about; other fields
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

// UsageHookFile is where `loope claude-usage-hook` writes the latest
// rate-limit snapshot, and where the telemetry exporter reads it back.
func UsageHookFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "loope-usage.json"), nil
}

// ParseClaudeStatusLine decodes the statusLine hook's JSON into a
// UsageSnapshot stamped with capturedAt. A reset timestamp that fails to
// parse is left zero rather than failing the whole snapshot — a partial
// usage read is still worth recording.
func ParseClaudeStatusLine(data []byte, capturedAt time.Time) (wire.UsageSnapshot, error) {
	var in claudeStatusLineInput
	if err := json.Unmarshal(data, &in); err != nil {
		return wire.UsageSnapshot{}, err
	}
	u := wire.UsageSnapshot{
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
