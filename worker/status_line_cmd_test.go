package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrappedAndBareCommand(t *testing.T) {
	got := wrappedCommand("/usr/local/bin/loope", "/path/to/real-statusline.sh")
	want := `sh -c 'tee >(/usr/local/bin/loope claude-usage-hook) | /path/to/real-statusline.sh'`
	if got != want {
		t.Errorf("wrappedCommand = %q, want %q", got, want)
	}

	got = bareCommand("/usr/local/bin/loope")
	want = `sh -c 'tee >(/usr/local/bin/loope claude-usage-hook) >/dev/null'`
	if got != want {
		t.Errorf("bareCommand = %q, want %q", got, want)
	}
}

func TestMatchOursBare(t *testing.T) {
	loopePath := "/usr/local/bin/loope"
	isOurs, isWrapped, original := matchOurs(bareCommand(loopePath), loopePath)
	if !isOurs || isWrapped || original != "" {
		t.Fatalf("matchOurs(bare) = (%v, %v, %q), want (true, false, \"\")", isOurs, isWrapped, original)
	}
}

func TestMatchOursWrapped(t *testing.T) {
	loopePath := "/usr/local/bin/loope"
	cmd := wrappedCommand(loopePath, "/path/to/real-statusline.sh")
	isOurs, isWrapped, original := matchOurs(cmd, loopePath)
	if !isOurs || !isWrapped || original != "/path/to/real-statusline.sh" {
		t.Fatalf("matchOurs(wrapped) = (%v, %v, %q), want (true, true, %q)",
			isOurs, isWrapped, original, "/path/to/real-statusline.sh")
	}
}

func TestMatchOursForeignCommand(t *testing.T) {
	loopePath := "/usr/local/bin/loope"
	isOurs, _, _ := matchOurs("/some/other/statusline.sh", loopePath)
	if isOurs {
		t.Fatal("matchOurs: unrelated command must not match")
	}
}

func TestMatchOursDifferentLoopePath(t *testing.T) {
	// A command wrapped for a different loope binary path must not match —
	// this is what makes remove-after-move fail safely (edit manually)
	// rather than silently discarding an unrelated statusLine.
	cmd := wrappedCommand("/old/path/loope", "/path/to/real-statusline.sh")
	isOurs, _, _ := matchOurs(cmd, "/usr/local/bin/loope")
	if isOurs {
		t.Fatal("matchOurs: command wrapped for a different loope path must not match")
	}
}

func TestLoadSettingsMissingFile(t *testing.T) {
	settings, existed, err := loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if existed {
		t.Error("existed = true for a missing file, want false")
	}
	if len(settings) != 0 {
		t.Errorf("settings = %v, want empty", settings)
	}
}

func TestLoadSettingsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark","statusLine":{"type":"command","command":"foo"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, existed, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if !existed {
		t.Error("existed = false for an existing file, want true")
	}
	if _, ok := settings["theme"]; !ok {
		t.Error("unrelated key \"theme\" was dropped")
	}
	cmd, ok := statusLineCommand(settings)
	if !ok || cmd != "foo" {
		t.Errorf("statusLineCommand = (%q, %v), want (\"foo\", true)", cmd, ok)
	}
}

func TestStatusLineCommandAbsent(t *testing.T) {
	_, ok := statusLineCommand(map[string]json.RawMessage{})
	if ok {
		t.Error("statusLineCommand: ok = true for absent key, want false")
	}
}

func TestSetAndReadBackStatusLineCommand(t *testing.T) {
	settings := map[string]json.RawMessage{}
	if err := setStatusLineCommand(settings, "my-command"); err != nil {
		t.Fatalf("setStatusLineCommand: %v", err)
	}
	cmd, ok := statusLineCommand(settings)
	if !ok || cmd != "my-command" {
		t.Errorf("statusLineCommand = (%q, %v), want (\"my-command\", true)", cmd, ok)
	}
}

func TestWriteSettingsCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "settings.json")
	settings := map[string]json.RawMessage{}
	_ = setStatusLineCommand(settings, "x")
	if err := writeSettings(path, settings); err != nil {
		t.Fatalf("writeSettings: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "\"x\"") {
		t.Errorf("written file %s does not contain expected command", data)
	}
}

func TestBackupSettingsCopiesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	backupSettings(&stderr, path)
	data, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(data) != `{"a":1}` {
		t.Errorf("backup contents = %q, want %q", data, `{"a":1}`)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on successful backup", stderr.String())
	}
}

func TestBackupSettingsMissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	backupSettings(&stderr, filepath.Join(dir, "settings.json"))
	if _, err := os.Stat(filepath.Join(dir, "settings.json.bak")); !os.IsNotExist(err) {
		t.Error("backup file was created for a settings.json that doesn't exist")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty when there's nothing to back up", stderr.String())
	}
}
