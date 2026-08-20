package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/testkit"
)

func TestGateBlocksOnRequiredFailure(t *testing.T) {
	f := &testkit.FakeRunner{Handler: okHandler(map[string]testkit.RResp{
		"claude --version": {Err: errors.New("not found")},
	})}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), false)
	if proceed {
		t.Fatal("proceed = true, want false when a required check failed")
	}
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "claude") {
		t.Fatalf("report must name the failing check, got %q", buf.String())
	}
}

func TestGateWarningsOnlyProceedSilently(t *testing.T) {
	f := &testkit.FakeRunner{Handler: okHandler(map[string]testkit.RResp{
		"curl --version": {Err: errors.New("not found")},
	})}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), false)
	if !proceed || code != 0 {
		t.Fatalf("gate = (%d, %v), want (0, true) for warnings only", code, proceed)
	}
	if buf.String() != "" {
		t.Fatalf("a healthy run must print nothing, got %q", buf.String())
	}
}

func TestGateDoctorAlwaysReportsAndNeverProceeds(t *testing.T) {
	f := &testkit.FakeRunner{Handler: okHandler(nil)}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, preflightConfig(), true)
	if proceed {
		t.Fatal("-doctor must never proceed into the loop")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when everything passes", code)
	}
	if !strings.Contains(buf.String(), "loope preflight") {
		t.Fatalf("-doctor must print the report even when healthy, got %q", buf.String())
	}

	f2 := &testkit.FakeRunner{Handler: okHandler(map[string]testkit.RResp{"gh --version": {Err: errors.New("not found")}})}
	var buf2 bytes.Buffer
	code2, _ := gate(context.Background(), &buf2, f2, preflightConfig(), true)
	if code2 != 1 {
		t.Fatalf("-doctor exit code = %d, want 1 when a required check failed", code2)
	}
}

func TestGateNoConfigDoctorReportsAndSkipsRepoChecks(t *testing.T) {
	f := &testkit.FakeRunner{Handler: okHandler(nil)}
	var buf bytes.Buffer
	code, proceed := gate(context.Background(), &buf, f, nil, true)
	if proceed {
		t.Fatal("-doctor must never proceed into the loop")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 when the environment is healthy without config", code)
	}
	if !strings.Contains(buf.String(), "loope preflight") {
		t.Fatalf("no-config -doctor must print the report, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "no --config") {
		t.Fatalf("no-config -doctor report must note the skipped repo checks, got %q", buf.String())
	}
}

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name       string
		configPath string
		version    bool
		doctor     bool
		want       cliMode
	}{
		{"version wins over config and doctor", "loope.json", true, true, modeVersion},
		{"version without config", "", true, false, modeVersion},
		{"config runs", "loope.json", false, false, modeRun},
		{"config with doctor still runs", "loope.json", false, true, modeRun},
		{"bare invocation is help", "", false, false, modeHelp},
		{"doctor without config runs config-less doctor", "", false, true, modeDoctorNoConfig},
	}
	for _, c := range cases {
		if got := resolveMode(c.configPath, c.version, c.doctor); got != c.want {
			t.Errorf("%s: resolveMode(%q, %v, %v) = %d, want %d",
				c.name, c.configPath, c.version, c.doctor, got, c.want)
		}
	}
}

// TestDispatchSubcommandTelemetryServerFallsThrough locks in the monorepo
// split: the worker binary no longer embeds the telemetry server, so the old
// `loope telemetry-server` invocation falls through to normal flag parsing
// (and fails there) instead of being silently handled.
func TestDispatchSubcommandTelemetryServerFallsThrough(t *testing.T) {
	if _, handled := dispatchSubcommand([]string{"loope", "telemetry-server"}, strings.NewReader("")); handled {
		t.Fatal("telemetry-server must no longer be a worker subcommand — it is its own binary")
	}
}

func TestDispatchSubcommandClaudeUsageHook(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	input := `{"rate_limits":{"five_hour":{"used_percentage":10},"seven_day":{"used_percentage":20}}}`
	code, handled := dispatchSubcommand([]string{"loope", "claude-usage-hook"}, strings.NewReader(input))
	if !handled {
		t.Fatal("expected claude-usage-hook to be handled")
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
}

func TestDispatchSubcommandStatusLineMissingConfigExits2(t *testing.T) {
	code, handled := dispatchSubcommand([]string{"loope", "status-line"}, strings.NewReader(""))
	if !handled {
		t.Fatal("expected status-line to be handled")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2 (missing required --config)", code)
	}
}

func TestDispatchSubcommandFallsThroughForDaemonInvocation(t *testing.T) {
	for _, args := range [][]string{{"loope"}, {"loope", "--config", "x.json"}, {"loope", "--version"}} {
		if _, handled := dispatchSubcommand(args, strings.NewReader("")); handled {
			t.Fatalf("args %v: expected handled=false", args)
		}
	}
}
