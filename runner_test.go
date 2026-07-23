package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerCapturesStdout(t *testing.T) {
	var r execRunner
	out, _, err := r.Run(context.Background(), "", nil, "", "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("stdout = %q, want %q", out, "hello")
	}
}

func TestExecRunnerReturnsErrorOnNonZeroExit(t *testing.T) {
	var r execRunner
	_, _, err := r.Run(context.Background(), "", nil, "", "false")
	if err == nil {
		t.Error("want error on non-zero exit, got nil")
	}
}

func TestExecRunnerPassesEnv(t *testing.T) {
	var r execRunner
	out, _, err := r.Run(context.Background(), "", []string{"LOOP_TEST_VAR=xyz"}, "", "sh", "-c", "printf %s \"$LOOP_TEST_VAR\"")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "xyz" {
		t.Errorf("env var = %q, want %q", out, "xyz")
	}
}

func TestExecRunnerRunsInDir(t *testing.T) {
	var r execRunner
	dir := t.TempDir()
	out, _, err := r.Run(context.Background(), dir, nil, "", "pwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.TrimSpace(out), dir) {
		t.Errorf("pwd = %q, want it to contain %q", out, dir)
	}
}

func TestExecRunnerPipesStdin(t *testing.T) {
	var r execRunner
	out, _, err := r.Run(context.Background(), "", nil, "hello from stdin", "cat")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello from stdin" {
		t.Errorf("stdin passthrough = %q, want %q", out, "hello from stdin")
	}
}

func TestExecRunnerStreamWritesStdout(t *testing.T) {
	var r execRunner
	var buf bytes.Buffer
	stderr, err := r.RunStream(context.Background(), "", nil, "", &buf, "printf", "a\nb\n")
	if err != nil {
		t.Fatalf("unexpected error: %v (stderr %q)", err, stderr)
	}
	if buf.String() != "a\nb\n" {
		t.Errorf("streamed stdout = %q, want %q", buf.String(), "a\nb\n")
	}
}

func TestExecRunnerStreamReturnsErrorOnNonZeroExit(t *testing.T) {
	var r execRunner
	var buf bytes.Buffer
	_, err := r.RunStream(context.Background(), "", nil, "", &buf, "false")
	if err == nil {
		t.Error("want error on non-zero exit, got nil")
	}
}

// shortWaitDelay shrinks the WaitDelay so the lingering-descendant tests below
// finish in well under a second instead of the production ten.
func shortWaitDelay(t *testing.T) {
	t.Helper()
	prev := runnerWaitDelay
	runnerWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { runnerWaitDelay = prev })
}

// A claude tool call can leave a descendant (an MCP server, a backgrounded
// shell) holding the stdout pipe after claude itself exits 0. exec reports that
// as ErrWaitDelay once WaitDelay expires; treating it as a failure would throw
// away a successful result and park the issue for rework.
func TestExecRunnerSucceedsWhenDescendantOutlivesACleanExit(t *testing.T) {
	shortWaitDelay(t)
	var r execRunner
	out, _, err := r.Run(context.Background(), "", nil, "", "sh", "-c", "sleep 5 & printf done")
	if err != nil {
		t.Fatalf("a command that exited 0 must not fail because a descendant held the pipe: %v", err)
	}
	if out != "done" {
		t.Errorf("stdout = %q, want %q", out, "done")
	}
}

func TestExecRunnerStreamSucceedsWhenDescendantOutlivesACleanExit(t *testing.T) {
	shortWaitDelay(t)
	var r execRunner
	var buf bytes.Buffer
	if _, err := r.RunStream(context.Background(), "", nil, "", &buf, "sh", "-c", "sleep 5 & printf done"); err != nil {
		t.Fatalf("a streamed command that exited 0 must not fail because a descendant held the pipe: %v", err)
	}
	if buf.String() != "done" {
		t.Errorf("streamed stdout = %q, want %q", buf.String(), "done")
	}
}

// The allowance is scoped to an UNCANCELLED run as well. Both of its
// conditions can hold on a stopped session — claude traps SIGTERM, flushes its
// transcript and exits 0, while a tool-call descendant still holds the pipe —
// and pardoning that would report a run the operator halted as a completed
// step, letting the pipeline march on to ship the ticket. (os/exec returns the
// context's error rather than ErrWaitDelay here, so this asserts the property
// rather than the mechanism.)
func TestExecRunnerCancelledRunNeverReadsAsSuccess(t *testing.T) {
	shortWaitDelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := execRunner{}.Run(ctx, "", nil, "", "sh", "-c",
			`trap 'exit 0' TERM; sleep 30 & wait`)
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled run that exited 0 with a lingering descendant must still report the cancellation")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled command did not exit")
	}
}

// The ErrWaitDelay allowance is scoped to a CLEAN exit: a non-zero exit with a
// lingering descendant is still a failure.
func TestExecRunnerStillFailsWhenExitIsNonZeroWithLingeringDescendant(t *testing.T) {
	shortWaitDelay(t)
	var r execRunner
	if _, _, err := r.Run(context.Background(), "", nil, "", "sh", "-c", "sleep 5 & exit 3"); err == nil {
		t.Error("want error on non-zero exit, got nil")
	}
}

// A cancelled command must be asked to exit with SIGTERM first, so claude gets
// a chance to flush its session transcript before it dies. The trap records
// that it arrived; a SIGKILL is untrappable and would leave no marker.
//
// Note: the exit error is deliberately NOT asserted to be nil. os/exec maps a
// cancelled command that exits cleanly to ctx.Err(), and suppressing that would
// make a stopped claude call look like a success to the pipeline.
func TestExecRunnerCancelSendsSIGTERM(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "sigterm")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = execRunner{}.Run(ctx, "", nil, "", "sh", "-c",
			`trap 'touch `+marker+`; exit 0' TERM; sleep 5 & wait`)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled command did not exit")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cancelled command was not sent SIGTERM (no marker): %v", err)
	}
}
