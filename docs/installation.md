# Installation

## Quick install

```sh
curl -fsSL https://raw.githubusercontent.com/ngthluu/loope/main/install.sh | sh
```

This downloads the prebuilt binary for your OS/arch from the
[latest release](https://github.com/ngthluu/loope/releases/latest), verifies its
checksum, and installs it to `/usr/local/bin` (override with `LOOPE_INSTALL_DIR`,
pin a version with `LOOPE_VERSION=v0.1.0`). Binaries are published for macOS and
Linux on `amd64` and `arm64`.

Prefer to do it yourself? Grab an archive from the
[releases page](https://github.com/ngthluu/loope/releases), or
[build from source](#build-from-source). Check the installed version with
`loope --version`.

## Prerequisites

loope is a wrapper around your local toolchain — these must be present at run
time:

- **Go 1.25+** to build.
- **git**, with the target repo cloned locally.
- **gh** (GitHub CLI), authenticated (`gh auth login`) with permission to edit
  issues, push branches, and open PRs on the target repo.
- **claude** (Claude Code CLI), logged in.
- The **superpowers** plugin installed in the Claude profile loope runs under
  (`claude plugin install superpowers@claude-plugins-official`) — the pipeline
  prompts are superpowers slash commands and are inert text without it.
- **curl** (optional) — used to download issue image attachments; without it
  those are skipped.

> **Warning:** pipeline sessions run with `--dangerously-skip-permissions` so
> they can work unattended. Only point the loop at repositories where you are
> comfortable with an autonomous agent reading, running, and committing code.

## Doctor

loope verifies this toolchain at startup and refuses to run when a required
piece is missing, printing what is missing and the command that fixes it. Run
the same checks standalone:

```bash
./loope --doctor --config loope.json
```

`--doctor` prints the full report even when everything passes and exits non-zero
when a required check failed. Missing labels and a missing `curl` are warnings:
they are reported but never block the run.

## Create the labels

The state labels and the eligible label must exist in the repo before the loop
can apply them — the `labels` preflight check warns with exactly these commands
when any are missing:

```bash
gh label create ai-agent      --repo your-org/your-repo
gh label create ai-wip        --repo your-org/your-repo
gh label create ai-done       --repo your-org/your-repo
gh label create ai-rework     --repo your-org/your-repo
gh label create ai-needs-info --repo your-org/your-repo
```

## Build from source

```bash
go build -o loope .
cp loope.json.example loope.json   # then edit repoPath / repoSlug / workDir
./loope --config loope.json        # daemon: poll every pollIntervalSec, serve the dashboard
```

`--config` is required — there is no default config path. The daemon shuts down
gracefully on Ctrl-C / SIGTERM; if a pipeline is interrupted mid-issue, the
failure path still cleans up labels and worktrees. To validate a new config
without starting the loop, run `./loope --doctor --config loope.json`.

See [Configuration](configuration.md) for every config field.
