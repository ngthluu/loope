package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// shellSingleQuote wraps s in single quotes for bash, escaping embedded
// single quotes with the standard quote-close, escaped-quote, quote-reopen
// dance, so s survives as one literal argument whatever it contains.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellSingleUnquote is the exact inverse of shellSingleQuote.
func shellSingleUnquote(s string) (string, bool) {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return "", false
	}
	return strings.ReplaceAll(s[1:len(s)-1], `'\''`, "'"), true
}

// hookMark closes the process-substitution that feeds loope's usage hook.
// "claude-usage-hook" is loope's own subcommand name, so its presence in this
// position is what marks a statusLine command as written by this tool —
// independent of which loope path wrote it (the binary moves on every
// Homebrew upgrade; recognition must survive that or install double-wraps
// and --remove strands the user's original command).
const hookMark = " claude-usage-hook)"

// wrappedCommand and bareCommand build the only two command strings this
// tool ever writes to settings.json's statusLine.command, and the only two
// shapes matchOurs recognizes as "ours". The user's original command is kept
// verbatim inside the bash -c string (it is already shell), while the whole
// inner command is single-quote-escaped, so an original containing single
// quotes — e.g. jq -r '.model.display_name' — round-trips intact, as does a
// loope path containing spaces.
func wrappedCommand(loopePath, original string) string {
	inner := fmt.Sprintf("tee >(%s claude-usage-hook) | %s", shellSingleQuote(loopePath), original)
	return "bash -c " + shellSingleQuote(inner)
}

func bareCommand(loopePath string) string {
	inner := fmt.Sprintf("tee >(%s claude-usage-hook) >/dev/null", shellSingleQuote(loopePath))
	return "bash -c " + shellSingleQuote(inner)
}

// matchOurs reports whether cmd is a command this tool wrote — for ANY loope
// path, current or stale (see hookMark). For a wrapped match, original is the
// user command it wraps — what status-line --remove restores settings.json
// to. It also recognizes the unquoted-path shape older loope versions wrote.
func matchOurs(cmd string) (isOurs, isWrapped bool, original string) {
	quoted, found := strings.CutPrefix(cmd, "bash -c ")
	if !found {
		return false, false, ""
	}
	inner, ok := shellSingleUnquote(quoted)
	if !ok {
		return false, false, ""
	}
	rest, found := strings.CutPrefix(inner, "tee >(")
	if !found {
		return false, false, ""
	}
	i := strings.Index(rest, hookMark)
	if i < 0 {
		return false, false, ""
	}
	tail := rest[i+len(hookMark):]
	if tail == " >/dev/null" {
		return true, false, ""
	}
	if after, found := strings.CutPrefix(tail, " | "); found && after != "" {
		return true, true, after
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
		if isOurs, isWrapped, original := matchOurs(command); isOurs {
			// Ours, but possibly written by a previous loope binary at another
			// path (Homebrew moves it every upgrade). Rewrite with the current
			// path instead of wrapping our own stale wrapper.
			want := bareCommand(loopePath)
			if isWrapped {
				want = wrappedCommand(loopePath, original)
			}
			if command == want {
				return installResult{
					message: fmt.Sprintf("status-line: already configured (%s)", settingsPath),
				}, nil
			}
			if err := setStatusLineCommand(settings, want); err != nil {
				return installResult{}, err
			}
			return installResult{
				message: fmt.Sprintf("status-line: updated loope path in %s", settingsPath),
				changed: true,
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
// `status-line --remove` does to settings, given whether settings.json
// existed at all, and mutates settings in place when changed is true. It
// needs no loope path: matchOurs recognizes our wrapper whichever binary
// wrote it, so --remove restores the original even after the binary moved.
func planRemove(settings map[string]json.RawMessage, settingsPath string, settingsExisted bool) removeResult {
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
	isOurs, isWrapped, original := matchOurs(command)
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
		result := planRemove(settings, settingsPath, existed)
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
