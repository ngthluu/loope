package shared

import (
	"fmt"
	"path/filepath"

	wire "github.com/ngthluu/loope/shared"
)

// BranchName is the deterministic branch an issue's work ships on.
func BranchName(issueNum int) string {
	return fmt.Sprintf("ai/issue-%d", issueNum)
}

// WorktreePath is the deterministic worktree location for an issue, shared by
// Worktree.Create and every consumer that needs to find the worktree later, so
// all agree on where it lives.
func WorktreePath(workDir string, issueNum int) string {
	return filepath.Join(workDir, wire.IssueDirName(issueNum))
}

// IssueLogDir is the log dir for one issue under workDir.
func IssueLogDir(workDir string, issueNum int) string {
	return filepath.Join(workDir, "logs", wire.IssueDirName(issueNum))
}

// ParseIssueDirName re-exports the wire format's issue-dir parser so packages
// above this one never import the telemetry wire module directly.
func ParseIssueDirName(name string) (int, bool) {
	return wire.ParseIssueDirName(name)
}
