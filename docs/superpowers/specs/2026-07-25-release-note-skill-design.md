# `/release-new-version` — a Claude skill that owns the release flow

Issue: #36

## Problem

Cutting a loope release today is two hand-typed commands (`git tag`, `git push
origin <tag>`) plus whatever changelog GoReleaser derives from commit subjects.
The subjects are written for reviewers, not readers of a release page, and
nobody reads the diff before tagging. There is no single place a maintainer can
go to be walked through "what changed, what should the next tag be, publish it".

## Goal

Typing `/release-new-version` in Claude Code inside this repo drives the whole
flow: diff the last tag against `HEAD`, have a Haiku subagent write a grouped
Markdown release note, ask the author for the tag, commit the note, push the
tag, and let the existing `Release` workflow publish a GitHub Release whose body
is exactly that note.

## Decisions (from the issue author)

1. GoReleaser's generated changelog is **disabled**; the skill supplies the
   release body.
2. The next version is **always typed by the author** — no proposed bump, no
   auto-increment.
3. Haiku is used through a **subagent** (`model: haiku`), not by switching the
   session model.
4. The note is **grouped Markdown sections** (Features / Fixes / Docs & chores)
   derived from conventional-commit prefixes.
5. The skill lives in **`.claude/skills/`** only — it is a maintainer tool, not
   something shipped to loope users.

## How the note reaches the GitHub Release

The tag push is already the single release trigger
(`.github/workflows/release.yml` → GoReleaser). Rather than racing CI with a
`gh release edit`, the note travels *with* the tag:

- The skill writes the note to `docs/release-notes/<tag>.md` and commits it to
  `main` **before** creating the tag, so the tag points at a commit that
  contains its own release note.
- CI passes that file to GoReleaser with `--release-notes`, which makes it the
  verbatim release body and bypasses changelog generation entirely.

This keeps one trigger, one source of truth, and leaves the published body
byte-identical to what the author approved locally. It also means the notes are
readable in-repo, not only on the releases page.

## Design

### New file: `.claude/skills/release-new-version/SKILL.md`

A single self-contained skill file — no supporting scripts. Frontmatter:

```yaml
---
name: release-new-version
description: Use when cutting a new loope release - diffs the latest tag against
  HEAD, writes a grouped release note with a Haiku subagent, then tags and pushes.
---
```

The body is an ordered procedure. Each step states its command, its stop
condition, and what to show the author.

#### Step 1 — Preflight

Run and check, in one Bash call:

| Check | Command | Stop if |
| --- | --- | --- |
| On `main` | `git rev-parse --abbrev-ref HEAD` | not `main` |
| Clean tree | `git status --porcelain` | non-empty |
| Synced with origin | `git fetch --tags origin && git rev-list --left-right --count origin/main...HEAD` | either side non-zero |
| GitHub CLI ready | `gh auth status` | non-zero exit |

On any failure the skill reports which check failed and stops. It does not
stash, reset, checkout, or pull on the author's behalf.

#### Step 2 — Establish the range

```bash
git describe --tags --abbrev=0   # latest tag, e.g. v0.2.0
```

If the command fails (no tags yet) the range is the full history and the note is
introduced as the first release. Otherwise the range is `<latest>..HEAD`.

If the range is empty (`git rev-list --count <latest>..HEAD` is `0`) the skill
reports that there is nothing to release and stops.

#### Step 3 — Collect the material

```bash
git log --no-merges --pretty=format:'%h %s%n%b---' <range>
git diff --stat <range>
```

Merge commits are excluded because this repo squashes nothing — the PR merges
carry no information the branch commits do not. `--stat` (not the full diff)
keeps the subagent input bounded; commit subjects and bodies carry the intent.

#### Step 4 — Write the note (Haiku subagent)

Dispatch **one** subagent with `model: haiku`, `subagent_type: general-purpose`,
`run_in_background: false`. Its prompt contains the collected log and stat
output plus these instructions:

- Group into `## Features`, `## Fixes`, `## Docs & chores`, using the
  conventional-commit prefix (`feat:` → Features, `fix:` → Fixes,
  `docs:`/`chore:`/`test:`/`refactor:` → Docs & chores). Commits with no
  recognizable prefix are classified by reading the subject.
- Omit any section that would be empty.
- One bullet per user-visible change, written for someone who did not read the
  code; collapse several commits that deliver one change into one bullet.
- No preamble, no heading for the version itself, no trailing sign-off — the
  release page supplies the version heading.
- Return the Markdown only.

The subagent has no write tools in play conceptually — the skill instructs it
explicitly not to run git commands or edit files. It only reads its prompt and
returns text.

#### Step 5 — Ask for the tag

Show the returned note, then ask the author to type the tag. The skill never
proposes a version.

Validate the answer:

- Matches `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`.
- Does not already exist locally or on origin (`git rev-parse -q --verify
  refs/tags/<tag>`, and the fetched remote tags from step 1).

On a rejected answer, say why and ask again. Three rejected answers in a row
ends the skill rather than looping forever.

#### Step 6 — Confirm and write

Write the note to `docs/release-notes/<tag>.md` verbatim, show the final file
path and contents, and ask for a plain yes/no to publish. Anything other than a
clear yes stops the skill, leaving the file on disk uncommitted so the author
can edit it and re-run — the file is reused if it already exists rather than
regenerated.

#### Step 7 — Publish

```bash
git add docs/release-notes/<tag>.md
git commit -m "chore(release): notes for <tag>"
git push origin main
git tag <tag>
git push origin <tag>
```

Each command is checked; the first failure stops the skill and reports what
succeeded. In particular, **the tag is never created if the notes commit failed
to push** — otherwise CI would tag a commit whose notes file is not on origin.

The commit message carries no attribution trailer: it is a mechanical release
artifact, and `.claude/settings.json` attribution applies to work commits.

#### Step 8 — Report

Print the tag, the notes path, and
`https://github.com/ngthluu/loope/actions/workflows/release.yml` so the author
can watch the build. The skill does not poll CI.

### Changed file: `.goreleaser.yaml`

Replace the `changelog:` block's filters with an outright disable:

```yaml
changelog:
  disable: true
```

The filters are no longer meaningful once the body is authored. Nothing else in
the file changes.

### Changed file: `.github/workflows/release.yml`

Insert one step before GoReleaser that decides the arguments:

```yaml
      - name: Select release notes
        id: notes
        run: |
          file="docs/release-notes/${GITHUB_REF_NAME}.md"
          if [ -f "$file" ]; then
            echo "args=release --clean --release-notes=$file" >> "$GITHUB_OUTPUT"
          else
            echo "args=release --clean" >> "$GITHUB_OUTPUT"
          fi

      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: ${{ steps.notes.outputs.args }}
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

The fallback matters: a tag pushed by hand (or an old tag re-pushed) still
produces a release, just with an empty body, instead of failing the workflow on
a missing `--release-notes` path.

### Changed file: `docs/development.md`

The "Releasing" section becomes: `/release-new-version` in Claude Code is the
normal path; it writes `docs/release-notes/<tag>.md`, commits it, and pushes the
tag. The manual `git tag && git push` path is documented as the fallback, with
the note that it yields an empty release body unless the notes file is committed
first.

### New directory: `docs/release-notes/`

Holds one `<tag>.md` per release. Nothing reads it except CI. It is not added to
the GoReleaser archive `files:` list — users get the notes from the release
page.

## Error handling

The skill's failure posture is **stop and report, never unwind**, consistent
with the project's "continue from existing state" principle:

- No `git reset`, no `git tag -d`, no force-push, no `gh release delete` in any
  branch of the skill.
- A half-finished run leaves either an uncommitted notes file (safe to re-run:
  step 6 reuses it) or a pushed notes commit without a tag (safe to re-run: step
  2 sees an empty-or-not range and step 6 finds the existing file).
- Re-running after a fully successful publish is caught in step 5 — the tag
  already exists — so the skill cannot double-publish.

## Testing

The skill is Markdown instructions, so there is no Go test surface. Verification
is:

1. `go build ./... && go vet ./... && go test ./...` — unchanged, proves the
   Go-side edits (none) broke nothing.
2. `goreleaser check` — proves `.goreleaser.yaml` is still valid with
   `changelog.disable`.
3. A dry run of the workflow logic locally:
   `GITHUB_REF_NAME=v0.2.0` against a hand-written
   `docs/release-notes/v0.2.0.md` to confirm the `args` selection produces the
   `--release-notes` form, and without the file to confirm the fallback.
4. `goreleaser release --snapshot --clean` to confirm a body-less snapshot still
   builds.

The end-to-end path (subagent → tag → published body) is exercised by the next
real release; the issue's acceptance is exactly that run.

## Out of scope

- Proposing or computing the version bump.
- A `CHANGELOG.md` in the repo root.
- Editing an already-published release.
- Shipping the skill to loope users or documenting it outside
  `docs/development.md`.
