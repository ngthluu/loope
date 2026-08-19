package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
