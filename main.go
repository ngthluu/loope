package main

import (
	"bytes"
	"context"
	"errors"
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

// cliMode is the run mode resolved from the parsed command-line flags.
type cliMode int

const (
	modeRun            cliMode = iota // start the daemon (config given)
	modeVersion                       // print version and exit, without reading config
	modeHelp                          // print usage and exit 0 (bare invocation / --help)
	modeDoctorNoConfig                // --doctor without --config: usage error, exit 2
)

// resolveMode maps the parsed flags to a run mode. --version wins over
// everything (the config is never read); a missing --config means help unless
// --doctor was asked for, which is a usage error.
func resolveMode(configPath string, showVersion, doctor bool) cliMode {
	switch {
	case showVersion:
		return modeVersion
	case configPath == "" && doctor:
		return modeDoctorNoConfig
	case configPath == "":
		return modeHelp
	default:
		return modeRun
	}
}

func main() {
	fs := flag.NewFlagSet("loope", flag.ContinueOnError)
	var help bytes.Buffer
	fs.SetOutput(&help)
	configPath := fs.String("config", "", "path to config file (required)")
	showVersion := fs.Bool("version", false, "print the loope version and exit")
	doctor := fs.Bool("doctor", false, "run preflight checks, print the report, and exit")
	fs.Usage = func() { usage(fs, &help) }

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Stdout.Write(help.Bytes()) // -h/--help: usage to stdout, exit 0
			os.Exit(0)
		}
		os.Stderr.Write(help.Bytes()) // genuine parse error: to stderr, exit 2
		os.Exit(2)
	}

	switch resolveMode(*configPath, *showVersion, *doctor) {
	case modeVersion:
		fmt.Println("loope", version)
		return
	case modeHelp:
		usage(fs, os.Stdout)
		os.Exit(0)
	case modeDoctorNoConfig:
		fmt.Fprintln(os.Stderr, "--config is required")
		usage(fs, os.Stderr)
		os.Exit(2)
	}

	cfg, err := LoadConfig(*configPath)
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
	if code, proceed := gate(ctx, os.Stderr, r, cfg, *doctor); !proceed {
		os.Exit(code)
	}

	o := &Orchestrator{cfg: cfg, runner: r, gh: NewGitHub(r, cfg),
		wt: &Worktree{runner: r, repoPath: cfg.RepoPath, retry: cfg.GitHubRetry.policy()}}

	// The daemon owns the workDir exclusively. The lock both stops a second
	// daemon from stealing live ai-wip work and proves any ai-wip issue found at
	// startup is an orphan from a crashed run — which is why the sweep only runs
	// when the lock is held.
	release, err := acquireLock(cfg.WorkDir)
	if err != nil {
		log.Fatal(err)
	}
	defer release()

	srv, err := NewServer(r, cfg)
	if err != nil {
		log.Fatalf("dashboard: %v", err)
	}
	srv.orch = o // enable the /stop and /continue mutation endpoints
	httpSrv := &http.Server{Addr: cfg.Addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		httpSrv.Close()
	}()
	// The dashboard is auxiliary: it runs in a goroutine and a listener error is
	// logged, never fatal, so the worker keeps shipping PRs.
	go func() {
		log.Printf("progress dashboard on http://%s (reading %s)", cfg.Addr, cfg.WorkDir)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("dashboard server stopped: %v", err)
		}
	}()

	runLoop(ctx, o, cfg, true /* sweep */)
}

// usage prints a one-line description and the flag defaults to w. It backs the
// FlagSet's Usage func (so -h/--help and parse errors reach it) and is also
// called directly for the bare-invocation and --doctor-without-config paths.
func usage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintln(w, "loope — autonomous GitHub issue pipeline daemon")
	fmt.Fprintf(w, "\nUsage:\n  %s --config <FILE>\n\nFlags:\n", fs.Name())
	fs.SetOutput(w)
	fs.PrintDefaults()
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
// Returns only when the context is cancelled, draining in-flight pipelines
// via o.Wait() on that path before returning.
func runLoop(ctx context.Context, o *Orchestrator, cfg *Config, sweep bool) {
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
		select {
		case <-ctx.Done():
			log.Println("shutting down: draining in-flight pipelines (signal again to force quit)")
			// Pipelines see the cancelled context and unwind through their
			// existing context.WithoutCancel cleanup paths, exactly as they did
			// when a Ctrl-C landed during the old in-cycle wg.Wait().
			o.Wait()
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
