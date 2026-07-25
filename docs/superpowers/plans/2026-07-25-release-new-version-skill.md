# `/release-new-version` Skill Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give loope maintainers a `/release-new-version` Claude Code skill that diffs the last tag against `HEAD`, has a Haiku subagent write a grouped release note, commits that note, pushes the tag, and lets CI publish it verbatim as the GitHub Release body.

**Architecture:** The release note travels *with* the tag instead of racing CI. The skill writes `docs/release-notes/<tag>.md`, commits and pushes it to `main`, then creates and pushes the tag — so the tagged commit already contains its own note. The `Release` workflow picks that file up and hands it to GoReleaser via `--release-notes`, with a fallback to a plain `release --clean` when no file exists. GoReleaser's own changelog generation is disabled. The skill itself is a single Markdown procedure file in `.claude/skills/` — no scripts, no Go code.

**Tech Stack:** Markdown skill file (Claude Code skill format, YAML frontmatter), GoReleaser v2 (`.goreleaser.yaml`), GitHub Actions (`.github/workflows/release.yml`), Bash, Go 1.25 (unchanged — this feature adds no Go code).

## Global Constraints

- The skill lives at `.claude/skills/release-new-version/SKILL.md` **only**. It is a maintainer tool; it is never shipped to loope users and is not added to the GoReleaser archive `files:` list.
- Skill frontmatter `name` must be exactly `release-new-version`.
- The skill **never proposes or computes a version**. The author types the tag.
- Tag format the skill accepts: `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`.
- Release-note path is exactly `docs/release-notes/<tag>.md` (e.g. `docs/release-notes/v0.2.0.md`). The workflow and the skill must agree on this string byte-for-byte.
- Release-note commit message is exactly `chore(release): notes for <tag>` and carries **no attribution trailer** (the `.claude/settings.json` attribution applies to work commits, not this mechanical artifact).
- Note section headings are exactly `## Features`, `## Fixes`, `## Docs & chores`. Empty sections are omitted. No version heading, no preamble, no sign-off.
- Haiku is used via a **subagent** (`model: haiku`, `subagent_type: general-purpose`, `run_in_background: false`) — never by switching the session model.
- Failure posture: **stop and report, never unwind.** No `git reset`, no `git tag -d`, no force-push, no `gh release delete`, no `git stash`/`checkout`/`pull` on the author's behalf, anywhere in the skill.
- The tag is never created if the notes commit failed to push.
- Three rejected tag answers in a row ends the skill.
- Commits made *while implementing this plan* follow normal repo convention: conventional-commit subjects, and the attribution trailer from `.claude/settings.json` (`🤖 Generated with [Claude Code](https://claude.com/claude-code)` + `Co-Authored-By: Claude <noreply@anthropic.com>`).

## Assumptions

Recorded because the spec is approved and no questions were asked:

1. **`goreleaser` is not installed on this machine** (verified: `which goreleaser` → not found). Task 1 installs it via `brew install goreleaser` if available; if installation is not possible, the plan gives a Python-based YAML validation fallback so the task is never blocked. `goreleaser release --snapshot --clean` (spec's Testing item 4) is therefore listed as an optional local check, not a gate.
2. **`docs/release-notes/` is created with a committed `README.md`** rather than a `.gitkeep`, so the directory exists in a fresh clone and its purpose is self-documenting. The spec says the directory holds one `<tag>.md` per release and that "nothing reads it except CI"; a README does not change that.
3. **The workflow's notes-selection logic is verified by extracting the same shell into a throwaway script and running both branches**, since GitHub Actions cannot be executed locally here. The script is not committed.
4. **No Go code changes.** `go build ./... && go vet ./... && go test ./...` is run once at the end as a regression gate (spec Testing item 1).

## File Structure

| File | Status | Responsibility |
| --- | --- | --- |
| `.goreleaser.yaml` | Modify (lines 43-51, the `changelog:` block) | Stop generating a changelog; the release body is authored. |
| `.github/workflows/release.yml` | Modify (the `Run GoReleaser` step) | Choose `--release-notes=<file>` when the note exists for this tag, else fall back to plain `release --clean`. |
| `docs/release-notes/README.md` | Create | Explains the directory contract to humans; makes the directory exist in git. |
| `.claude/skills/release-new-version/SKILL.md` | Create | The whole procedure: preflight → range → material → Haiku subagent → tag prompt → confirm → publish → report. |
| `docs/development.md` | Modify (`## Releasing`, lines 41-53) | Document `/release-new-version` as the normal path and the manual tag push as the fallback. |

Task order matters: Task 1 lands the CI half so that a note file committed by the skill is actually honored; Task 2 lands the skill that produces such files; Task 3 documents both.

---

### Task 1: CI honors a committed release note

**Files:**
- Modify: `.goreleaser.yaml:43-51` (the `changelog:` block)
- Modify: `.github/workflows/release.yml:23-29` (the `Run GoReleaser` step)
- Create: `docs/release-notes/README.md`
- Test: no committed test file — verification is `goreleaser check` plus a throwaway extraction of the workflow shell (see steps 2 and 6)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: the contract `docs/release-notes/<GITHUB_REF_NAME>.md` — a file at that exact path, if present at the tagged commit, becomes the verbatim GitHub Release body. Task 2's skill writes to this path.

- [ ] **Step 1: Write the failing check for the workflow's notes selection**

Create a throwaway script (do **not** `git add` it) at `/tmp/notes-args.sh` holding exactly the shell the workflow step will run, with `$GITHUB_OUTPUT` redirected so it can be inspected:

```bash
cat > /tmp/notes-args.sh <<'EOF'
#!/bin/bash
set -euo pipefail
file="docs/release-notes/${GITHUB_REF_NAME}.md"
if [ -f "$file" ]; then
  echo "args=release --clean --release-notes=$file" >> "$GITHUB_OUTPUT"
else
  echo "args=release --clean" >> "$GITHUB_OUTPUT"
fi
EOF
chmod +x /tmp/notes-args.sh
```

Then write the assertion script at `/tmp/notes-args-test.sh`:

```bash
cat > /tmp/notes-args-test.sh <<'EOF'
#!/bin/bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

fail=0
check() { # check <label> <expected> <actual>
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: expected [$2] got [$3]"; fail=1; fi
}

# Case A: the note exists for this tag -> --release-notes form
mkdir -p docs/release-notes
printf '## Fixes\n\n- test note\n' > docs/release-notes/v9.9.9-plantest.md
export GITHUB_REF_NAME=v9.9.9-plantest
export GITHUB_OUTPUT=/tmp/notes-out-a.txt
: > "$GITHUB_OUTPUT"
/tmp/notes-args.sh
check "with-notes" \
  "args=release --clean --release-notes=docs/release-notes/v9.9.9-plantest.md" \
  "$(cat "$GITHUB_OUTPUT")"
rm -f docs/release-notes/v9.9.9-plantest.md

# Case B: no note for this tag -> fallback
export GITHUB_REF_NAME=v9.9.9-missing
export GITHUB_OUTPUT=/tmp/notes-out-b.txt
: > "$GITHUB_OUTPUT"
/tmp/notes-args.sh
check "fallback" "args=release --clean" "$(cat "$GITHUB_OUTPUT")"

exit $fail
EOF
chmod +x /tmp/notes-args-test.sh
```

- [ ] **Step 2: Run the check against the *current* workflow to confirm it is not yet implemented**

Run:

```bash
grep -n "release-notes" .github/workflows/release.yml; echo "grep exit: $?"
```

Expected: no matching lines, `grep exit: 1` — the workflow has no notes selection yet. (`/tmp/notes-args.sh` is the *target* shell, so it passes on its own; the failing condition being fixed is that this shell does not exist in the workflow.)

- [ ] **Step 3: Add the notes-selection step to the workflow**

Edit `.github/workflows/release.yml`, replacing the `Run GoReleaser` step with:

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

The whole file should end up as:

```yaml
name: Release

on:
  push:
    tags:
      - "v*"

permissions:
  contents: write

jobs:
  goreleaser:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0

      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"
          cache: true

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

- [ ] **Step 4: Disable GoReleaser's changelog**

Edit `.goreleaser.yaml`, replacing:

```yaml
changelog:
  sort: asc
  filters:
    exclude:
      - "^docs:"
      - "^test:"
      - "^chore:"
      - Merge pull request
      - Merge branch
```

with:

```yaml
changelog:
  disable: true
```

Nothing else in the file changes.

- [ ] **Step 5: Create the release-notes directory**

Create `docs/release-notes/README.md`:

```markdown
# Release notes

One `<tag>.md` per release, e.g. `v0.2.0.md`.

These files are written by the `/release-new-version` Claude Code skill and
committed to `main` **before** the tag is created, so the tagged commit carries
its own note. The `Release` workflow passes the file for the pushed tag to
GoReleaser with `--release-notes`, which makes it the verbatim GitHub Release
body. A tag with no matching file still releases — with an empty body.

Do not edit a note for an already-published tag; it will not change the
published release.
```

- [ ] **Step 6: Run the workflow-shell check and confirm it passes**

Run:

```bash
diff <(sed -n '/^          file="docs\/release-notes/,/^          fi$/p' .github/workflows/release.yml | sed 's/^          //') <(sed -n '/^file=/,/^fi$/p' /tmp/notes-args.sh)
/tmp/notes-args-test.sh
```

Expected: `diff` prints nothing (the workflow shell and the tested shell are identical), and the test prints:

```
PASS with-notes
PASS fallback
```

with exit status 0.

- [ ] **Step 7: Validate the GoReleaser config**

Run:

```bash
command -v goreleaser >/dev/null || brew install goreleaser
goreleaser check
```

Expected: `1 configuration file(s) validated` / no errors.

If `brew` is unavailable or installation fails, fall back to a structural check and record in the commit message body that `goreleaser check` could not be run locally:

```bash
python3 -c "
import re,sys
t=open('.goreleaser.yaml').read()
assert re.search(r'^changelog:\n  disable: true\n', t, re.M), 'changelog block not as expected'
assert 'filters:' not in t, 'stale changelog filters remain'
print('goreleaser yaml structure OK')
"
```

Expected: `goreleaser yaml structure OK`.

- [ ] **Step 8: Validate the workflow YAML parses**

Run:

```bash
python3 -c "
import json,subprocess,sys
try:
    import yaml
except ImportError:
    sys.exit('pyyaml missing - run: python3 -m pip install --user pyyaml')
d=yaml.safe_load(open('.github/workflows/release.yml'))
steps=d['jobs']['goreleaser']['steps']
names=[s.get('name') for s in steps]
assert 'Select release notes' in names, names
gr=[s for s in steps if s.get('name')=='Run GoReleaser'][0]
assert gr['with']['args']=='\${{ steps.notes.outputs.args }}', gr['with']['args']
print('workflow OK:', names)
"
```

Expected: `workflow OK: [None, None, 'Select release notes', 'Run GoReleaser']`

(The first two steps are the bare `uses:` checkout/setup-go steps, which have no `name`.)

- [ ] **Step 9: Commit**

```bash
git add .goreleaser.yaml .github/workflows/release.yml docs/release-notes/README.md
git commit -m "$(cat <<'EOF'
feat(release): publish committed release notes as the release body

Disable GoReleaser's generated changelog and pass
docs/release-notes/<tag>.md to GoReleaser with --release-notes when the
tagged commit carries one, falling back to a body-less release otherwise.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: The `/release-new-version` skill

**Files:**
- Create: `.claude/skills/release-new-version/SKILL.md`
- Test: no committed test file — verification is the frontmatter/structure check in step 3 and the read-through in step 4

**Interfaces:**
- Consumes: the path contract from Task 1 — writing `docs/release-notes/<tag>.md` and committing it to `main` before the tag is what makes CI use it as the release body.
- Produces: a skill invocable as `/release-new-version`. Nothing in the codebase imports it; Task 3 references it by that name in `docs/development.md`.

- [ ] **Step 1: Confirm the skill does not exist yet**

Run:

```bash
ls .claude/skills/release-new-version/SKILL.md 2>&1
```

Expected: `ls: .claude/skills/release-new-version/SKILL.md: No such file or directory`

- [ ] **Step 2: Write the skill file**

Create `.claude/skills/release-new-version/SKILL.md` with exactly this content:

````markdown
---
name: release-new-version
description: Use when cutting a new loope release - diffs the latest tag against HEAD, writes a grouped release note with a Haiku subagent, then tags and pushes.
---

# Cutting a loope release

Announce at start: "Using release-new-version to cut a release."

This procedure is **stop and report, never unwind**. If any step fails, say
which step failed and what has already happened, then stop. Never run
`git reset`, `git tag -d`, `git push --force`, `gh release delete`,
`git stash`, `git checkout`, or `git pull` in this skill — not even to clean up
after yourself. A half-finished run is safe to re-run.

Never propose a version number. The author types it.

## Step 1 — Preflight

Run in one Bash call:

```bash
echo "branch: $(git rev-parse --abbrev-ref HEAD)"
echo "dirty: [$(git status --porcelain)]"
git fetch --tags origin
echo "divergence: $(git rev-list --left-right --count origin/main...HEAD)"
gh auth status
```

Stop if any of these hold, reporting exactly which check failed:

| Check | Stop if |
| --- | --- |
| On `main` | branch is not `main` |
| Clean tree | `git status --porcelain` output is non-empty |
| Synced with origin | either number in the divergence count is non-zero |
| GitHub CLI ready | `gh auth status` exited non-zero |

Do not stash, reset, checkout, or pull on the author's behalf. Tell them what
to fix and stop.

## Step 2 — Establish the range

```bash
git describe --tags --abbrev=0
```

- If this fails (no tags yet): the range is the full history — use `HEAD` as
  the range for the log/diff commands below, and introduce the note as the
  first release when you present it.
- Otherwise the range is `<latest>..HEAD`.

Then:

```bash
git rev-list --count <range>
```

If the count is `0`, report that there is nothing to release since `<latest>`
and stop.

## Step 3 — Collect the material

```bash
git log --no-merges --pretty=format:'%h %s%n%b---' <range>
git diff --stat <range>
```

Merge commits are excluded: this repo does not squash, so the PR merge commits
carry nothing the branch commits do not. Use `--stat`, not the full diff — the
commit subjects and bodies carry the intent, and the stat keeps the subagent
input bounded.

## Step 4 — Write the note with a Haiku subagent

Dispatch **exactly one** subagent: `model: haiku`,
`subagent_type: general-purpose`, `run_in_background: false`. Paste the full
step-3 output into its prompt, followed by these instructions verbatim:

> You are writing the release note for a Go CLI project called loope. Below is
> the commit log and diffstat for the range being released.
>
> Write Markdown release notes:
>
> - Group into `## Features`, `## Fixes`, `## Docs & chores`. Use the
>   conventional-commit prefix: `feat:` → Features, `fix:` → Fixes, and
>   `docs:`/`chore:`/`test:`/`refactor:` → Docs & chores. Classify commits with
>   no recognizable prefix by reading the subject.
> - Omit any section that would be empty.
> - One bullet per user-visible change, written for someone who did not read the
>   code. Collapse several commits that deliver one change into one bullet.
> - No preamble, no heading for the version itself, no trailing sign-off — the
>   release page supplies the version heading.
> - Return the Markdown and nothing else.
>
> Do not run git commands. Do not read or edit any file. Work only from the text
> in this prompt.

Use the returned Markdown verbatim as the note. Do not rewrite it yourself; if
it is unusable, say so and stop.

## Step 5 — Ask for the tag

Show the returned note, then ask the author to type the tag for this release.
Never suggest one.

Validate the answer:

1. It matches `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`.
2. It does not already exist:

```bash
git rev-parse -q --verify refs/tags/<tag>
```

Because step 1 ran `git fetch --tags origin`, a tag that exists on origin is
already local, so this one check covers both.

If the answer is rejected, say why (bad format, or "that tag already exists —
it was probably already released") and ask again. After **three** rejected
answers in a row, stop the skill.

## Step 6 — Confirm and write

If `docs/release-notes/<tag>.md` already exists, **reuse it as-is** — the
author may have edited it after an earlier stopped run. Do not overwrite it.
Otherwise write the note there verbatim.

Show the file path and its full contents, then ask for a plain yes/no to
publish. Anything other than a clear yes stops the skill. The file stays on
disk, uncommitted, so the author can edit it and re-run.

## Step 7 — Publish

Run these one at a time, checking each:

```bash
git add docs/release-notes/<tag>.md
git commit -m "chore(release): notes for <tag>"
git push origin main
git tag <tag>
git push origin <tag>
```

The commit message carries **no attribution trailer** — this is a mechanical
release artifact, and the `.claude/settings.json` attribution applies to work
commits.

On the first failure, stop and report which commands succeeded. In particular:
**never create the tag if `git push origin main` failed** — otherwise CI would
tag a commit whose notes file is not on origin, and the release body would be
empty.

## Step 8 — Report

Print:

- the tag
- `docs/release-notes/<tag>.md`
- `https://github.com/ngthluu/loope/actions/workflows/release.yml`

Do not poll CI.

## Recovering from a stopped run

Just re-run the skill.

- Stopped before step 7: an uncommitted `docs/release-notes/<tag>.md` may be on
  disk. Step 1's clean-tree check will flag it — the author either commits it,
  or the tree is clean because nothing was written. Once past preflight, step 6
  reuses the existing file.
- Stopped between the notes push and the tag push: the notes commit is on
  `main`, the tag is not. Step 6 finds the existing file and step 7 creates the
  tag.
- Ran to completion: step 5 rejects the tag because it now exists, so the skill
  cannot double-publish.
````

- [ ] **Step 3: Verify the frontmatter and required sections**

Run:

```bash
python3 -c "
import re,sys
p='.claude/skills/release-new-version/SKILL.md'
t=open(p).read()
m=re.match(r'^---\n(.*?)\n---\n', t, re.S)
assert m, 'missing YAML frontmatter'
fm=m.group(1)
assert 'name: release-new-version' in fm, fm
assert fm.strip().startswith('name:'), fm
assert re.search(r'^description: \S', fm, re.M), fm
body=t[m.end():]
for needed in ['## Step 1 — Preflight','## Step 2','## Step 3','## Step 4','## Step 5','## Step 6','## Step 7','## Step 8',
               'docs/release-notes/<tag>.md','chore(release): notes for <tag>','model: haiku',
               '## Features','## Fixes','## Docs & chores']:
    assert needed in body, 'missing: '+needed
for banned in ['git reset --hard','push --force','gh release delete','git tag -d <']:
    assert banned not in body, 'banned op present: '+banned
print('SKILL.md OK')
"
```

Expected: `SKILL.md OK`

- [ ] **Step 4: Read the file back end-to-end**

Read `.claude/skills/release-new-version/SKILL.md` and confirm, by eye:

- Every command that needs the tag or range uses the `<tag>` / `<range>`
  placeholder consistently.
- Step 7's ordering is notes commit → push main → tag → push tag.
- No step tells the agent to invent a version number.

Fix anything that drifted, then re-run step 3's check.

- [ ] **Step 5: Commit**

```bash
git add .claude/skills/release-new-version/SKILL.md
git commit -m "$(cat <<'EOF'
feat(release): add the /release-new-version skill

Drives the whole release: preflight, tag-to-HEAD range, a Haiku subagent
that writes grouped notes, an author-typed tag, then the notes commit and
tag push. Stops and reports on any failure; never unwinds.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Document the new release path

**Files:**
- Modify: `docs/development.md:41-53` (the `## Releasing` section)
- Test: no committed test file — verification is the link/path consistency check in step 3 and the full Go gate in step 4

**Interfaces:**
- Consumes: the skill name `/release-new-version` from Task 2 and the `docs/release-notes/<tag>.md` contract from Task 1.
- Produces: nothing other tasks depend on. This is the last task.

- [ ] **Step 1: Confirm the current section still describes only the manual path**

Run:

```bash
sed -n '41,54p' docs/development.md
```

Expected: the existing `## Releasing` section, with `git tag v0.1.0` as the only documented path and no mention of `/release-new-version`.

- [ ] **Step 2: Rewrite the Releasing section**

Replace lines 41-53 of `docs/development.md` (from `## Releasing` through the
`goreleaser release --snapshot --clean` sentence) with:

````markdown
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
````

- [ ] **Step 3: Verify the docs agree with the implementation**

Run:

```bash
python3 -c "
t=open('docs/development.md').read()
for needed in ['/release-new-version','docs/release-notes/<tag>.md','empty body','git push origin v0.1.0']:
    assert needed in t, 'missing: '+needed
print('development.md OK')
"
grep -c "release-notes" .github/workflows/release.yml
```

Expected: `development.md OK`, then `2` (the `file=` line and the `--release-notes` line in the workflow).

- [ ] **Step 4: Run the full Go gate**

Run:

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: build and vet silent, all packages `ok` / `no test files`. This
feature adds no Go code; the gate proves nothing regressed.

- [ ] **Step 5: Commit**

```bash
git add docs/development.md
git commit -m "$(cat <<'EOF'
docs: document /release-new-version as the release path

🤖 Generated with [Claude Code](https://claude.com/claude-code)

Co-Authored-By: Claude <noreply@anthropic.com>
EOF
)"
```

---

## Out of scope

Per the spec, do not build any of these:

- Proposing or computing the version bump.
- A `CHANGELOG.md` in the repo root.
- Editing an already-published release.
- Shipping the skill to loope users, or documenting it anywhere other than
  `docs/development.md`.
- Polling CI after the tag push.

## Acceptance

The end-to-end path (subagent → tag → published body) is exercised by the next
real release — that run is the issue's acceptance criterion. Everything
verifiable before then is covered by the task steps above:
`goreleaser check`, the workflow-shell branch check, the SKILL.md structure
check, and `go build ./... && go vet ./... && go test ./...`.
