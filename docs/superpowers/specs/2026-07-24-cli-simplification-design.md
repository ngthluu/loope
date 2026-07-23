# Design: Simplify the loope CLI (#21)

**Date:** 2026-07-24
**Branch:** `ai/issue-21`
**Status:** Approved (design)

## Problem

The `loope` binary exposes six run-affecting flags (`-config`, `-once`,
`-rework`, `-serve`, `-addr`, plus `-version`/`-doctor`). Several are redundant
or seldom used, and the flag surface is broader than the daemon actually needs.
The listen address is a flag even though every other runtime setting lives in
the config JSON. Running `./loope` with a bare, mistaken default (`loope.json`)
silently assumes a config file exists.

## Goal

Collapse the CLI to a single run mode plus three utility flags, driven entirely
by a **required** config file:

- `./loope --config <FILE>` — the only run mode.
- `./loope --doctor --config <FILE>` — preflight report, then exit.
- `./loope --version` — print version, then exit.
- `./loope --help` (and bare `./loope`) — print usage, then exit 0.

The dashboard listen address moves from the `-addr` flag into the config JSON.
The daemon always serves the dashboard (the old `-serve`-gated behavior becomes
unconditional). `-once` and the manual `-rework <N>` flags are removed.

## Non-goals

- No change to the poll loop, pipelines, slots, preflight checks, or dashboard
  rendering.
- No new flag-parsing library — the standard `flag` package already accepts
  both `-config` and `--config`, so it satisfies the "more standard `--`"
  requirement without a dependency.
- The internal auto-resume of parked `ai-rework` issues (`ResumeParked`, which
  calls the `Rework` method from the poll loop) is unchanged. Only the manual
  one-shot `-rework <N>` *entry point* is removed.

## Final CLI surface

| Invocation | Behavior | Exit |
|---|---|---|
| `./loope --config <FILE>` | Preflight gate → acquire workDir lock → run poll loop **and** dashboard (addr from config). Runs until signal. | 0 on clean shutdown; 1 if preflight fails |
| `./loope --doctor --config <FILE>` | Run preflight, print report, exit without starting the loop. | 0 healthy / 1 on required failure |
| `./loope --version` | Print `loope <version>`, exit. Config not read. | 0 |
| `./loope --help`, `./loope -h` | Print usage to stdout, exit. | 0 |
| `./loope` (no flags) | Print usage, exit (treated as an intentional help request). | 0 |
| `./loope --doctor` (no `--config`) | Error: `--config` is required, print usage. | 2 |

Single-dash forms (`-config`, `-doctor`, `-version`, `-h`) continue to work
because Go's `flag` package treats `-x` and `--x` identically. Help text and
docs present the double-dash forms.

### Removed flags

`-once`, `-rework`, `-serve`, `-addr` are deleted from the flag set.

## Design

### 1. Config — add the listen address (`config.go`)

Add one field to `Config`:

```go
Addr string `json:"addr"`
```

It is **optional**. `LoadConfig` sets the default `localhost:8080` in the same
literal that seeds the other defaults, so existing configs without `addr` behave
exactly as the old `-addr` default did. No new required-field check.

### 2. main.go control flow

```
1. Define flags:
     config  = flag.String("config", "", "path to config file (required)")
     version = flag.Bool("version", false, "print the loope version and exit")
     doctor  = flag.Bool("doctor", false, "run preflight checks, print the report, and exit")
   (flag.Usage stays the standard usage printer; -h/--help are handled by flag.)
2. flag.Parse()
   NOTE: the default `flag.CommandLine` is `ExitOnError`, so `-h`/`--help`
   prints usage and exits 2. To honor the exit-0 requirement, set
   `flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)` (or use a locally
   constructed FlagSet) and, when `flag.Parse()` returns `flag.ErrHelp`, print
   usage to stdout and `os.Exit(0)`. A genuine parse error still prints usage to
   stderr and exits 2.
3. if *version:            print "loope <version>"; return (exit 0)
4. if *config == "":
     - if *doctor:         Fprintln(os.Stderr, "--config is required"); flag.Usage(); os.Exit(2)
     - else:               flag.Usage() to stdout; os.Exit(0)   // bare ./loope and --help
5. cfg = LoadConfig(*config)  (fatal on error, as today)
6. signal context setup (unchanged)
7. gate(ctx, os.Stderr, r, cfg, *doctor)  — handles --doctor (proceed=false ⇒ exit code)
8. build Orchestrator (unchanged)
9. ALWAYS: acquire workDir lock (defer release); set sweep = true
10. ALWAYS: NewServer + start httpSrv on cfg.Addr in goroutines (moved out of the
    old `if *serve` block; listener errors stay logged-not-fatal)
11. runLoop(ctx, o, cfg, sweep)   // `once` parameter removed
```

Deleted from the current `main`:
- the `once`, `rework`, `serve`, `addr` flag definitions;
- the `if *rework > 0 { o.Rework(...) ; return }` branch;
- the `if !*once { acquireLock... ; sweep = true }` conditional (lock/sweep are
  now unconditional);
- the `if *serve { ... }` wrapper around the dashboard startup.

`flag.Usage`: the default Go usage printer writes to `os.Stderr`. For bare
`./loope`/`--help` we want stdout and exit 0. Implementation detail: print the
usage via `flag.CommandLine.SetOutput(os.Stdout); flag.Usage()` on the help path,
then `os.Exit(0)`. (The error path in step 4 leaves output on stderr and exits
2.) A brief top-line description ("loope — autonomous GitHub issue pipeline
daemon") is prepended via a custom `flag.Usage` function so the help is
self-explanatory.

### 3. `runLoop` signature (`main.go`)

Drop the `once bool` parameter and the `if once { o.Wait(); return }` block. The
loop now returns only when the context is cancelled (signal), draining in-flight
pipelines via `o.Wait()` on that path exactly as today.

### 4. Dashboard startup

Move the `NewServer`/`http.Server`/`ListenAndServe` goroutines out of the
`if *serve` block so they always run, listening on `cfg.Addr`. Behavior is
otherwise identical: a `NewServer` error is fatal (as it is today under
`-serve`), and a listener error is logged, never fatal.

## Ripple / files touched

| File | Change |
|---|---|
| `main.go` | Flag rework, help/version/required-config handling, unconditional lock+sweep and dashboard, `runLoop` signature. |
| `config.go` | Add `Addr` field + `localhost:8080` default in `LoadConfig`. |
| `main_test.go` | `TestRunLoopOnceDrainsInFlightPipelines`: drop the `once` arg; drive the drain-then-return via context cancellation instead of the removed `once=true` path. |
| `loope.json.example` | Add `"addr": "localhost:8080"`. |
| `launchd/com.loope.plist.example` | Program args become `--config /ABSOLUTE/PATH/TO/loope.json` (remove `-serve`; add `addr` to the JSON instead). |
| `README.md` | Rewrite the CLI/flags section and flag table: remove `-once`, `-serve`, `-addr`, and the manual `loope -rework <N>` instructions; state the dashboard is always served on `addr`; document `--config` (required), `--doctor`, `--version`, `--help`; note auto-resume replaces manual rework. |

## Testing

- **New/updated unit tests in `main_test.go`:**
  - `--version` prints the version and does not read config. (Testable by
    extracting the arg-dispatch into a small testable helper, or by asserting on
    a `run`-style function; keep it minimal — a helper `func resolveMode(args)`
    returning an enum {version, help, doctorNoConfig, run} that `main` switches
    on, unit-tested directly, is the cleanest seam.)
  - bare args ⇒ help/exit-0 path; `--doctor` without `--config` ⇒ usage error /
    exit 2; `--config x` ⇒ run mode.
  - `TestRunLoopOnceDrainsInFlightPipelines` updated to cancel the context and
    assert the loop drains the in-flight pipeline before returning.
- **config_test.go:** assert `Addr` defaults to `localhost:8080` when the JSON
  omits it, and is honored when present.
- **Existing gate tests** (`TestGate*`) are unaffected — `gate`'s signature is
  unchanged.
- `go build ./...` and `go test ./...` green.

## Rollout notes

Breaking change for anyone invoking `loope -once`, `loope -rework <N>`,
`loope -serve`, or `loope -addr`. The launchd example and README are updated in
the same change. Existing config files keep working (new `addr` field is
optional). The `loope.json` default path is gone: `--config` must be passed
explicitly.
