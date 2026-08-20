# Development

## Testing

```bash
go test ./...                                             # unit tests (no network, no CLIs)
```

All process execution goes through the `shared.Runner` port (`worker/shared/runner.go`,
implemented by `worker/infra`); tests
inject a fake, so the suite runs without git/gh/claude installed.

## Prompts

Every prompt loope sends to Claude, and every comment it posts to GitHub, lives
in [`worker/engine/ai/prompts/`](../worker/engine/ai/prompts) as a `text/template` file — no prompt text is
in the Go source. The directory is embedded into the binary with `go:embed`, so a
release is still a single self-contained file that reads nothing from disk at
runtime; editing a prompt means rebuilding.

Session outcomes are **structured output, not prose sentinels**. Every primary
call passes a `--json-schema` (the `*ResultSchema` constants next to each
stage's code), so the CLI itself constrains the session's final output and the
daemon reads a typed field — `outcome`, `status`, `confidence` — instead of
grepping the reply for a token. The schema is enforced on `--resume` turns too,
which is the point: a resumed session triggered with a bare `continue` used to
answer in prose and strand the stage, because the trigger taught it nothing
about the contract.

The prompt still explains, in prose, *which* outcome fits *when* — the schema
fixes the shape of the answer, not the choice. Rewording a prompt is safe;
adding a placeholder means adding the matching key in the builder, and the
tests in `prompts_test.go` fail loudly if a retired sentinel reappears in a
template or a schema stops being valid JSON.

## Logs

Every Claude call is saved for postmortems. Each call writes three files to the
issue's log dir: the prompt (`NNN-<label>.prompt.md`), the model's result text
(`NNN-<label>.output.md`), and the raw CLI JSON (`NNN-<label>.json`):

```
<workDir>/logs/triage/NNN-triage.{prompt.md,output.md,json}          # one per poll cycle
<workDir>/logs/issue-<N>/NNN-<label>.{prompt.md,output.md,json}      # brainstorm-*, answer-*, plan, execute, debug
```

Numbering continues across restarts; nothing is overwritten.

## Releasing

The normal path is the `/release-new-version` skill in Claude Code, run from a
clean `main` that is in sync with origin. It diffs the latest tag against
`HEAD`, has a Haiku subagent write grouped release notes, asks you for the tag,
then writes `docs/release-notes/<tag>.md`, commits and pushes it, and pushes the
tag. It never picks the version for you and never unwinds a partial run — a
stopped run is safe to re-run.

The tag push triggers the `Release` workflow, which builds the darwin/linux ·
amd64/arm64 binaries, uploads them plus `checksums.txt` to a GitHub Release, and
passes `docs/release-notes/<tag>.md` to GoReleaser with `--release-notes` so the
release body is exactly the note you approved. The `install.sh` one-liner picks
the archives up automatically.

The manual fallback still works:

```bash
git tag v0.1.0
git push origin v0.1.0
```

A hand-pushed tag releases with an **empty body** unless
`docs/release-notes/v0.1.0.md` was committed before the tag — GoReleaser's
generated changelog is disabled.

Dry-run the build locally with `goreleaser release --snapshot --clean`.

## Contributing

Issues and pull requests are welcome. CI (`go build`, `go vet`, `go test ./...`)
must pass; please keep new behavior covered by tests that run without the network
or external CLIs.
