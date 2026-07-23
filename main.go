package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
)

// version is the loope release version. It defaults to "dev" for local builds
// and is overridden at release time via -ldflags "-X main.version=<tag>".
var version = "dev"

// stopWatchInterval is how often a process running pipelines re-checks its live
// issues for a stop marker written by another process. One os.Stat per live
// pipeline per tick, so it stays cheap on a quiet daemon.
const stopWatchInterval = 2 * time.Second

// cliFlags is loope's whole command-line surface. It is declared in one place
// so the flag set can be built and asserted on without running main.
type cliFlags struct {
	configPath    *string
	addr          *string
	once          *bool
	serve         *bool
	showVersion   *bool
	rework        *int
	stopIssue     *int
	continueIssue *int
	doctor        *bool
}

func registerFlags(fs *flag.FlagSet) cliFlags {
	return cliFlags{
		configPath:    fs.String("config", "loope.json", "path to config file"),
		once:          fs.Bool("once", false, "run a single poll cycle and exit"),
		rework:        fs.Int("rework", 0, "resume a parked (ai-rework) issue by number, ship it, then exit"),
		stopIssue:     fs.Int("stop", 0, "stop work on issue N, preserving all progress, then exit"),
		continueIssue: fs.Int("continue", 0, "continue a stopped issue N from its persisted Claude session, then exit"),
		serve:         fs.Bool("serve", false, "run the read-only progress dashboard and exit on signal"),
		addr:          fs.String("addr", "localhost:8080", "address for -serve to listen on"),
		showVersion:   fs.Bool("version", false, "print the loope version and exit"),
		doctor:        fs.Bool("doctor", false, "run the preflight checks, print the report, and exit"),
	}
}

func main() {
	f := registerFlags(flag.CommandLine)
	flag.Parse()

	if *f.showVersion {
		fmt.Println("loope", version)
		return
	}

	cfg, err := LoadConfig(*f.configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Shutdown drains in-flight pipelines, which can take as long as the work
	// they are doing. Unregistering the handlers as soon as the first signal
	// lands restores default signal behaviour, so a second Ctrl-C (or SIGTERM)
	// terminates immediately instead of being swallowed by the handler that is
	// still installed until main returns.
	go func() {
		<-ctx.Done()
		stop()
	}()

	r := execRunner{}
	if code, proceed := gate(ctx, os.Stderr, r, cfg, *f.doctor); !proceed {
		os.Exit(code)
	}

	o := &Orchestrator{cfg: cfg, runner: r, gh: NewGitHub(r, cfg), baseCtx: ctx,
		wt: &Worktree{runner: r, repoPath: cfg.RepoPath, retry: cfg.GitHubRetry.policy()}}

	// Every mode that can drive a pipeline needs the stop watcher, so it is
	// started for all of them rather than for a list of modes someone has to
	// remember to extend — -once was exactly that oversight. Without it a
	// `loope -stop <N>` from another shell has no way to reach a run in this
	// process: that process can only write the marker, and nothing here would
	// ever read it, so the stop would relabel the issue underneath a claude
	// session that keeps running to completion. On a mode that runs no pipelines
	// the watcher iterates an empty registry and costs one wakeup every 2s.
	go o.watchStops(ctx, stopWatchInterval)

	if *f.rework > 0 {
		if err := o.Rework(ctx, *f.rework); err != nil {
			log.Fatalf("rework #%d: %v", *f.rework, err)
		}
		return
	}

	// -stop is safe against a live daemon: it writes the durable marker and
	// returns promptly, printing which path it took. It does not wait for the
	// running session to die.
	if *f.stopIssue > 0 {
		if err := o.Stop(ctx, *f.stopIssue); err != nil {
			log.Fatalf("stop #%d: %v", *f.stopIssue, err)
		}
		return
	}

	// -continue runs the resume synchronously and exits when the ticket ships or
	// parks, matching -rework. Two claude sessions in one worktree are prevented
	// by prepareContinue, which refuses an issue that is not stopped or that any
	// process already has a run on — a sharper test than "is a daemon up", and
	// one the dashboard's continue goes through as well.
	if *f.continueIssue > 0 {
		if err := o.Continue(ctx, *f.continueIssue); err != nil {
			log.Fatalf("continue #%d: %v", *f.continueIssue, err)
		}
		return
	}

	// Long-running modes own the workDir exclusively. The lock both stops a
	// second daemon from stealing live ai-wip work and proves any ai-wip issue
	// found at startup is an orphan from a crashed run — which is why the
	// sweep only runs when the lock is held.
	sweep := false
	if !*f.once {
		release, err := acquireLock(cfg.WorkDir)
		if err != nil {
			log.Fatal(err)
		}
		defer release()
		sweep = true
	}

	if *f.serve {
		srv, err := NewServer(r, cfg, o.controller())
		if err != nil {
			log.Fatalf("serve: %v", err)
		}
		httpSrv := &http.Server{Addr: *f.addr, Handler: srv.Handler()}
		go func() {
			<-ctx.Done()
			httpSrv.Close()
		}()
		// The dashboard is auxiliary: it runs in a goroutine and a listener
		// error is logged, never fatal, so the worker keeps shipping PRs.
		go func() {
			log.Printf("progress dashboard on http://%s (reading %s)", *f.addr, cfg.WorkDir)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("dashboard server stopped: %v", err)
			}
		}()
	}

	runLoop(ctx, o, cfg, *f.once, sweep)
}

// gate runs the preflight checks before any mode starts. It returns the process
// exit code and whether the caller should continue. The report is printed only
// when a required check failed or when -doctor asked for it, so a healthy
// daemon run adds no output.
func gate(ctx context.Context, w io.Writer, r Runner, cfg *Config, doctor bool) (exitCode int, proceed bool) {
	results := Preflight(ctx, r, cfg)
	failed := ReportPreflightFailedCount(results) > 0
	if doctor || failed {
		ReportPreflight(w, results)
	}
	if failed {
		return 1, false
	}
	if doctor {
		return 0, false
	}
	return 0, true
}

// runLoop drives the poll cycle forever: one startup orphan sweep (retried
// until it succeeds once), then auto-resume resumable parked issues and top the
// in-flight pipeline set up from the eligible queue, waiting one interval
// between cycles. Cycles no longer block on the pipelines they start, so both
// exit paths drain in-flight work with o.Wait() before returning — main's
// deferred workDir-lock release must not run while a pipeline is live. Every
// stage runs under guard, so a panic is one bad cycle, not a dead daemon.
// Returns when the context is cancelled or after a single cycle when once is set.
func runLoop(ctx context.Context, o *Orchestrator, cfg *Config, once, sweep bool) {
	log.Printf("watching %s for label %q every %ds", cfg.RepoSlug, cfg.EligibleLabel, cfg.PollIntervalSec)
	for {
		if sweep {
			if err := guard("orphan sweep", func() error { return o.SweepOrphans(ctx) }); err != nil {
				log.Printf("orphan sweep failed (will retry next cycle): %v", err)
			} else {
				sweep = false
			}
		}
		// Resumes run BEFORE new work: they continue an issue that already has a
		// worktree and session on disk, and they draw from the same slot budget.
		// With ProcessOnce first, a queue that always has eligible issues would
		// claim every slot every cycle and no parked issue would ever be resumed.
		if err := guard("auto-resume", func() error { return o.ResumeParked(ctx) }); err != nil {
			log.Printf("auto-resume error: %v", err)
		}
		if err := guard("cycle", func() error { return o.ProcessOnce(ctx) }); err != nil {
			log.Printf("cycle error: %v", err)
		}
		if once {
			// -once fills slots once and drains them; it does not top up as
			// pipelines complete.
			o.drain()
			return
		}
		select {
		case <-ctx.Done():
			log.Println("shutting down: draining in-flight pipelines (signal again to force quit)")
			// Pipelines see the cancelled context and unwind through their
			// existing context.WithoutCancel cleanup paths, exactly as they did
			// when a Ctrl-C landed during the old in-cycle wg.Wait().
			o.drain()
			return
		case <-time.After(time.Duration(cfg.PollIntervalSec) * time.Second):
		}
	}
}

// guard runs fn, converting a panic into an error so a bug in one stage can
// never kill the daemon. The stack is logged at recovery time.
func guard(what string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s panic: %v\n%s", what, r, debug.Stack())
			err = fmt.Errorf("%s panic: %v", what, r)
		}
	}()
	return fn()
}
