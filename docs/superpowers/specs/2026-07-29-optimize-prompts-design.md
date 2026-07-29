# Optimize loope's prompt templates

Issue: #50

## Problem

Every model-facing prompt loope sends lives in `ai/prompts/*.md.tmpl`, rendered
through a shared `text/template` set (`prompts.go`). The set already has two
factored shared blocks — `ask-format` and `uat-format` — each with tests
(`TestBothRoutesShareThe*FormatBlock`) that fail if a prompt drifts from the
shared source instead of being edited in place. That pattern was only applied
where it happened to be introduced (the UAT and needs-info work); it was never
swept across the rest of the templates. A prompt-by-prompt audit (see below)
found the same class of drift elsewhere: identical or near-identical text
duplicated across files with no shared source, plus two asymmetries where one
prompt has a guard clause its sibling lacks.

## Audit findings

**Duplication with no shared source:**

1. `answerer.md.tmpl` and `done-confirm.md.tmpl` open with an identical
   three-paragraph preamble: "You are the product owner's proxy in an
   automated development pipeline." / "The GitHub issue being implemented:
   `{{.Issue}}`" / "Product owner preferences (persona): `{{.Persona}}`".
2. `uat-bug.md.tmpl` and `uat-feature.md.tmpl` both contain the identical
   sentence "Output ONLY the checklist, between a line reading
   `{{.UATBeginSentinel}}` and a line reading `{{.UATEndSentinel}}`. Print
   nothing before or after those two lines." — a natural fit for the
   `uat-format` block that already sits right below it in both files, but not
   in it.
3. `debug.md.tmpl` and `execute.md.tmpl` both contain the identical sentence
   "HEADLESS: do not ask questions; make reasonable calls and note them in
   commit messages."

**Asymmetric guards (same shape of prompt, one has a safety clause the other lacks):**

4. `uat-bug.md.tmpl` instructs the model to print nothing if the diff is empty
   (nothing was committed). `uat-feature.md.tmpl` has no equivalent guard for
   its own empty case (spec exists but nothing was actually shipped) — a
   plausible pipeline state (feature pipeline stops after planning, or the
   commit hook fails) that would otherwise make the model fabricate a
   checklist for unshipped work.
5. `debug.md.tmpl` states explicitly that the `{{.ConfidenceSentinel}}` line
   must print first "even when an instruction below tells you to print
   another sentinel and stop" (i.e. it outranks `AlreadyDoneSentinel`).
   `brainstorm.md.tmpl` has the identical two-sentinel structure
   (`ConfidenceSentinel` gate, `AlreadyDoneSentinel` later) but no such
   ordering rule — the model is left to guess which sentinel wins if both
   conditions are true at once.

**Other clarity gap:**

6. `triage.md.tmpl` classifies every issue as exactly `"bug"` or `"feature"`
   (enforced structurally by `TriageDecision.Kind` validation in `triage.go`),
   but the two descriptions given to the model can both apply to the same
   issue — "a small, well-scoped defect" (bug) and "unclear scope" / "needs
   design work" (feature) overlap when a small bug's correct fix requires a
   design decision. No tiebreaker is stated.

**Considered and rejected:** merging `brainstorm.md.tmpl`'s and
`debug.md.tmpl`'s confidence-gate preambles into one shared block. They share
a skeleton (print sentinel, gate on threshold, refuse to act below it) but the
scoring rubric in the middle is genuinely different per prompt (spec
completeness vs. bug-report clarity), and debug's rubric carries its own
multi-sentence caveat ("score the report, not the repair..."). Forcing these
into one template parameter would either lose that nuance or require enough
indirection to make the block harder to audit than the current duplication —
not a net win. Leaving them as separate, non-identical prompts is correct;
only the ordering gap (finding 5) is worth fixing.

## Design

Apply the same shared-block pattern already established for `ask-format` and
`uat-format`, plus two targeted content fixes. No behavior change is intended
beyond findings 4 and 5, which close real gaps.

1. **New shared block `proxy-preamble`** in a `.md.tmpl` file, taking
   `.Issue` and `.Persona`. `answerer.md.tmpl` and `done-confirm.md.tmpl` both
   start with `{{template "proxy-preamble" .}}` followed by their own
   distinct instructions.
2. **Extend `uat-format`** to include the "Output ONLY the checklist, between
   a line reading `{{.UATBeginSentinel}}`..." sentence, so `uat-bug.md.tmpl`
   and `uat-feature.md.tmpl` only state their own distinct instructions plus
   `{{template "uat-format" .}}`.
3. **New shared block** (e.g. `headless-commit-notes`) for the identical
   "HEADLESS: do not ask questions; make reasonable calls and note them in
   commit messages." sentence, used by `debug.md.tmpl` and `execute.md.tmpl`.
4. **Add a termination guard to `uat-feature.md.tmpl`**, symmetric to
   `uat-bug.md.tmpl`'s: if inspecting the repository shows nothing was
   actually shipped for the spec (no commits, or the spec's feature isn't
   present in the diff), print nothing — no markers, no checklist.
5. **Add the sentinel-ordering rule to `brainstorm.md.tmpl`**: the
   `{{.ConfidenceSentinel}}` line must print first, before any
   `{{.AlreadyDoneSentinel}}` check is even evaluated — mirroring debug's
   existing rule, worded for brainstorm's own two sentinels.
6. **Add a tiebreaker line to `triage.md.tmpl`**: if an issue plausibly fits
   both categories, classify it `"bug"` when the fix needs no design
   decision, otherwise `"feature"`.

Shared blocks live in whichever existing file already owns a closely related
block (`ask-format.md.tmpl` and `uat-format.md.tmpl` already do this — a new
block does not need its own file unless no natural owner exists; if none
fits, add it to `comments.md.tmpl`-style container file conventions, i.e. a
small dedicated file only if it doesn't belong with an existing one).

## Testing

- Extend `prompts_test.go` with a `TestBothRoutesShareTheProxyPreambleBlock`
  and `TestBothRoutesShareTheHeadlessCommitNotesBlock`, following the existing
  pattern for `ask-format`/`uat-format`.
- Extend `TestUATFormatBlockCarriesItsRules` to assert the moved "Output ONLY
  the checklist..." sentence is present.
- Update `prompts_golden_test.go` expectations for every prompt whose rendered
  text changes shape (answerer, done-confirm, uat-bug, uat-feature, debug,
  execute, brainstorm, triage) — these tests assert exact output, so they are
  the mechanical checklist for "did I preserve everything but the duplication."
- Add a golden case for `uat-feature.md.tmpl`'s new termination guard wording
  and for `brainstorm.md.tmpl`'s new ordering line, mirroring the existing
  bug-prompt cases already in the golden file.
- `TestEveryTemplateRenders`, `TestEveryPromptFileOnDiskIsParsed`, and
  `TestNoSentinelIsHardcodedInATemplate` require no changes; they exercise the
  whole embedded set generically and will simply pick up the new blocks.

## Non-goals

- No change to `comments.md.tmpl`'s per-purpose templates (`pickup`,
  `already-done`, `park`, etc.) or to `triage.md.tmpl`'s JSON output contract
  beyond the one added tiebreaker sentence — the audit found no duplication or
  ambiguity there beyond what's listed above.
- No change to which pipeline stage calls which prompt, to `prompts.go`'s
  rendering mechanism, or to any sentinel constant's value.
- No attempt to unify the confidence-gate preambles of `brainstorm.md.tmpl`
  and `debug.md.tmpl` (see "Considered and rejected" above).
