package shared

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMachineIDStableAndDistinct(t *testing.T) {
	a := MachineID("host1", "/work/a")
	b := MachineID("host1", "/work/a")
	if a != b {
		t.Fatalf("machineID not stable: %q != %q", a, b)
	}
	if len(a) != 12 {
		t.Fatalf("machineID length = %d, want 12", len(a))
	}
	c := MachineID("host1", "/work/b")
	if a == c {
		t.Fatalf("machineID for a different workDir must differ")
	}
}

func TestPushRequestRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	want := PushRequest{
		Resource: Resource{RepoSlug: "o/r", MachineID: "abc123def456", Hostname: "host1", WorkDir: "/work", Version: "dev", PushIntervalSec: 15},
		Logs:     []LogRecord{{Timestamp: now, Body: "hello"}},
		Usage: &UsageSnapshot{
			FiveHourUsedPct: 12.5, FiveHourResetAt: now,
			SevenDayUsedPct: 40, SevenDayResetAt: now,
			CapturedAt: now,
		},
		SentAt: now,
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got PushRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Resource != want.Resource {
		t.Fatalf("resource round-trip = %+v, want %+v", got.Resource, want.Resource)
	}
	if len(got.Logs) != 1 || got.Logs[0].Body != "hello" || !got.Logs[0].Timestamp.Equal(now) {
		t.Fatalf("logs round-trip = %+v", got.Logs)
	}
	if got.Usage == nil || *got.Usage != *want.Usage {
		t.Fatalf("usage round-trip = %+v, want %+v", got.Usage, want.Usage)
	}
}

func TestPushRequestNilUsageRoundTrip(t *testing.T) {
	data, err := json.Marshal(PushRequest{Resource: Resource{RepoSlug: "o/r"}})
	if err != nil {
		t.Fatal(err)
	}
	var got PushRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Usage != nil {
		t.Fatalf("usage = %+v, want nil", got.Usage)
	}
}
