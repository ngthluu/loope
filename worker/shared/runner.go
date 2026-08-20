package shared

import (
	"context"
	"io"
)

// Runner abstracts process execution so tests can fake git/gh/claude. env holds
// extra KEY=VALUE entries layered on top of the parent environment; pass nil to
// inherit it unchanged. stdin, when non-empty, is piped to the process's stdin
// (claude reads its prompt this way); pass "" to leave stdin closed.
//
// Runner is a port: the real implementation is infra.ExecRunner, and every
// package that shells out receives a Runner by injection rather than
// constructing one.
type Runner interface {
	Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (stdout, stderr string, err error)
	// RunStream runs a process writing stdout to w as bytes arrive (for live
	// transcripts), rather than buffering it. It returns captured stderr and the
	// exit error. stdin/env/dir behave exactly as in Run.
	RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (stderr string, err error)
}
