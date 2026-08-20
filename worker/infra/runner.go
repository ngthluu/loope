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

	"github.com/ngthluu/loope/worker/shared"
)

// ExecRunner is the real shared.Runner: it executes processes with os/exec.
type ExecRunner struct{}

var _ shared.Runner = ExecRunner{}

func (ExecRunner) Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return out.String(), errBuf.String(), err
}

func (ExecRunner) RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var errBuf bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return errBuf.String(), err
}
