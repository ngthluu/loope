package main

import (
	"context"
	"flag"
	"fmt"
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

	r := execRunner{}
	o := &Orchestrator{cfg: cfg, runner: r, gh: NewGitHub(r, cfg), baseCtx: ctx,
		wt: &Worktree{runner: r, repoPath: cfg.RepoPath, retry: cfg.GitHubRetry.policy()}}

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
	// parks, matching -rework. It refuses when a live daemon holds the lock and
	// the issue is WIP, since that would put two claude sessions in one worktree.
	if *f.continueIssue > 0 {
		n := *f.continueIssue
		if lockOwnerAlive(cfg.WorkDir) {
			state, err := o.currentStateLabel(ctx, n)
			if err != nil {
				log.Fatalf("continue #%d: %v", n, err)
			}
			if state == cfg.StateLabels.WIP {
				log.Fatalf("continue #%d: a daemon owns this workDir and #%d is %s — stop the daemon or use the dashboard", n, n, state)
			}
		}
		if err := o.Continue(ctx, n); err != nil {
			log.Fatalf("continue #%d: %v", n, err)
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

		// Only a lock-holding daemon owns live pipelines, so the watcher that
		// halts them on an out-of-band stop belongs here.
		go o.watchStops(ctx, 2*time.Second)
	}

	if *f.serve {
		srv, err := NewServer(r, cfg)
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
