# Externalize prompts into `ai/prompts/` — design

Issue: #12 — move all prompts out of the Go source into `ai/prompts/`, while the
production build stays a single binary.

## Problem

Every string loope sends to Claude, and every comment it posts to GitHub, is a
raw string literal inside a `fmt.Sprintf` in the Go source:

| Text | Location |
| --- | --- |
| brainstorm prompt (+ conditional confidence paragraph) | `pipeline_feature.go:258` |
| answerer prompt | `pipeline_feature.go:284` |
| done-confirm prompt | `pipeline_feature.go:303` |
| plan prompt | `pipeline_feature.go:322` |
| execute prompt | `pipeline_feature.go:331` |
| bug/debug prompt | `pipeline_bug.go:29` |
| rework prompt | `rework.go:61` |
| triage prompt (inline, not extracted) | `triage.go:22` |
| pickup / already-done / needs-info / park / PR comments | `loop.go:166,213,239,307,515,519` |
| `classifyCause` guidance sentences | `loop.go:258` |

Editing prompt wording means editing Go, and the prompts are hard to read
interleaved with control flow.

## Goal

The prompt text lives in `ai/prompts/`. The Go source contains no prompt text.
The released artifact remains one self-contained binary that reads nothing from
disk at runtime.

## Scope

**In scope:**

- **A — model-facing prompts:** all eight LLM prompts listed above.
- **B — human-facing outbound text:** the GitHub comment, PR title, and PR body
  templates in `loop.go`, plus the `classifyCause` guidance sentences that are
  concatenated into the park comment.

**Out of scope:**

- The dashboard HTML/JS templates in `serve.go` (`pageHead`, `railTmpl`,
  `detailTmpl`, `stepcardTmpl`) and `ui.go` (`uiJS`). These are UI markup, not
  prompts, and moving them is a separate concern.
- Any rewording of prompt text. This is a pure relocation.
- Any runtime override of the embedded prompts (no config key pointing at a
  prompts directory). Deliberately omitted as YAGNI: it adds a config surface, a
  precedence rule, and failure modes the issue does not ask for.

## Design

### 1. Layout

A flat directory at the repository root:

```
ai/prompts/
  brainstorm.md.tmpl
  answerer.md.tmpl
  done-confirm.md.tmpl
  plan.md.tmpl
  execute.md.tmpl
  debug.md.tmpl
  rework.md.tmpl
  triage.md.tmpl
  comments.md.tmpl
```

Flat, not grouped into `feature/` / `bug/` / `github/` subdirectories, for a
concrete reason: `template.ParseFS` names each parsed template by its **base
filename**, so two files named `prompt.md.tmpl` in different subdirectories
would silently shadow one another. Eight uniquely-named prompts do not need
grouping, and the flat form makes the shadowing hazard impossible.

`comments.md.tmpl` is the one multi-template file: the `loop.go` strings are
single lines, and a file per one-liner would be noise. It holds `{{define}}`
blocks:

- `pickup`, `already-done`, `needs-info`, `park`, `pr-comment`, `pr-title`,
  `pr-body`
- one block per `classifyCause` guidance sentence:
  `guidance-usage-limit`, `guidance-budget`, `guidance-interrupted`,
  `guidance-network`

### 2. Loading — `prompts.go` (new file)

```go
//go:embed ai/prompts
var promptFS embed.FS

var prompts = template.Must(
    template.New("prompts").
        Option("missingkey=error").
        ParseFS(promptFS, "ai/prompts/*.md.tmpl"),
)
```

Parsed once at package init. `missingkey=error` is required, not incidental:
without it a typo'd placeholder renders as the literal `<no value>` inside a
prompt that then gets sent to Claude, which is a silent, expensive failure. With
it, the same typo is a loud render error caught by the tests in §6.

### 3. Rendering

One helper:

```go
func mustRender(name string, data map[string]any) string
```

It executes the named template into a buffer and returns the result with the
file's trailing newline trimmed. The trim matters for correctness: text editors
end files with a newline, but the current string literals do not end with one,
so trimming is what keeps the moved output byte-identical to today's.

Template data always originates from a constructor:

```go
func promptData() map[string]any // pre-populated with every sentinel constant
```

pre-populated with `confidenceSentinel`, `specReadySentinel`, `readySentinel`,
`alreadyDoneSentinel`, and `doneConfirmSentinel`. Each builder adds its own keys
on top.

**Sentinels are never written as literal text in a `.tmpl` file.** The same
constants drive the parsers in `confidence.go`, `done.go`, and
`pipeline_feature.go`; hardcoding them in the prompt files would let the
instruction given to the model and the parser reading its reply drift apart
silently. Injecting them from the Go constants makes drift impossible.

**`mustRender` panics on error.** This is deliberate. A render failure is a
static defect — an unknown template name or a missing key — not a runtime
condition. §6 covers every template with a rendering test, so such a defect
fails the build rather than reaching a running daemon. The alternative,
threading an `error` return through eight builders whose callers have no
meaningful recovery path, adds noise and buys nothing.

### 4. Call sites

These builders keep their **exact current signatures**; each body collapses to a
single `mustRender` call:

`brainstormPrompt`, `answererPrompt`, `doneConfirmPrompt`, `planPrompt`,
`executePrompt`, `bugPrompt`, `reworkPrompt`.

`triage.go`'s prompt is currently inlined in the `Triage` function body. It is
extracted into a `triagePrompt(list string) string` builder, matching the shape
of the other seven.

`classifyCause` keeps its `(guidance string, resumable bool)` signature and its
`switch`; only the returned sentence changes from a literal to a `mustRender`
call. Control flow, the resumable booleans, and every caller are untouched.

### 5. Conditionals move into the templates

Two pieces of prompt assembly currently live in Go and move into template
syntax:

- `brainstormPrompt`'s confidence paragraph, today a second `fmt.Sprintf`
  assigned to a `confidence` variable, becomes `{{if gt .Threshold 0}}…{{end}}`
  inside `brainstorm.md.tmpl`. The Go-side fragment disappears.
- `park`'s optional guidance line and error tail, today built by `+=` string
  concatenation in `loop.go`, become `{{if}}` blocks in the `park` template.

Whitespace control (`{{-`, `-}}`) is applied as needed; §6's golden tests are
what verify it came out exactly right.

### 6. Testing — golden-first

The plan is ordered so that "verbatim relocation" is a *checked* claim rather
than an intention:

1. **First**, add `prompts_golden_test.go` asserting each builder's output
   against a full literal expectation of today's text. These tests pass against
   the current `fmt.Sprintf` implementation.
2. **Then** perform the move. The same tests must still pass byte-for-byte.

Coverage requirements:

- One golden case per builder, plus both branches of every conditional:
  `brainstormPrompt` with the confidence threshold on and off; the `park`
  comment with and without guidance, and with and without an error tail.
- A table test that renders **every** template present in the embedded FS with
  representative data. This is what makes `mustRender`'s panic safe, and it also
  catches a prompt file that was added but never wired up.
- The existing substring assertions in `pipeline_feature_test.go` (lines
  141–166, 327, 365) and `pipeline_bug_test.go` (line 23) stay untouched and
  must keep passing. They are the independent check that the pipelines still
  send what they used to.

### 7. Single binary

`go:embed` compiles the files into the binary. `.goreleaser.yaml` needs no
change: `ai/prompts/` sits inside the module and package `main` is at the
repository root, so the embed directive needs no `..` escape. No new
dependencies — `embed` and `text/template` are standard library, and `go.mod`
stays dependency-free.

## Verification

- `go build ./...`
- `go vet ./...`
- `go test ./...` — golden tests, full-FS render test, and the pre-existing
  pipeline assertions all green.
- `goreleaser release --snapshot --clean`, then run the built binary with
  `ai/prompts/` renamed away. It must behave identically, proving nothing is
  read from disk at runtime.

## Risks

- **Whitespace drift** when hand-converting `%s` interpolation and `{{if}}`
  blocks. Mitigated by the golden-first ordering: the expectations are written
  from the current output before the code changes.
- **A prompt file added without a builder**, or renamed without updating the
  lookup name. Mitigated by the full-FS render test and by `mustRender`'s
  failure on an unknown template name.
