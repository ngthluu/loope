# Release notes

One `<tag>.md` per release, e.g. `v0.2.0.md`.

These files are written by the `/release-new-version` Claude Code skill and
committed to `main` **before** the tag is created, so the tagged commit carries
its own note. The `Release` workflow passes the file for the pushed tag to
GoReleaser with `--release-notes`, which makes it the verbatim GitHub Release
body. A tag with no matching file still releases — with an empty body.

Do not edit a note for an already-published tag; it will not change the
published release.
