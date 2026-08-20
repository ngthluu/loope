# Worker package split: ports-and-adapters layout

**Date:** 2026-08-20
**Status:** Implemented (2026-08-20)

## Goal

Reorganize the flat `worker/` Go package (`package main`, ~33 source files +
39 test files) into coarse sub-packages with tests co-located next to their
source, extract the shared domain core into `worker/shared`, and invert the
domain→infrastructure dependency so orchestration code depends only on
interfaces (ports) while concrete integrations (gh CLI, git, Claude CLI)
become swappable adapters. A future provider (e.g. a different code host or a
different agent CLI) must be addable by writing a new adapter and changing
one wiring line in `main`.

Behavior is unchanged. Small cleanups forced by the split (exporting
identifiers, adding constructors, folding existing partial interfaces into
ports) are in scope; feature changes are not.

## Package layout

```
worker/                   package main — composition root
worker/shared/            domain core + ports (interfaces)
worker/infra/             adapters implementing the ports
worker/engine/            orchestration: Orchestrator, slots, pipelines,
                          triage, codereview, uat, resume, prompts
worker/web/               dashboard: serve, render, tracker, embedded assets
worker/telemetry/         exporter + log tailer
worker/testkit/           exported fakes for the ports (test-only helpers)
```

Module path stays `github.com/ngthluu/loope/worker`; sub-packages import as
`github.com/ngthluu/loope/worker/shared`, etc. The repo-root `shared/` module
(telemetry wire types) is unrelated and keeps its name; imports distinguish
them (`loope/shared` vs `loope/worker/shared`).

### File → package mapping

| Package | Files (current names) |
|---|---|
| `worker` (main) | main.go, daemon.go, preflight.go, logrotate.go, status_line_cmd.go, telemetry_usage_hook.go |
| `worker/shared` | config.go (types + LoadConfig), Issue/Label (from github.go), sentinels + errors (from done.go, confidence.go, and the sentinel constants in pipeline_feature.go, uat.go, codereview.go, pipeline_mergeresolve.go, loop.go), stage enums (from claude.go), retry.go, Runner interface (from runner.go) |
| `worker/infra` | execRunner (runner.go), github.go, worktree.go, claude.go, sessions.go, images.go |
| `worker/engine` | loop.go, slots.go, pipeline_feature.go, pipeline_bug.go, pipeline_mergeresolve.go, codereview.go, uat.go, triage.go, resume.go, prompts.go (+ the `ai/prompts` embed dir moves to `worker/engine/ai/prompts/`) |
| `worker/web` | serve.go, render.go, web.go (+ `web/` embed dir moves to `worker/web/web/`), tracker.go |
| `worker/telemetry` | telemetry_exporter.go, telemetry_tailer.go |
| `worker/testkit` | fakes and builders extracted from helpers_test.go (FakeRunner, gh/claude JSON builders) plus new fake port implementations |

Every `X_test.go` moves with its `X.go`. Cross-cutting tests live where the
code they exercise lives: flow/loop/slots/pipeline/no-auto-retry tests in
`engine`, integration_test.go in `engine` (keeps its `integration` build
tag), main/daemon/preflight/logrotate/status-line tests stay in `worker`.

## Ports (interfaces in `worker/shared`)

Interfaces are defined in `shared`, implemented in `infra`, consumed by
`engine`/`web`/`telemetry`, and wired in `main`.

- **Runner** — process execution seam (the existing `Runner` interface,
  relocated). `infra.ExecRunner` implements it.
- **CodeHost** — issues, labels, comments, PR create/lookup, review comments.
  `infra.GitHub` implements it. The existing consumer-side `UATTarget` and
  `CodeReviewTarget` interfaces fold into this port.
- **Workspace** — worktree/branch/fetch/merge/push operations.
  `infra.Worktree` implements it.
- **Agent** — run a Claude session (with stage, resume, checkpoints,
  snapshots) and read/append the sessions.jsonl chain. `infra.Claude`
  implements it. This is the widest port (~8–10 methods); acceptable given
  how much the pipelines ask of it. Data types the port speaks
  (`ClaudeResult`, `Usage`, `SessionInfo`, `SessionNode`, `CallCheckpoint`)
  move to `shared`.

Port shapes are extracted from the methods the engine actually calls today —
no speculative methods.

## Dependency rules

```
shared    → stdlib + github.com/ngthluu/loope/shared only
infra     → shared
engine    → shared            (never infra — the core rule)
web       → shared, engine
telemetry → shared (+ root loope/shared)
testkit   → shared
main      → everything
```

Enforced by **go-arch-lint** with a checked-in `.go-arch-lint.yml` declaring
these components and allowed edges, run via a `make lint-arch` (or documented
`go run github.com/fe3dback/go-arch-lint@<version> check`) step suitable for
CI. A violation (e.g. `engine` importing `infra`) fails the check.

## Dependency injection

Manual constructor injection; `main.go` is the composition root. Engine
receives its ports through a deps struct:

```go
orch := engine.NewOrchestrator(engine.Deps{
    Cfg:       cfg,
    Host:      infra.NewGitHub(runner, cfg),
    Workspace: infra.NewWorktree(runner, ...),
    Agent:     infra.NewClaude(runner, ...),
    Runner:    runner,
})
```

No DI framework. `web.NewServer` and `telemetry.NewExporter` similarly take
their dependencies (Orchestrator, Runner, Config) as constructor arguments.

## Migration mechanics

Executed in compiling phases; `go build ./...` and `go test ./...` pass after
every phase. Temporary type aliases (`type Config = shared.Config`) in the
old location keep intermediate phases small, and are removed by the end.

1. Create `worker/shared`: move config types/LoadConfig, Issue/Label,
   sentinels, errors, stage enums, retry, Runner interface + port
   definitions and the data types they speak.
2. Create `worker/infra`: move execRunner, GitHub, Worktree, Claude,
   sessions, images; make them implement the ports (compile-time
   `var _ shared.CodeHost = (*GitHub)(nil)` assertions).
3. Create `worker/engine`: move orchestration files; replace concrete
   `*GitHub`/`*Worktree`/`*Claude` fields with ports; add
   `NewOrchestrator(Deps)`; move `ai/prompts` embed root.
4. Create `worker/web` and `worker/telemetry`; export what `main`/`web`
   need (e.g. the not-running/already-running sentinel errors).
5. Create `worker/testkit`; migrate helpers_test.go fakes to exported fakes;
   move all test files next to their sources and fix them up.
6. Add `.go-arch-lint.yml` + make target; remove leftover aliases; final
   full-tree verification.

## Constraints and gotchas (from codebase survey)

- `loop.go`, `slots.go`, `pipeline_mergeresolve.go` define methods on
  `Orchestrator` and must land in the same package (`engine`).
- `prompts.go`'s `promptData()` reads sentinels from 7 files — moving all
  sentinels to `shared` is what makes the split acyclic.
- Two `go:embed` roots move with their files: `ai/prompts` → `engine/`,
  `web/` → `web/`.
- `main.go` stays at `worker/` root so `-ldflags -X main.version` is
  unchanged.
- 16 test files construct structs via unexported fields — solved by
  co-locating tests in the same package; cross-package helpers come from
  `testkit`.
- `serve.go` currently reads unexported `Orchestrator` fields; replaced by
  exported methods/constructor, not by exporting fields wholesale.
- No `init()` functions or global mutable state beyond embed/template vars;
  the process-global `log` logger keeps working across packages unchanged.

## Implementation deviations

Discovered while executing; none change the architecture:

- `runLoop`/`guard` moved from `main.go` into `engine/runloop.go`
  (`engine.RunLoop`): they are orchestration, and their tests need the engine's
  fakes. `main.go` stays the composition root and calls `engine.RunLoop`.
- The dashboard `.go` files live directly in `worker/web/` next to the
  `templates/` and `static/` asset dirs (one dir, not `web/web/`); embed paths
  shortened accordingly.
- The `Agent` port needs only `Call`, `RecordSnapshot`, `CheckpointStage`, and
  `LogDir` — session-chain reads/writes and all per-issue markers became plain
  functions in `worker/shared` (`ResumePoint`, `RecordState`, ...), shared by
  the adapter, the engine, and the dashboard.
- Session-lineage and marker persistence (`sessions.go`, `markers.go`,
  `agentcall.go`) live in `worker/shared`: they are domain state, file-backed
  but stdlib-only.
- Tests are exempt from the arch rules (excludeFiles in `.go-arch-lint.yml`):
  package tests wire real adapters exactly the way main does, which is the
  point of the seams. go-arch-lint's deepScan is off because it flags main's
  injection of adapters into `web.NewServer` — the composition-root pattern
  itself.

## Testing

- No behavior change: the existing test suite is the safety net; all tests
  must pass unmodified in intent (mechanical updates to package names,
  exported identifiers, and testkit imports only).
- New: compile-time port-satisfaction assertions in `infra`; arch-lint check;
  `testkit` fakes get the two self-tests currently in
  concurrency_helpers_test.go.
- `go vet ./...` and the full test suite run per phase.
