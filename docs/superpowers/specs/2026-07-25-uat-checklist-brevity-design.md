# UAT checklist brevity — design

Issue: #44 — *optimize: make the UAT output more simpler to read and quick-check*

## Problem

The published `## 🤖 UAT checklist` is correct but slow to scan. Today's prompt
rules ask for `###` group headings and "one concrete action plus the one
observable result" written as a sentence, so a typical checklist runs several
screens: prose items that wrap to two lines, split across four or five headings.
A human opening the issue cannot see the whole verification pass at once.

The `ask-format` block used by the confidence pushback comment is the model to
follow: a tight, rule-bound format that produces something a human can read in
one glance.

## Decisions

Settled with the issue author:

1. **Coverage is not traded for length.** Every behavior the spec (or fix)
   describes still gets an item, including error and edge cases. Only the
   wording is compressed.
2. **Item shape is `Action → expected result`, 15 words or fewer**, no prose
   sentences.
3. **Flat list.** The `###` group headings are dropped; the checklist is a
   single ungrouped list of `- [ ]` checkboxes.
4. **No enforcement outside the prompt.** `maxUATChars` stays at 8000, and there
   is no new "too long, drop it" path. Brevity is achieved the same way the
   pushback comment achieves it: by the prompt rules alone.
5. **Both routes, bare heading.** The feature and bug checklists follow
   identical rules, and `## 🤖 UAT checklist` gains no lead sentence.

## Design

### Shared format block

The two UAT prompts today carry near-identical copies of the rules list, which
is how they drifted into being long in the first place. Extract the rules into a
single template block, mirroring `ask-format`:

- New file `ai/prompts/uat-format.md.tmpl` containing `{{define "uat-format"}}`.
- `uat-feature.md.tmpl` and `uat-bug.md.tmpl` each end with
  `{{template "uat-format" .}}` in place of their own rules list.
- The one route-specific line — what "every behavior" means — is passed in as a
  `UATCoverage` value set by `uatFeaturePrompt` / `uatBugPrompt`, alongside the
  `SpecPath` / `Issue` values they already set.

Everything else in the two prompts (the framing paragraph, the diff inspection
and empty-diff self-skip on the bug route, the sentinel output contract) is
unchanged.

### The new rules

The block reads:

```
Rules for the checklist:
- A single flat list of Markdown `- [ ]` checkboxes. No headings, no grouping,
  no intro line, no closing line.
- Each item is `Action → expected result`: what the human does, then the one
  thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover <coverage>, including its error and edge cases,
  but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the
  check it makes.
- Do not modify, create, or commit any file.
```

`<coverage>` renders as `every behavior the spec describes` (feature) or `the
reported bug and every behavior the fix touches` (bug).

The "aim for under 20 items" line is removed: it is a cap on coverage, which
decision 1 rules out, and the per-item word limit is now the brevity lever.

### Shape of the output

Before:

```markdown
### Preflight guards

- [ ] Run the release command from a non-`main` branch — it reports that the
      branch check failed and stops without tagging or committing.
```

After:

```markdown
- [ ] Run from a non-`main` branch → reports branch check failed, stops
```

Same check, same coverage, one line.

## Files touched

- `ai/prompts/uat-format.md.tmpl` — new, holds the shared rules block.
- `ai/prompts/uat-feature.md.tmpl`, `ai/prompts/uat-bug.md.tmpl` — replace the
  inline rules with the shared block.
- `uat.go` — `uatFeaturePrompt` / `uatBugPrompt` set `UATCoverage`.
- `prompts_test.go` — register `uat-format` in `promptTestData`, add
  `uat-format.md.tmpl` to `skipTemplates` (it is a container file), and add a
  test asserting the block still carries its rules, paralleling
  `TestAskFormatBlockCarriesItsRules`.
- `prompts_golden_test.go` — update the two golden prompt strings.
- `docs/how-it-works.md` — one sentence describing the checklist's shape.

Not touched: `maxUATChars`, `maxIssueBodyChars`, `parseUAT`, `uatSection`, the
`## 🤖 UAT checklist` heading, and the pipeline's non-blocking contract.

## Testing

- **Golden prompts** — `TestGoldenUATFeaturePrompt` and
  `TestGoldenUATBugPrompt` assert the full rendered prompt text, so the new
  rules and the correct per-route coverage line are both pinned.
- **Format block** — a new `TestUATFormatBlockCarriesItsRules` asserts the block
  contains `15 words or fewer`, `Action → expected result`, `No headings`, and
  `Compress wording, never coverage`, so a silent deletion of one rule fails.
- **Shared, not duplicated** — assert both `uatFeaturePrompt` and `uatBugPrompt`
  contain the rendered `uat-format` block, the way the `ask-format` test does.
- **Existing suites** — `TestEveryTemplateRenders` and
  `TestEveryPromptFileOnDiskIsParsed` cover the new file once it is registered.

The output itself is model-generated, so it is not unit-testable; the golden
prompt is the contract, and the checklist on this issue is the acceptance
evidence.

## Risks

The model may ignore the 15-word limit on a behavior that genuinely needs more
words. That is acceptable: an occasional long item is better than a dropped
check, and decision 4 rules out enforcing length in code.
