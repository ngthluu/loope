package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ngthluu/loope/worker/telemetry"
)

// runClaudeUsageHookCmd implements `loope claude-usage-hook`: it reads the
// statusLine JSON from stdin and overwrites ~/.claude/loope-usage.json with
// the parsed rate-limit snapshot. It prints nothing to stdout, so a user can
// tee the same stdin into it alongside their real statusLine command without
// affecting what Claude Code displays.
func runClaudeUsageHookCmd(stdin io.Reader) int {
	data, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: read stdin: %v\n", err)
		return 1
	}
	usage, err := telemetry.ParseClaudeStatusLine(data, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: parse: %v\n", err)
		return 1
	}
	path, err := telemetry.UsageHookFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	out, err := json.Marshal(usage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "claude-usage-hook: write %s: %v\n", path, err)
		return 1
	}
	return 0
}
