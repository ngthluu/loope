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
