package shared

import "context"

// This file defines the ports the engine (and dashboard) depend on. The
// concrete adapters live in worker/infra; main is the only place a port meets
// its implementation. The engine never imports infra — swapping GitHub for
// another code host, or the claude CLI for another agent, means writing a new
// adapter and changing one wiring line in main.

// CodeHost is the issue-tracker/code-review surface of the loop: everything
// the daemon reads from and writes to the hosting provider. infra.GitHub (the
// gh CLI adapter) implements it.
type CodeHost interface {
	ListEligibleIssues(ctx context.Context, label string) ([]Issue, error)
	ListIssuesWithLabel(ctx context.Context, label string) ([]Issue, error)
	AddLabel(ctx context.Context, num int, label string) error
	RemoveLabel(ctx context.Context, num int, label string) error
	SwapLabels(ctx context.Context, num int, remove, add string) error
	Comment(ctx context.Context, num int, body string) error
	CloseIssue(ctx context.Context, num int) error
	FetchIssueContent(ctx context.Context, num int) (string, error)
	IssueTitle(ctx context.Context, num int) (string, error)
	UATSurfaces(ctx context.Context, n int) ([]string, error)
	CreatePR(ctx context.Context, branch, title, body string) (string, error)
	PRURLForBranch(ctx context.Context, branch string) (string, error)
	PRNumberForBranch(ctx context.Context, branch string) (int, error)
	ReviewComment(ctx context.Context, prNumber int, body string) error
}

// Workspace is the version-control workspace surface: worktree lifecycle and
// branch operations for one repository. infra.Worktree (the git adapter)
// implements it.
type Workspace interface {
	DefaultBranch(ctx context.Context) (string, error)
	Create(ctx context.Context, workDir string, issueNum int, baseBranch string) (string, error)
	Fetch(ctx context.Context) error
	Merge(ctx context.Context, wtPath, ref string) error
	HasUnmergedPaths(ctx context.Context, wtPath string) (bool, error)
	MergeInProgress(ctx context.Context, wtPath string) bool
	Push(ctx context.Context, wtPath, branch string) error
	CommitCount(ctx context.Context, wtPath, baseBranch string) (int, error)
}

// Agent is one issue's coding-agent session surface: run a call, persist the
// issue snapshot, and checkpoint stages onto the session chain. infra.Claude
// (the claude CLI adapter) implements it. An Agent is scoped to one log dir —
// the engine obtains one per issue through its injected factory.
type Agent interface {
	Call(ctx context.Context, call ClaudeCall) (*ClaudeResult, error)
	RecordSnapshot(content string)
	CheckpointStage(kind, stage, artifact string)
	// SetKind stamps the resolved pipeline kind ("bug"/"feature") onto the
	// session-chain head once the merged entry session's outcome is known —
	// see shared.SetHeadKind.
	SetKind(kind string)
	// LogDir is the issue log directory this agent writes its artifacts to —
	// also where the session chain and state markers live.
	LogDir() string
}
