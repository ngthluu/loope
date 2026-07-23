package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Runner abstracts process execution so tests can fake git/gh/claude. env holds
// extra KEY=VALUE entries layered on top of the parent environment; pass nil to
// inherit it unchanged. stdin, when non-empty, is piped to the process's stdin
// (claude reads its prompt this way); pass "" to leave stdin closed.
type Runner interface {
	Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (stdout, stderr string, err error)
	// RunStream runs a process writing stdout to w as bytes arrive (for live
	// transcripts), rather than buffering it. It returns captured stderr and the
	// exit error. stdin/env/dir behave exactly as in Run.
	RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (stderr string, err error)
}

type execRunner struct{}

// runnerWaitDelay is how long a cancelled process has to exit on SIGTERM before
// exec escalates to SIGKILL. Ten seconds is enough for claude to flush its
// session transcript, which is what makes a stop resumable.
const runnerWaitDelay = 10 * time.Second

// gracefulCancel makes ctx cancellation send SIGTERM rather than the SIGKILL
// exec.CommandContext defaults to, escalating only if the process is still
// alive after runnerWaitDelay. This matters for `claude`: a SIGKILL mid-call
// loses the session transcript, so a stopped run could not be continued.
func gracefulCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = runnerWaitDelay
}

func (execRunner) Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	gracefulCancel(cmd)
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

func (execRunner) RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	gracefulCancel(cmd)
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
