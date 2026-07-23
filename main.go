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

func main() {
	configPath := flag.String("config", "loope.json", "path to config file")
	once := flag.Bool("once", false, "run a single poll cycle and exit")
	rework := flag.Int("rework", 0, "resume a parked (ai-rework) issue by number, ship it, then exit")
	serve := flag.Bool("serve", false, "run the read-only progress dashboard and exit on signal")
	addr := flag.String("addr", "localhost:8080", "address for -serve to listen on")
	showVersion := flag.Bool("version", false, "print the loope version and exit")
	doctor := flag.Bool("doctor", false, "run the preflight checks, print the report, and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("loope", version)
		return
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := execRunner{}
	if code, proceed := gate(ctx, os.Stderr, r, cfg, *doctor); !proceed {
		os.Exit(code)
	}

	o := &Orchestrator{cfg: cfg, runner: r, gh: NewGitHub(r, cfg),
		wt: &Worktree{runner: r, repoPath: cfg.RepoPath, retry: cfg.GitHubRetry.policy()}}

	if *rework > 0 {
		if err := o.Rework(ctx, *rework); err != nil {
			log.Fatalf("rework #%d: %v", *rework, err)
		}
		return
	}

	// Long-running modes own the workDir exclusively. The lock both stops a
	// second daemon from stealing live ai-wip work and proves any ai-wip issue
	// found at startup is an orphan from a crashed run — which is why the
	// sweep only runs when the lock is held.
	sweep := false
	if !*once {
		release, err := acquireLock(cfg.WorkDir)
		if err != nil {
			log.Fatal(err)
		}
		defer release()
		sweep = true
	}

	if *serve {
		srv, err := NewServer(r, cfg)
		if err != nil {
			log.Fatalf("serve: %v", err)
		}
		httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler()}
		go func() {
			<-ctx.Done()
			httpSrv.Close()
		}()
		// The dashboard is auxiliary: it runs in a goroutine and a listener
		// error is logged, never fatal, so the worker keeps shipping PRs.
		go func() {
			log.Printf("progress dashboard on http://%s (reading %s)", *addr, cfg.WorkDir)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("dashboard server stopped: %v", err)
			}
		}()
	}

	runLoop(ctx, o, cfg, *once, sweep)
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
// until it succeeds once), then process eligible issues and auto-resume
// resumable parked ones, waiting one interval between cycles. Every stage runs
// under guard, so a panic is one bad cycle, not a dead daemon. Returns when the
// context is cancelled or after a single cycle when once is set.
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
		if err := guard("cycle", func() error { return o.ProcessOnce(ctx) }); err != nil {
			log.Printf("cycle error: %v", err)
		}
		if err := guard("auto-resume", func() error { return o.ResumeParked(ctx) }); err != nil {
			log.Printf("auto-resume error: %v", err)
		}
		if once {
			return
		}
		select {
		case <-ctx.Done():
			log.Println("shutting down")
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
