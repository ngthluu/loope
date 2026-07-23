package main

import (
	"bytes"
	"context"
	"errors"
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
// session transcript, which is what makes a stop resumable. A var, not a const,
// so tests can shorten it.
var runnerWaitDelay = 10 * time.Second

// gracefulCancel makes ctx cancellation send SIGTERM rather than the SIGKILL
// exec.CommandContext defaults to, escalating only if the process is still
// alive after runnerWaitDelay. This matters for `claude`: a SIGKILL mid-call
// loses the session transcript, so a stopped run could not be continued.
func gracefulCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = runnerWaitDelay
}

// settle maps the result of cmd.Wait to what the caller should see.
//
// WaitDelay bounds two separate waits, not one: a cancelled process that will
// not die, and a process that exited while a descendant still holds its stdout
// pipe open. The second case is the one that bites here — a claude tool call
// can leave an MCP server or a backgrounded shell holding that pipe — and exec
// reports it as ErrWaitDelay on an otherwise clean run. That is a report about
// a stray file descriptor, not a failed command: the command itself exited 0,
// and passing the error through would throw away its result and park the issue
// for rework.
//
// What the pardon does NOT promise is that stdout was complete: exec force-
// closes the pipes when the delay elapses, so a copy still in flight is cut
// short. It is the descendant that is still writing, not the command, so in
// practice what is lost is trailing noise — but when it is not, the truncated
// stream fails its own parse (Claude.Call finds no result line) and the run
// parks with that error instead of shipping something half-read.
//
// The pardon is confined to an uncancelled run. Once ctx is done the delay is
// the FIRST case — the process was signalled and exec force-closed the pipes on
// its way out — and a process that handles SIGTERM by exiting 0 would otherwise
// read as a clean success: a stopped claude session reported as a completed
// step, with the pipeline marching on to ship a ticket the operator halted.
// Anything else is returned untouched.
func settle(ctx context.Context, cmd *exec.Cmd, err error) error {
	if ctx.Err() != nil {
		return err
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return nil
	}
	return err
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
	return out.String(), errBuf.String(), settle(ctx, cmd, err)
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
	return errBuf.String(), settle(ctx, cmd, err)
}
