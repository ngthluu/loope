# Simpler clarifying questions and grouped UAT checklist — design

Issue: #56 — *fix: simpler brainstorm question, group UAT by group*

## Problem

Two prompt outputs are harder to use than they need to be:

1. **Clarifying questions** (`ai/prompts/ask-format.md.tmpl`, shared by the
   brainstorm and debug confidence gates) are one-sentence-each and capped at
   5, but nothing stops a sentence from being dense or jargon-heavy. The
   person answering may be skimming quickly and needs the question to land on
   first read.
2. **UAT checklists** (`ai/prompts/uat-format.md.tmpl`, shared by the feature
   and bug routes) were deliberately flattened to a single ungrouped list in
   the #44 brevity work (`docs/superpowers/specs/2026-07-25-uat-checklist-brevity-design.md`).
   That fixed the "several screens of prose under many free-form headings"
   problem, but lost all structure — a human verifying a checklist with both
   normal-path and error-path items has to mentally sort them itself.

## Decisions

1. **Plain-language rule for questions, not a rewrite of the whole format.**
   `ask-format` keeps everything from #44/#45 (one sentence per question,
   `a) b) c)` guessable answers, 5-question cap, 200-word cap). It gains one
   new rule: write each question in short, plain sentences — common everyday
   words, no jargon or nested clauses, one idea per sentence. This is the
   ASD-STE100 (Simplified Technical English) principle applied to prose, not
   a citation the model needs to know by name.
2. **UAT goes back to grouped, but with exactly two fixed groups, not
   free-form headings.** The problem the brevity work solved was *many*
   ad hoc `###` headings ("Preflight guards", "Edge cases", "Cleanup", ...)
   each with prose items. Two fixed groups avoids that regression:
   - `### Happy path` — the normal, successful behavior.
   - `### Edge cases` — errors, invalid input, and boundary behavior.
   A group with no items is omitted entirely (never invent items to fill
   it). Order is always Happy path first, Edge cases second.
3. **Item shape is unchanged.** Still `Action → expected result`, 15 words or
   fewer, no prose sentences, no implementation detail. Only the grouping
   wrapper changes — coverage and brevity rules from #44 stand.
4. **No enforcement outside the prompt**, matching #44 decision 4: no new
   length or shape validation in code. The prompt rules are the only
   mechanism, for both changes.

## Design

### `ask-format.md.tmpl`

Add one rule to the existing list, placed right after the one-sentence-each
rule since it governs the same sentences:

```
- Write each question in short, plain sentences: common words, one idea per sentence, no jargon.
```

Nothing else in the block changes. This is shared by `brainstorm.md.tmpl` and
`debug.md.tmpl`, so both confidence-gate pushbacks get plainer questions for
free.

### `uat-format.md.tmpl`

Replace the "single flat list" rule with a two-group rule, keep every other
rule as-is:

```
Rules for the checklist:
- Two group headings only, in this order: `### Happy path`, then `### Edge cases`. Omit a group with no items; never invent items to fill it. No other headings, no intro line, no closing line.
- Each item is `Action → expected result`: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover {{.UATCoverage}}, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.
```

Shape of the output:

```markdown
### Happy path
- [ ] Submit valid form → shows success message

### Edge cases
- [ ] Submit with empty required field → shows inline validation error
```

`UATCoverage`, `SpecPath`/`Issue`, and the sentinel contract in
`uat-feature.md.tmpl` / `uat-bug.md.tmpl` are untouched.

### Docs

`docs/how-it-works.md`'s UAT section (around line 39) currently reads "a
single flat list of `- [ ]` items — no group headings". Update it to describe
the two-group shape instead.

## Files touched

- `ai/prompts/ask-format.md.tmpl` — add the plain-language rule.
- `ai/prompts/uat-format.md.tmpl` — replace the flat-list rule with the
  two-group rule.
- `prompts_test.go` — `TestAskFormatBlockCarriesItsRules` gains an assertion
  for the plain-language rule; `TestUATFormatBlockCarriesItsRules` drops the
  `strings.Contains(got, "###")` negative assertion (headings are back by
  design) and instead asserts `### Happy path` and `### Edge cases` are both
  present, alongside the rules that still apply (`Action → expected result`,
  `15 words or fewer`, `Compress wording, never coverage`, coverage line, "Do
  not modify, create, or commit any file").
- `prompts_golden_test.go` — update `TestGoldenBrainstormPromptWithThreshold`,
  the debug golden test, and `TestGoldenUATFeaturePrompt` /
  `TestGoldenUATBugPrompt` to match the new rendered rule text.
- `docs/how-it-works.md` — update the one sentence describing the checklist's
  shape.

Not touched: `maxUATChars`, `parseUAT`, `uatSection`, the
`## 🤖 UAT checklist` heading, the confidence-gate sentinels, and the
pipeline's non-blocking contract.

## Testing

- **Golden prompts** pin the exact rendered rule text for both changed
  blocks, the same mechanism #44/#45 relied on.
- **Format block tests** assert the new rules are present and the old
  "no headings" constraint is gone from `uat-format`.
- **Existing suites** (`TestEveryTemplateRenders`,
  `TestEveryPromptFileOnDiskIsParsed`, `TestBothRoutesShareTheAskFormatBlock`)
  need no changes — both blocks stay shared, single-source templates.
- The checklist and question text themselves are model-generated and not
  unit-testable; the golden prompt is the contract, same as #44/#45.

## Risks

The model may occasionally put an item in the wrong group (e.g., a recoverable
validation error filed under Happy path). That is a minor, self-correcting
quality issue, not a coverage loss, and is acceptable for the same reason #44
accepted occasional long items: enforcing it in code would reintroduce the
complexity this design avoids.
