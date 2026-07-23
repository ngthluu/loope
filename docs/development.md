# Development

## Testing

```bash
go test ./...                                             # unit tests (no network, no CLIs)
go test -tags integration -run TestIntegrationTriage -v   # real claude CLI smoke test
```

All process execution goes through the `Runner` interface (`runner.go`); tests
inject a fake, so the suite runs without git/gh/claude installed.

## Prompts

Every prompt loope sends to Claude, and every comment it posts to GitHub, lives
in [`ai/prompts/`](../ai/prompts) as a `text/template` file — no prompt text is
in the Go source. The directory is embedded into the binary with `go:embed`, so a
release is still a single self-contained file that reads nothing from disk at
runtime; editing a prompt means rebuilding.

Sentinel tokens (`CONFIDENCE:`, `SPEC_READY:`, `PIPELINE_READY`,
`PIPELINE_ALREADY_DONE:`, `DONE_CONFIRMED`) are injected from the Go constants
rather than written in the templates, so the instruction given to the model and
the parser reading its reply cannot drift apart. Rewording a prompt is safe;
adding a placeholder means adding the matching key in the builder, and the tests
in `prompts_test.go` will fail loudly if you forget.

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

Releases are cut by [GoReleaser](https://goreleaser.com) from a pushed tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `Release` workflow builds the darwin/linux · amd64/arm64 binaries, uploads
them plus `checksums.txt` to a GitHub Release, and the `install.sh` one-liner
picks them up automatically. Dry-run the build locally with
`goreleaser release --snapshot --clean`.

## Contributing

Issues and pull requests are welcome. CI (`go build`, `go vet`, `go test ./...`)
must pass; please keep new behavior covered by tests that run without the network
or external CLIs.
