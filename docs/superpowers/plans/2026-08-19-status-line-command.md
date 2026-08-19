# `loope status-line` Command Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `loope status-line --config <FILE> [--remove]` subcommand that
automatically wires (or unwires) Claude Code's `statusLine` setting to also
capture usage data for the fleet telemetry dashboard, replacing the current
manual JSON-editing instructions in `docs/telemetry.md`.

**Architecture:** A new `worker/status_line_cmd.go` file holds pure,
directly-testable functions for template building/matching
(`wrappedCommand`/`bareCommand`/`matchOurs`), settings.json read/write
(`loadSettings`/`writeSettings`/`backupSettings`), path resolution
(`resolveLoopePath`/`resolveClaudeConfigDir`), and the install/remove
decision logic (`planInstall`/`planRemove`). `runStatusLineCmd` wires these
together for the actual subcommand, and is dispatched from the existing
`dispatchSubcommand` switch in `worker/main.go` exactly like
`claude-usage-hook` already is.

**Tech Stack:** Go 1.25 (module `github.com/ngthluu/loope/worker`), standard
library only (`encoding/json`, `flag`, `os`, `path/filepath`), Go's built-in
`testing` package with `t.TempDir()` fixtures — no new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-19-status-line-command-design.md`

## Global Constraints

- `claudeConfigDir` resolution: `Config.ClaudeConfigDir` from `--config`'s
  `loope.json` if set, else `~/.claude` (same field/default the daemon uses
  for `CLAUDE_CONFIG_DIR`).
- `--config` is required; missing it prints usage to stderr and exits 2.
- The `loope` path embedded in written commands must be absolute, resolved
  via `os.Executable()` + `filepath.EvalSymlinks()`.
- Only the top-level `statusLine` key in `settings.json` is read/written; all
  other keys pass through untouched via `map[string]json.RawMessage`.
- Exactly two command string shapes are ever written or recognized as "ours":
  - Wrapped: `` sh -c 'tee >($LOOPE claude-usage-hook) | <original>' ``
  - Bare: `` sh -c 'tee >($LOOPE claude-usage-hook) >/dev/null' ``
- A backup (`settings.json.bak`) is written best-effort before every mutating
  write, except when `--remove` finds nothing to change.
- Install/remove behavior, exact stdout/stderr messages, and exit codes must
  match the spec's "Install" and "Remove" sections precisely.

---

## File Structure

- **`worker/status_line_cmd.go`** (new) — everything the `status-line`
  subcommand needs: template helpers, settings I/O, path resolution, the
  install/remove decision functions, and `runStatusLineCmd` itself.
- **`worker/status_line_cmd_test.go`** (new) — unit tests for every function
  above, plus end-to-end tests of `runStatusLineCmd` against `t.TempDir()`
  fixtures.
- **`worker/main.go`** (modify) — `dispatchSubcommand` gains a `status-line`
  case; `usage()` gains a `Subcommands:` block.
- **`worker/main_test.go`** (modify) — one new test locking in that
  `status-line` is dispatched.
- **`docs/telemetry.md`** (modify) — "Capturing Claude usage (optional)"
  section leads with the new command.

---

### Task 1: Command templates and "is this ours?" matching

**Files:**
- Create: `worker/status_line_cmd.go`
- Test: `worker/status_line_cmd_test.go`

**Interfaces:**
- Produces: `wrappedCommand(loopePath, original string) string`,
  `bareCommand(loopePath string) string`,
  `matchOurs(cmd, loopePath string) (isOurs, isWrapped bool, original string)`
  — used by Task 4's `planInstall`/`planRemove`.

- [ ] **Step 1: Write the failing tests**

```go
package main

import "testing"

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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd worker && go test ./... -run 'TestWrappedAndBareCommand|TestMatchOurs' -v`
Expected: FAIL — `wrappedCommand`, `bareCommand`, `matchOurs` undefined
(package doesn't compile yet).

- [ ] **Step 3: Write the implementation**

Create `worker/status_line_cmd.go` with this content:

```go
package main

import (
	"fmt"
	"strings"
)

// wrappedCommand and bareCommand build the only two command strings this
// tool ever writes to settings.json's statusLine.command, and the only two
// shapes matchOurs recognizes as "ours".
func wrappedCommand(loopePath, original string) string {
	return fmt.Sprintf(`sh -c 'tee >(%s claude-usage-hook) | %s'`, loopePath, original)
}

func bareCommand(loopePath string) string {
	return fmt.Sprintf(`sh -c 'tee >(%s claude-usage-hook) >/dev/null'`, loopePath)
}

// wrappedPrefix is the fixed portion of a wrapped command up to (and
// including) the space before the original command it wraps.
func wrappedPrefix(loopePath string) string {
	return fmt.Sprintf(`sh -c 'tee >(%s claude-usage-hook) | `, loopePath)
}

// matchOurs reports whether cmd is a command this tool wrote for loopePath.
// For a wrapped match, original is the literal remainder of cmd after the
// fixed prefix, up to the closing quote — the command status-line --remove
// restores settings.json to.
func matchOurs(cmd, loopePath string) (isOurs, isWrapped bool, original string) {
	if cmd == bareCommand(loopePath) {
		return true, false, ""
	}
	prefix := wrappedPrefix(loopePath)
	if strings.HasPrefix(cmd, prefix) && strings.HasSuffix(cmd, "'") && len(cmd) > len(prefix) {
		return true, true, cmd[len(prefix) : len(cmd)-1]
	}
	return false, false, ""
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd worker && go test ./... -run 'TestWrappedAndBareCommand|TestMatchOurs' -v`
Expected: PASS (all 5 tests)

- [ ] **Step 5: Commit**

```bash
git add worker/status_line_cmd.go worker/status_line_cmd_test.go
git commit -m "feat: add status-line command templates and matcher"
```

---

### Task 2: settings.json read/write helpers

**Files:**
- Modify: `worker/status_line_cmd.go`
- Test: `worker/status_line_cmd_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `statusLineValue struct{ Type, Command string }`,
  `loadSettings(settingsPath string) (settings map[string]json.RawMessage, existed bool, err error)`,
  `writeSettings(settingsPath string, settings map[string]json.RawMessage) error`,
  `backupSettings(w io.Writer, settingsPath string)`,
  `statusLineCommand(settings map[string]json.RawMessage) (command string, ok bool)`,
  `setStatusLineCommand(settings map[string]json.RawMessage, command string) error`
  — used by Task 4's `planInstall`/`planRemove` and Task 5's `runStatusLineCmd`.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd worker && go test ./... -run 'TestLoadSettings|TestStatusLineCommandAbsent|TestSetAndReadBack|TestWriteSettings|TestBackupSettings' -v`
Expected: FAIL — functions undefined.

- [ ] **Step 3: Write the implementation**

Append to `worker/status_line_cmd.go` (also add imports
`encoding/json`, `io`, `os`, `path/filepath` to the existing import block):

```go
// statusLineValue is the shape this tool ever writes to the top-level
// "statusLine" key in settings.json.
type statusLineValue struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// loadSettings reads and parses settingsPath into a generic
// map[string]json.RawMessage, so keys this tool doesn't know about (and any
// of Claude Code's own settings) pass through untouched. A missing file is
// treated as an empty settings object, not an error.
func loadSettings(settingsPath string) (settings map[string]json.RawMessage, existed bool, err error) {
	data, err := os.ReadFile(settingsPath)
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	settings = map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, false, err
	}
	return settings, true, nil
}

// writeSettings marshals settings as indented JSON and writes it to
// settingsPath, creating the parent directory if it doesn't exist yet.
func writeSettings(settingsPath string, settings map[string]json.RawMessage) error {
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(settingsPath, out, 0o644)
}

// backupSettings copies settingsPath's current bytes to settingsPath+".bak"
// before it's mutated. Best-effort: a missing settingsPath (fresh install)
// is not an error, and any other failure is logged to w but never blocks
// the caller's write — cheap insurance, not a hard dependency.
func backupSettings(w io.Writer, settingsPath string) {
	data, err := os.ReadFile(settingsPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		fmt.Fprintf(w, "status-line: backup %s: %v\n", settingsPath, err)
		return
	}
	if err := os.WriteFile(settingsPath+".bak", data, 0o644); err != nil {
		fmt.Fprintf(w, "status-line: backup %s: %v\n", settingsPath, err)
	}
}

// statusLineCommand extracts settings["statusLine"].command, if present and
// non-empty.
func statusLineCommand(settings map[string]json.RawMessage) (command string, ok bool) {
	raw, present := settings["statusLine"]
	if !present {
		return "", false
	}
	var v statusLineValue
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	if v.Command == "" {
		return "", false
	}
	return v.Command, true
}

// setStatusLineCommand sets settings["statusLine"] to
// {"type":"command","command":command}, the only shape this tool writes.
func setStatusLineCommand(settings map[string]json.RawMessage, command string) error {
	raw, err := json.Marshal(statusLineValue{Type: "command", Command: command})
	if err != nil {
		return err
	}
	settings["statusLine"] = raw
	return nil
}
```

Add `"bytes"` to the test file's imports (used by the backup tests above).

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd worker && go test ./... -run 'TestLoadSettings|TestStatusLineCommandAbsent|TestSetAndReadBack|TestWriteSettings|TestBackupSettings' -v`
Expected: PASS (all 7 tests)

- [ ] **Step 5: Commit**

```bash
git add worker/status_line_cmd.go worker/status_line_cmd_test.go
git commit -m "feat: add settings.json read/write helpers for status-line"
```

---

### Task 3: Path resolution helpers

**Files:**
- Modify: `worker/status_line_cmd.go`
- Test: `worker/status_line_cmd_test.go`

**Interfaces:**
- Consumes: `Config.ClaudeConfigDir` (`worker/config.go:128`, already
  `~`-expanded by `LoadConfig`).
- Produces: `resolveLoopePath() (string, error)`,
  `resolveClaudeConfigDir(cfg *Config) (string, error)`
  — used by Task 5's `runStatusLineCmd`.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd worker && go test ./... -run 'TestResolveLoopePath|TestResolveClaudeConfigDir' -v`
Expected: FAIL — functions undefined.

- [ ] **Step 3: Write the implementation**

Append to `worker/status_line_cmd.go`:

```go
// resolveLoopePath returns the absolute path to the running loope binary,
// dereferencing symlinks so the command this tool writes keeps working
// regardless of the shell's PATH when Claude Code invokes it later —
// os.Executable()'s doc explicitly does not guarantee an unresolved symlink
// stays valid.
func resolveLoopePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve loope path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve loope path: %w", err)
	}
	return resolved, nil
}

// resolveClaudeConfigDir returns cfg.ClaudeConfigDir if set, else ~/.claude
// — the same field and default the daemon itself uses for CLAUDE_CONFIG_DIR
// (see worker/claude.go).
func resolveClaudeConfigDir(cfg *Config) (string, error) {
	if cfg.ClaudeConfigDir != "" {
		return cfg.ClaudeConfigDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd worker && go test ./... -run 'TestResolveLoopePath|TestResolveClaudeConfigDir' -v`
Expected: PASS (all 3 tests)

- [ ] **Step 5: Commit**

```bash
git add worker/status_line_cmd.go worker/status_line_cmd_test.go
git commit -m "feat: add path resolution helpers for status-line"
```

---

### Task 4: Install/remove decision logic

**Files:**
- Modify: `worker/status_line_cmd.go`
- Test: `worker/status_line_cmd_test.go`

**Interfaces:**
- Consumes: `wrappedCommand`, `bareCommand`, `matchOurs` (Task 1);
  `statusLineCommand`, `setStatusLineCommand` (Task 2).
- Produces: `installResult struct{ message string; changed bool }`,
  `planInstall(settings map[string]json.RawMessage, loopePath, settingsPath string) (installResult, error)`,
  `removeResult struct{ message string; changed bool; err bool }`,
  `planRemove(settings map[string]json.RawMessage, loopePath, settingsPath string, settingsExisted bool) removeResult`
  — used by Task 5's `runStatusLineCmd`.

This task is the heart of the spec's "Install" and "Remove" sections — it
directly encodes every branch of those two sections' numbered lists.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd worker && go test ./... -run 'TestPlanInstall|TestPlanRemove' -v`
Expected: FAIL — `planInstall`/`planRemove`/`installResult`/`removeResult`
undefined.

- [ ] **Step 3: Write the implementation**

Append to `worker/status_line_cmd.go`:

```go
// installResult carries what planInstall decided, so runStatusLineCmd can
// drive the backup/write/print I/O around a decision that's otherwise pure
// and directly testable.
type installResult struct {
	message string
	changed bool
}

// planInstall implements the spec's "Install" section: it decides what
// `status-line` (no --remove) does to settings, given the resolved loope
// path, and mutates settings in place when changed is true.
func planInstall(settings map[string]json.RawMessage, loopePath, settingsPath string) (installResult, error) {
	command, ok := statusLineCommand(settings)
	if ok {
		if isOurs, _, _ := matchOurs(command, loopePath); isOurs {
			return installResult{
				message: fmt.Sprintf("status-line: already configured (%s)", settingsPath),
			}, nil
		}
		if err := setStatusLineCommand(settings, wrappedCommand(loopePath, command)); err != nil {
			return installResult{}, err
		}
		return installResult{
			message: fmt.Sprintf("status-line: wrapped existing statusLine command in %s", settingsPath),
			changed: true,
		}, nil
	}
	if err := setStatusLineCommand(settings, bareCommand(loopePath)); err != nil {
		return installResult{}, err
	}
	return installResult{
		message: fmt.Sprintf("status-line: configured in %s (no previous statusLine was set, so your status line will show no visible output — set your own command later to see one alongside usage capture)", settingsPath),
		changed: true,
	}, nil
}

// removeResult carries what planRemove decided. err marks a message that
// belongs on stderr with exit code 1, as opposed to an informational exit-0
// message.
type removeResult struct {
	message string
	changed bool
	err     bool
}

// planRemove implements the spec's "Remove" section: it decides what
// `status-line --remove` does to settings, given the resolved loope path and
// whether settings.json existed at all, and mutates settings in place when
// changed is true.
func planRemove(settings map[string]json.RawMessage, loopePath, settingsPath string, settingsExisted bool) removeResult {
	if !settingsExisted {
		return removeResult{
			message: fmt.Sprintf("status-line: nothing to remove (%s does not exist)", settingsPath),
		}
	}
	command, ok := statusLineCommand(settings)
	if !ok {
		return removeResult{
			message: fmt.Sprintf("status-line: already removed (%s)", settingsPath),
		}
	}
	isOurs, isWrapped, original := matchOurs(command, loopePath)
	if !isOurs {
		return removeResult{
			message: fmt.Sprintf("status-line: statusLine command in %s was not set by this tool (or was modified since) — edit it manually", settingsPath),
			err:     true,
		}
	}
	if isWrapped {
		_ = setStatusLineCommand(settings, original)
		return removeResult{
			message: fmt.Sprintf("status-line: restored your original statusLine command in %s", settingsPath),
			changed: true,
		}
	}
	delete(settings, "statusLine")
	return removeResult{
		message: fmt.Sprintf("status-line: removed from %s", settingsPath),
		changed: true,
	}
}
```

`setStatusLineCommand`'s error return is ignored in the `isWrapped` branch
because `original` is a plain string extracted from already-valid JSON —
`json.Marshal` on a `statusLineValue` cannot fail.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd worker && go test ./... -run 'TestPlanInstall|TestPlanRemove' -v`
Expected: PASS (all 9 tests)

- [ ] **Step 5: Commit**

```bash
git add worker/status_line_cmd.go worker/status_line_cmd_test.go
git commit -m "feat: add status-line install/remove decision logic"
```

---

### Task 5: `runStatusLineCmd` — wire it into a runnable subcommand

**Files:**
- Modify: `worker/status_line_cmd.go`
- Test: `worker/status_line_cmd_test.go`

**Interfaces:**
- Consumes: `resolveLoopePath`, `resolveClaudeConfigDir` (Task 3);
  `loadSettings`, `writeSettings`, `backupSettings` (Task 2); `planInstall`,
  `planRemove` (Task 4); `LoadConfig(path string) (*Config, error)`
  (`worker/config.go:139`).
- Produces: `runStatusLineCmd(args []string, stdout, stderr io.Writer) int`
  — used by Task 6's `dispatchSubcommand`.

- [ ] **Step 1: Write the failing tests**

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd worker && go test ./... -run TestRunStatusLineCmd -v`
Expected: FAIL — `runStatusLineCmd` undefined.

- [ ] **Step 3: Write the implementation**

Append to `worker/status_line_cmd.go` (add `"flag"` to the import block):

```go
// runStatusLineCmd implements `loope status-line`: it wires (or, with
// --remove, unwires) Claude Code's statusLine setting to also capture usage
// for the fleet telemetry dashboard. See docs/telemetry.md.
func runStatusLineCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status-line", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to loope config file (required)")
	remove := fs.Bool("remove", false, "remove the statusLine wiring installed by a previous run")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: loope status-line --config <FILE> [--remove]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *configPath == "" {
		fs.Usage()
		return 2
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "status-line: %v\n", err)
		return 1
	}
	claudeConfigDir, err := resolveClaudeConfigDir(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "status-line: %v\n", err)
		return 1
	}
	loopePath, err := resolveLoopePath()
	if err != nil {
		fmt.Fprintf(stderr, "status-line: %v\n", err)
		return 1
	}
	settingsPath := filepath.Join(claudeConfigDir, "settings.json")

	settings, existed, err := loadSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "status-line: %v\n", err)
		return 1
	}

	if *remove {
		result := planRemove(settings, loopePath, settingsPath, existed)
		if result.changed {
			backupSettings(stderr, settingsPath)
			if err := writeSettings(settingsPath, settings); err != nil {
				fmt.Fprintf(stderr, "status-line: %v\n", err)
				return 1
			}
		}
		if result.err {
			fmt.Fprintln(stderr, result.message)
			return 1
		}
		fmt.Fprintln(stdout, result.message)
		return 0
	}

	result, err := planInstall(settings, loopePath, settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "status-line: %v\n", err)
		return 1
	}
	if result.changed {
		backupSettings(stderr, settingsPath)
		if err := writeSettings(settingsPath, settings); err != nil {
			fmt.Fprintf(stderr, "status-line: %v\n", err)
			return 1
		}
	}
	fmt.Fprintln(stdout, result.message)
	return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd worker && go test ./... -run TestRunStatusLineCmd -v`
Expected: PASS (all 4 tests)

- [ ] **Step 5: Run the full package test suite**

Run: `cd worker && go build ./... && go test ./... -v 2>&1 | tail -60`
Expected: PASS — no regressions, and every test added in Tasks 1-5 present
and green.

- [ ] **Step 6: Commit**

```bash
git add worker/status_line_cmd.go worker/status_line_cmd_test.go
git commit -m "feat: implement runStatusLineCmd for loope status-line"
```

---

### Task 6: Dispatch `status-line` and document it in `--help`

**Files:**
- Modify: `worker/main.go:170-183` (`dispatchSubcommand`), `worker/main.go:188-193` (`usage`)
- Test: `worker/main_test.go`

**Interfaces:**
- Consumes: `runStatusLineCmd(args []string, stdout, stderr io.Writer) int` (Task 5).

- [ ] **Step 1: Write the failing test**

Add to `worker/main_test.go`:

```go
func TestDispatchSubcommandStatusLineMissingConfigExits2(t *testing.T) {
	code, handled := dispatchSubcommand([]string{"loope", "status-line"}, strings.NewReader(""))
	if !handled {
		t.Fatal("expected status-line to be handled")
	}
	if code != 2 {
		t.Errorf("code = %d, want 2 (missing required --config)", code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd worker && go test ./... -run TestDispatchSubcommandStatusLine -v`
Expected: FAIL — `dispatchSubcommand` doesn't yet recognize `status-line`,
so `handled` is `false`.

- [ ] **Step 3: Wire the dispatch and usage text**

In `worker/main.go`, extend the `dispatchSubcommand` switch:

```go
	switch args[1] {
	case "claude-usage-hook":
		return runClaudeUsageHookCmd(stdin), true
	case "status-line":
		return runStatusLineCmd(args[2:], os.Stdout, os.Stderr), true
	}
```

And extend `usage`:

```go
func usage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintln(w, "loope — autonomous GitHub issue pipeline daemon")
	fmt.Fprintf(w, "\nUsage:\n  %s --config <FILE>\n\nFlags:\n", fs.Name())
	fs.SetOutput(w)
	fs.PrintDefaults()
	fmt.Fprint(w, `
Subcommands:
  status-line --config <FILE> [--remove]
        wire (or unwire) Claude Code's statusLine to capture usage for the
        fleet dashboard; see docs/telemetry.md
  claude-usage-hook
        internal: reads statusLine JSON from stdin, writes the usage
        snapshot loope reads back (wired automatically by status-line)
`)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd worker && go test ./... -run TestDispatchSubcommandStatusLine -v`
Expected: PASS

- [ ] **Step 5: Run the full worker test suite and manually check `--help`**

Run: `cd worker && go build ./... && go test ./... -v 2>&1 | tail -80`
Expected: PASS, no regressions.

Run: `cd worker && go run . --help`
Expected: output includes the new `Subcommands:` block with both
`status-line` and `claude-usage-hook` entries.

- [ ] **Step 6: Commit**

```bash
git add worker/main.go worker/main_test.go
git commit -m "feat: dispatch loope status-line and document it in --help"
```

---

### Task 7: Update `docs/telemetry.md`

**Files:**
- Modify: `docs/telemetry.md:72-95` ("Capturing Claude usage (optional)" section)

**Interfaces:** none (documentation only).

- [ ] **Step 1: Rewrite the section**

Replace lines 72-95 of `docs/telemetry.md` (from `## Capturing Claude usage
(optional)` through the end of that section) with:

```markdown
## Capturing Claude usage (optional)

The 5-hour/7-day usage numbers come from the JSON Claude Code feeds to a
configured `statusLine` command. Wire it up with:

```bash
loope status-line --config /path/to/loope.json
```

This wraps (or sets, if none exists) your `~/.claude/settings.json`
`statusLine` command so it also feeds `loope claude-usage-hook`, without
disturbing whatever your existing statusline already shows. Run it again
with `--remove` to undo. Under the hood, this produces (or you can wire up
by hand instead, for example to review or tweak the wrapping) something like:

```bash
# ~/.claude/settings.json
"statusLine": {
  "type": "command",
  "command": "sh -c 'tee >(loope claude-usage-hook) | /path/to/your/real-statusline.sh'"
}
```

`loope claude-usage-hook` writes the latest rate-limit snapshot to
`~/.claude/loope-usage.json` and prints nothing, so it never affects what your
real statusline displays. If this file is missing, or its capture is older
than 30 minutes, the dashboard shows "usage: unknown" for that worker rather
than a stale or fabricated number — whether headless `claude -p` runs (how
loope's own pipeline steps invoke Claude) trigger the statusLine hook the same
way interactive sessions do is unconfirmed; the degraded "unknown" state is
what you'll see until that's verified on your setup.
```

- [ ] **Step 2: Proofread the rendered section**

Run: `sed -n '70,100p' docs/telemetry.md`
Expected: the `status-line` command appears first, followed by one
explanatory sentence, followed by the manual example under an "under the
hood" framing, followed by the unchanged final paragraph about
`loope-usage.json` and the "usage: unknown" fallback.

- [ ] **Step 3: Commit**

```bash
git add docs/telemetry.md
git commit -m "docs: lead Claude usage capture with loope status-line"
```

---

## Self-Review Notes

- **Spec coverage:** command surface (Task 6), path resolution (Task 3),
  settings.json read/write incl. backup (Task 2), template
  recognition (Task 1), install behavior incl. all 3 branches (Task 4/5),
  remove behavior incl. all 4 branches (Task 4/5), help text (Task 6), docs
  (Task 7), and every listed unit test scenario (Tasks 1-5) are each covered
  by a task.
- **Assumption called out:** the spec doesn't specify `--remove`'s exit code
  when nothing needs to change (already-clean case). This plan treats it as
  exit 0 with an informational message, consistent with the "already
  configured" install no-op and with "Exit 0 on success (or the
  already-removed no-op above)" in the spec's Remove section.
- **Assumption called out:** `setStatusLineCommand`'s error is ignored in
  `planRemove`'s wrapped branch (Task 4) since `original` is always a valid
  Go string and `json.Marshal` on `statusLineValue` cannot fail for it —
  documented inline in Task 4's implementation step rather than left
  unexplained.
- **Type consistency:** `runStatusLineCmd(args []string, stdout, stderr
  io.Writer) int` (Task 5) matches its call site in Task 6's
  `dispatchSubcommand` (`os.Stdout, os.Stderr` in that argument order).
  `installResult`/`removeResult`/`planInstall`/`planRemove` signatures are
  identical between their Task 4 definition and Task 5's usage.
