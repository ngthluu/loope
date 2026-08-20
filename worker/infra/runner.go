// Package infra holds the adapters behind the ports defined in worker/shared:
// process execution (ExecRunner), the GitHub CLI (GitHub → shared.CodeHost),
// git worktrees (Worktree → shared.Workspace), and the claude CLI
// (Claude → shared.Agent). Nothing above main constructs these types — the
// engine and dashboard receive them as ports by injection.
package infra

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/ngthluu/loope/worker/shared"
)

// ExecRunner is the real shared.Runner: it executes processes with os/exec.
type ExecRunner struct{}

var _ shared.Runner = ExecRunner{}

// command builds an exec.Cmd wired so that cancelling ctx reliably ends the
// whole process tree and Wait cannot hang on it:
//
//   - The child starts its own process group (Setpgid) and Cancel SIGKILLs
//     that group, not just the direct child. claude spawns grandchildren (tool
//     subprocesses, MCP servers) that would otherwise survive a Stop and keep
//     running against the worktree.
//   - WaitDelay bounds how long Wait blocks after the child exits/is killed
//     while it waits for the stdout/stderr pipes to close. Any grandchild that
//     inherited those pipes and was not killed keeps them open, and without a
//     WaitDelay Wait would block forever (Stop would appear to hang).
func command(ctx context.Context, dir string, env []string, stdin, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func (ExecRunner) Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (string, string, error) {
	cmd := command(ctx, dir, env, stdin, name, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.String(), errBuf.String(), err
}

func (ExecRunner) RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (string, error) {
	cmd := command(ctx, dir, env, stdin, name, args...)
	var errBuf bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return errBuf.String(), err
}
