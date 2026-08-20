package main

import (
	"github.com/ngthluu/loope/shared"
	"github.com/ngthluu/loope/worker/telemetry"

	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseClaudeStatusLine(t *testing.T) {
	in := []byte(`{"rate_limits":{"five_hour":{"used_percentage":12.5,"resets_at":"2026-08-19T15:00:00Z"},"seven_day":{"used_percentage":40,"resets_at":"2026-08-22T00:00:00Z"}}}`)
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	u, err := telemetry.ParseClaudeStatusLine(in, now)
	if err != nil {
		t.Fatal(err)
	}
	if u.FiveHourUsedPct != 12.5 || u.SevenDayUsedPct != 40 {
		t.Fatalf("usage = %+v", u)
	}
	if !u.CapturedAt.Equal(now) {
		t.Fatalf("CapturedAt = %v, want %v", u.CapturedAt, now)
	}
	wantReset := time.Date(2026, 8, 19, 15, 0, 0, 0, time.UTC)
	if !u.FiveHourResetAt.Equal(wantReset) {
		t.Fatalf("FiveHourResetAt = %v, want %v", u.FiveHourResetAt, wantReset)
	}
}

func TestParseClaudeStatusLineMalformedJSON(t *testing.T) {
	if _, err := telemetry.ParseClaudeStatusLine([]byte("not json"), time.Now()); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestParseClaudeStatusLineUnparsableResetLeftZero(t *testing.T) {
	in := []byte(`{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":"garbage"}}}`)
	u, err := telemetry.ParseClaudeStatusLine(in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !u.FiveHourResetAt.IsZero() {
		t.Fatalf("FiveHourResetAt = %v, want zero value for an unparsable timestamp", u.FiveHourResetAt)
	}
}

func TestRunClaudeUsageHookCmdWritesFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	input := `{"rate_limits":{"five_hour":{"used_percentage":5},"seven_day":{"used_percentage":9}}}`
	if code := runClaudeUsageHookCmd(strings.NewReader(input)); code != 0 {
		t.Fatalf("code = %d", code)
	}
	path, err := telemetry.UsageHookFile()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got shared.UsageSnapshot
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.FiveHourUsedPct != 5 || got.SevenDayUsedPct != 9 {
		t.Fatalf("got = %+v", got)
	}
}

func TestRunClaudeUsageHookCmdInvalidInputReturnsNonZero(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if code := runClaudeUsageHookCmd(strings.NewReader("not json")); code == 0 {
		t.Fatal("expected a non-zero exit code for malformed input")
	}
}
