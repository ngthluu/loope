package main

import "testing"

func TestParseTelemetryServerFlagsDefaults(t *testing.T) {
	addr, token, dataDir, err := parseTelemetryServerFlags([]string{"-token", "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if addr != ":9090" {
		t.Fatalf("addr = %q, want the :9090 default", addr)
	}
	if token != "secret" {
		t.Fatalf("token = %q", token)
	}
	if dataDir != "" {
		t.Fatalf("dataDir = %q, want empty by default", dataDir)
	}
}

func TestParseTelemetryServerFlagsOverrides(t *testing.T) {
	addr, token, dataDir, err := parseTelemetryServerFlags([]string{"-addr", ":9999", "-token", "secret", "-data-dir", "/tmp/telemetry"})
	if err != nil {
		t.Fatal(err)
	}
	if addr != ":9999" || token != "secret" || dataDir != "/tmp/telemetry" {
		t.Fatalf("addr=%q token=%q dataDir=%q", addr, token, dataDir)
	}
}

func TestParseTelemetryServerFlagsRequiresToken(t *testing.T) {
	if _, _, _, err := parseTelemetryServerFlags([]string{}); err == nil {
		t.Fatal("expected an error when -token is not given")
	}
}

func TestRunTelemetryServerCmdReturns2WithoutToken(t *testing.T) {
	if code := runTelemetryServerCmd([]string{}); code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
}
