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
