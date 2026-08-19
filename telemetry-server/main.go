package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// version is the loope-telemetry-server release version. It defaults to
// "dev" for local builds and is overridden at release time via
// -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	os.Exit(runTelemetryServerCmd(os.Args[1:]))
}

// parseTelemetryServerFlags parses the server's flags. token is required —
// an empty value is treated as a parse error so the caller exits before
// ever listening without auth configured.
func parseTelemetryServerFlags(args []string) (addr, token, dataDir string, err error) {
	fs := flag.NewFlagSet("loope-telemetry-server", flag.ContinueOnError)
	a := fs.String("addr", ":9090", "address to listen on")
	t := fs.String("token", "", "shared bearer token workers authenticate with (required)")
	d := fs.String("data-dir", "", "reserved for future persistence; created if given, but unused today")
	if perr := fs.Parse(args); perr != nil {
		return "", "", "", perr
	}
	if *t == "" {
		return "", "", "", fmt.Errorf("-token is required")
	}
	return *a, *t, *d, nil
}

// runTelemetryServerCmd is the whole server process: parse flags,
// start the fleet dashboard/ingest HTTP server, and block until a shutdown
// signal. Returns the process exit code.
func runTelemetryServerCmd(args []string) int {
	addr, token, dataDir, err := parseTelemetryServerFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry-server: %v\n", err)
		return 2
	}
	if dataDir != "" {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "telemetry-server: data-dir: %v\n", err)
			return 1
		}
	}

	srv, err := NewTelemetryServer(token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telemetry-server: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		httpSrv.Close()
	}()
	log.Printf("loope-telemetry-server %s on http://%s", version, addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "telemetry-server: %v\n", err)
		return 1
	}
	return 0
}
