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
	"path/filepath"
	"syscall"
	"time"

	"github.com/ngthluu/loope/worker/engine"
	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/telemetry"
	"github.com/ngthluu/loope/worker/web"
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
	modeDoctorNoConfig                // --doctor without --config: run config-less preflight and exit
)

// resolveMode maps the parsed flags to a run mode. --version wins over
// everything (the config is never read); a missing --config means help unless
// --doctor was asked for, which runs the environment checks without a config.
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
	if code, handled := dispatchSubcommand(os.Args, os.Stdin); handled {
		os.Exit(code)
	}

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
		// -doctor without -config: run the environment checks that need no
		// config (binaries, auth, superpowers) and skip the repo-specific
		// ones. Lets a user verify their machine before writing a config.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		code, _ := gate(ctx, os.Stderr, infra.ExecRunner{}, nil, true)
		os.Exit(code)
	}

	cfg, err := shared.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	daemonLogPath := filepath.Join(cfg.WorkDir, "logs", "daemon.log")
	logFile, err := NewRotatingFile(daemonLogPath, rotatingFileMaxBytes)
	if err != nil {
		log.Fatalf("daemon log: %v", err)
	}
	defer logFile.Close()
	log.SetOutput(io.MultiWriter(os.Stderr, logFile))

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

	// main is the composition root: every port defined in worker/shared meets
	// its infra adapter exactly here, and nowhere else.
	r := infra.ExecRunner{}
	if code, proceed := gate(ctx, os.Stderr, r, cfg, *doctor); !proceed {
		os.Exit(code)
	}

	gh := infra.NewGitHub(r, cfg)
	o := engine.NewOrchestrator(engine.Deps{
		Cfg:       cfg,
		Host:      gh,
		Workspace: infra.NewWorktree(r, cfg),
		NewAgent: func(logDir string) shared.Agent {
			return infra.NewClaude(r, logDir, cfg.ClaudeConfigDir)
		},
		DownloadImages: func(ctx context.Context, content, destDir string) string {
			return infra.DownloadIssueImages(ctx, r, content, destDir)
		},
	})

	// The daemon owns the workDir exclusively. The lock both stops a second
	// daemon from stealing live ai-wip work and proves any ai-wip issue found at
	// startup is an orphan from a crashed run — which is why the sweep only runs
	// when the lock is held.
	release, err := acquireLock(cfg.WorkDir)
	if err != nil {
		log.Fatal(err)
	}
	defer release()

	srv, err := web.NewServer(r, cfg, gh, o) // a non-nil orchestrator enables /stop and /continue
	if err != nil {
		log.Fatalf("dashboard: %v", err)
	}
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
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

	if cfg.Telemetry != nil {
		exp := telemetry.NewTelemetryExporter(cfg, daemonLogPath, version)
		go exp.Run(ctx)
	}

	engine.RunLoop(ctx, o, cfg, true /* sweep */)
}

// dispatchSubcommand handles the `claude-usage-hook` subcommand, which takes
// over the whole process instead of running the daemon. handled is false for
// every other invocation (the normal --config-driven daemon, bare
// --version/--help), so main falls through to its existing flag-based
// dispatch unchanged. The fleet telemetry server is its own binary since the
// monorepo split: loope-telemetry-server (telemetry-server/).
func dispatchSubcommand(args []string, stdin io.Reader) (code int, handled bool) {
	if len(args) < 2 {
		return 0, false
	}
	switch args[1] {
	case "claude-usage-hook":
		return runClaudeUsageHookCmd(stdin), true
	case "status-line":
		return runStatusLineCmd(args[2:], os.Stdout, os.Stderr), true
	}
	return 0, false
}

// usage prints a one-line description and the flag defaults to w. It backs the
// FlagSet's Usage func (so -h/--help and parse errors reach it) and is also
// called directly for the bare-invocation and --doctor-without-config paths.
func usage(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprintln(w, "loope — autonomous GitHub issue pipeline daemon")
	fmt.Fprintf(w, "\nUsage:\n  %s --config <FILE>\n\nFlags:\n", fs.Name())
	fs.SetOutput(w)
	fs.PrintDefaults()
	fmt.Fprint(w, `
Subcommands:
  status-line --config <FILE> [--remove]
        wire (or unwire) Claude Code's statusLine to capture usage for the
        fleet dashboard; see docs/telemetry.md
  claude-usage-hook
        internal: reads statusLine JSON from stdin, writes the usage
        snapshot loope reads back (wired automatically by status-line)
`)
}

// gate runs the preflight checks before any mode starts. It returns the process
// exit code and whether the caller should continue. The report is printed only
// when a required check failed or when -doctor asked for it, so a healthy
// daemon run adds no output.
func gate(ctx context.Context, w io.Writer, r shared.Runner, cfg *shared.Config, doctor bool) (exitCode int, proceed bool) {
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
