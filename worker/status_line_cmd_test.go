package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestResolveLoopePathIsAbsoluteAndExists(t *testing.T) {
	path, err := resolveLoopePath()
	if err != nil {
		t.Fatalf("resolveLoopePath: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("resolveLoopePath = %q, want an absolute path", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("resolveLoopePath = %q, which does not exist: %v", path, err)
	}
}

func TestResolveClaudeConfigDirOverride(t *testing.T) {
	cfg := &Config{ClaudeConfigDir: "/custom/claude"}
	dir, err := resolveClaudeConfigDir(cfg)
	if err != nil {
		t.Fatalf("resolveClaudeConfigDir: %v", err)
	}
	if dir != "/custom/claude" {
		t.Errorf("dir = %q, want %q", dir, "/custom/claude")
	}
}

func TestResolveClaudeConfigDirDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &Config{}
	dir, err := resolveClaudeConfigDir(cfg)
	if err != nil {
		t.Fatalf("resolveClaudeConfigDir: %v", err)
	}
	want := filepath.Join(home, ".claude")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

const testLoopePath = "/usr/local/bin/loope"
const testSettingsPath = "/home/user/.claude/settings.json"

func TestPlanInstallFreshNoStatusLine(t *testing.T) {
	settings := map[string]json.RawMessage{}
	result, err := planInstall(settings, testLoopePath, testSettingsPath)
	if err != nil {
		t.Fatalf("planInstall: %v", err)
	}
	if !result.changed {
		t.Fatal("changed = false, want true")
	}
	wantMsg := fmt.Sprintf("status-line: configured in %s (no previous statusLine was set, so your status line will show no visible output — set your own command later to see one alongside usage capture)", testSettingsPath)
	if result.message != wantMsg {
		t.Errorf("message = %q, want %q", result.message, wantMsg)
	}
	cmd, ok := statusLineCommand(settings)
	if !ok || cmd != bareCommand(testLoopePath) {
		t.Errorf("statusLine.command = %q, want %q", cmd, bareCommand(testLoopePath))
	}
}

func TestPlanInstallWrapsExistingCommand(t *testing.T) {
	settings := map[string]json.RawMessage{}
	_ = setStatusLineCommand(settings, "/path/to/real-statusline.sh")
	result, err := planInstall(settings, testLoopePath, testSettingsPath)
	if err != nil {
		t.Fatalf("planInstall: %v", err)
	}
	if !result.changed {
		t.Fatal("changed = false, want true")
	}
	wantMsg := fmt.Sprintf("status-line: wrapped existing statusLine command in %s", testSettingsPath)
	if result.message != wantMsg {
		t.Errorf("message = %q, want %q", result.message, wantMsg)
	}
	cmd, _ := statusLineCommand(settings)
	if cmd != wrappedCommand(testLoopePath, "/path/to/real-statusline.sh") {
		t.Errorf("statusLine.command = %q, want the wrapped form", cmd)
	}
}

func TestPlanInstallIdempotentOnBare(t *testing.T) {
	settings := map[string]json.RawMessage{}
	_ = setStatusLineCommand(settings, bareCommand(testLoopePath))
	result, err := planInstall(settings, testLoopePath, testSettingsPath)
	if err != nil {
		t.Fatalf("planInstall: %v", err)
	}
	if result.changed {
		t.Fatal("changed = true on a re-run, want false (idempotent)")
	}
	wantMsg := fmt.Sprintf("status-line: already configured (%s)", testSettingsPath)
	if result.message != wantMsg {
		t.Errorf("message = %q, want %q", result.message, wantMsg)
	}
}

func TestPlanInstallIdempotentOnWrapped(t *testing.T) {
	settings := map[string]json.RawMessage{}
	_ = setStatusLineCommand(settings, wrappedCommand(testLoopePath, "/path/to/real-statusline.sh"))
	result, err := planInstall(settings, testLoopePath, testSettingsPath)
	if err != nil {
		t.Fatalf("planInstall: %v", err)
	}
	if result.changed {
		t.Fatal("changed = true on a re-run, want false (idempotent)")
	}
}

func TestPlanRemoveSettingsFileMissing(t *testing.T) {
	settings := map[string]json.RawMessage{}
	result := planRemove(settings, testLoopePath, testSettingsPath, false)
	if result.changed || result.err {
		t.Fatalf("result = %+v, want changed=false err=false", result)
	}
	wantMsg := fmt.Sprintf("status-line: nothing to remove (%s does not exist)", testSettingsPath)
	if result.message != wantMsg {
		t.Errorf("message = %q, want %q", result.message, wantMsg)
	}
}

func TestPlanRemoveRestoresOriginalAfterWrap(t *testing.T) {
	settings := map[string]json.RawMessage{}
	_ = setStatusLineCommand(settings, wrappedCommand(testLoopePath, "/path/to/real-statusline.sh"))
	result := planRemove(settings, testLoopePath, testSettingsPath, true)
	if !result.changed || result.err {
		t.Fatalf("result = %+v, want changed=true err=false", result)
	}
	wantMsg := fmt.Sprintf("status-line: restored your original statusLine command in %s", testSettingsPath)
	if result.message != wantMsg {
		t.Errorf("message = %q, want %q", result.message, wantMsg)
	}
	cmd, ok := statusLineCommand(settings)
	if !ok || cmd != "/path/to/real-statusline.sh" {
		t.Errorf("statusLine.command = (%q, %v), want (\"/path/to/real-statusline.sh\", true)", cmd, ok)
	}
}

func TestPlanRemoveDeletesKeyAfterBareInstall(t *testing.T) {
	settings := map[string]json.RawMessage{}
	_ = setStatusLineCommand(settings, bareCommand(testLoopePath))
	result := planRemove(settings, testLoopePath, testSettingsPath, true)
	if !result.changed || result.err {
		t.Fatalf("result = %+v, want changed=true err=false", result)
	}
	wantMsg := fmt.Sprintf("status-line: removed from %s", testSettingsPath)
	if result.message != wantMsg {
		t.Errorf("message = %q, want %q", result.message, wantMsg)
	}
	if _, present := settings["statusLine"]; present {
		t.Error("statusLine key still present after remove")
	}
}

func TestPlanRemoveAlreadyClean(t *testing.T) {
	settings := map[string]json.RawMessage{}
	result := planRemove(settings, testLoopePath, testSettingsPath, true)
	if result.changed || result.err {
		t.Fatalf("result = %+v, want changed=false err=false", result)
	}
	wantMsg := fmt.Sprintf("status-line: already removed (%s)", testSettingsPath)
	if result.message != wantMsg {
		t.Errorf("message = %q, want %q", result.message, wantMsg)
	}
}

func TestPlanRemoveHandEditedErrors(t *testing.T) {
	settings := map[string]json.RawMessage{}
	_ = setStatusLineCommand(settings, "/some/hand/edited/command.sh")
	result := planRemove(settings, testLoopePath, testSettingsPath, true)
	if result.changed {
		t.Fatal("changed = true, want false — a hand-edited command must not be touched")
	}
	if !result.err {
		t.Fatal("err = false, want true — a hand-edited command must be reported as an error")
	}
	wantMsg := fmt.Sprintf("status-line: statusLine command in %s was not set by this tool (or was modified since) — edit it manually", testSettingsPath)
	if result.message != wantMsg {
		t.Errorf("message = %q, want %q", result.message, wantMsg)
	}
}

// newTestConfig writes a minimal valid loope.json (repoPath/repoSlug/workDir
// are LoadConfig's only required fields) with the given claudeConfigDir and
// returns its path.
func newTestConfig(t *testing.T, dir, claudeConfigDir string) string {
	t.Helper()
	cfg := fmt.Sprintf(`{
		"repoPath": %q,
		"repoSlug": "owner/repo",
		"workDir": %q,
		"claudeConfigDir": %q
	}`, dir, filepath.Join(dir, "work"), claudeConfigDir)
	path := filepath.Join(dir, "loope.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunStatusLineCmdMissingConfigExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runStatusLineCmd(nil, &stdout, &stderr)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Error("stderr is empty, want usage text")
	}
}

func TestRunStatusLineCmdFreshInstall(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	configPath := newTestConfig(t, dir, claudeDir)

	var stdout, stderr bytes.Buffer
	code := runStatusLineCmd([]string{"--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status-line: configured in") {
		t.Errorf("stdout = %q, want a \"configured in\" message", stdout.String())
	}

	settingsPath := filepath.Join(claudeDir, "settings.json")
	settings, existed, err := loadSettings(settingsPath)
	if err != nil || !existed {
		t.Fatalf("loadSettings after install: existed=%v err=%v", existed, err)
	}
	cmd, ok := statusLineCommand(settings)
	if !ok || !strings.Contains(cmd, "claude-usage-hook") {
		t.Errorf("statusLine.command = %q, want it to reference claude-usage-hook", cmd)
	}

	// Re-run must be a no-op (idempotent) and must not write a backup file,
	// since nothing changed.
	stdout.Reset()
	stderr.Reset()
	code = runStatusLineCmd([]string{"--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second run: code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already configured") {
		t.Errorf("second run stdout = %q, want \"already configured\"", stdout.String())
	}
}

func TestRunStatusLineCmdRemoveAfterInstall(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	configPath := newTestConfig(t, dir, claudeDir)

	var stdout, stderr bytes.Buffer
	if code := runStatusLineCmd([]string{"--config", configPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("install: code = %d, stderr = %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := runStatusLineCmd([]string{"--config", configPath, "--remove"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remove: code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status-line: removed from") {
		t.Errorf("stdout = %q, want a \"removed from\" message", stdout.String())
	}

	settingsPath := filepath.Join(claudeDir, "settings.json")
	settings, _, err := loadSettings(settingsPath)
	if err != nil {
		t.Fatalf("loadSettings after remove: %v", err)
	}
	if _, ok := statusLineCommand(settings); ok {
		t.Error("statusLine.command still present after remove")
	}
	if _, err := os.Stat(settingsPath + ".bak"); err != nil {
		t.Errorf("no backup file written before remove: %v", err)
	}
}

func TestRunStatusLineCmdDefaultClaudeConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"repoPath": %q, "repoSlug": "owner/repo", "workDir": %q}`, dir, filepath.Join(dir, "work"))
	configPath := filepath.Join(dir, "loope.json")
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runStatusLineCmd([]string{"--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Errorf("settings.json not written under default ~/.claude: %v", err)
	}
}
