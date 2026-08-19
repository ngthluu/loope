package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
