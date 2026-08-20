package engine

import (
	"testing"

	"github.com/ngthluu/loope/worker/shared"
)

// Golden expectations for every prompt builder: the exact text each template
// renders. A deliberate prompt edit updates the golden here in the same
// change; an accidental one (a typo, a partial that drifted) fails loudly.

func check(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

const goldenEntryOutcomes = `Outcomes — your final structured output reports where the session ended:
- "fix_committed": a fix for this issue is committed on this branch; one-sentence summary of the fix in detail.
- "spec_ready": the spec document for this issue is written and committed on this branch; its path relative to the repository root in spec_path, one-sentence summary in detail.
- "already_done": the issue's work is already fully implemented in this codebase — do not invent work; one-sentence reason in detail.
- "question": you need an answer or approval from the product owner before reaching a terminal outcome (also used for a status update); the full question or message in detail.`

const goldenEntryRole = `Handle this GitHub issue:
ISSUE BODY

## Environment
- You are the architect session of an automated development pipeline, scoped to this issue only, in a dedicated git worktree on a per-issue branch created from the repository's base branch (origin/<base>). Commit your work on this branch.
- The pipeline pushes the branch and opens the PR. Never push, open PRs, switch branches, rebase, amend, or force-push.
- Write spec documents under a ` + "`specs/`" + ` directory (e.g. ` + "`docs/superpowers/specs/`" + `) and report the repository-relative path in spec_path.
- HEADLESS MODE: AskUserQuestion is disabled. To ask anything, end the turn with outcome "question" and the question in detail; the answer arrives as the next message.`

const goldenEntryRoutes = `## Process
Investigate the repository first, then commit to whichever route fits:
- A small, well-scoped defect with a clear expected behavior: follow the
  systematic-debugging flow — reproduce it with a failing test first, then fix
  it, verify the full test suite passes, and commit. Make reasonable calls and
  note them in commit messages. End with "fix_committed".
- Anything that needs design work (new functionality, refactors, unclear
  scope — including a "fix" whose right behavior first needs designing):
  follow the brainstorming flow to a committed spec — clarifying questions,
  design, then write and commit the spec document. Do NOT invoke the
  writing-plans skill; a separate session writes the implementation plan.
  End with "spec_ready".
- If the issue's work is already fully implemented, end with "already_done"
  instead of continuing.

## Output
` + goldenEntryOutcomes

func TestGoldenEntryPromptWithThreshold(t *testing.T) {
	want := goldenEntryRole + ` A below-threshold confidence "question" (see the gate below) goes to the issue's human author as a needs-info comment, and the pipeline pauses until they reply; any later "question" is answered by the automated product-owner proxy, not a human.

## Confidence gate (first turn)
You may read the codebase to investigate, but do NOT write code, tests, or
commits yet. Once you understand the issue, report how confidently it can be
handled as written in the confidence field (0-100) of this turn's structured
output. Score the issue, not the work: an issue described precisely enough to
act on scores high however large the change, and it still scores high when
investigation shows the work is already done — that is a finding about the
code, not a gap in the issue. Score low only when you cannot tell what
behavior or outcome is wanted. If the score is below 70, the issue
is too under-specified to act on responsibly: change no file, end the turn
with outcome "question" with what is missing and the specific questions the
author must answer in detail, then stop. Report the confidence field even
when an instruction below has you end with another outcome.

Write that detail text as a short, skimmable list the author can answer in one comment:
- Open with ONE sentence naming the single thing that blocks you most.
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Write each question in short, plain sentences: common words, one idea per sentence, no jargon.
- Where plausible answers are guessable, offer them inline as ` + "`a) … b) … c) …`" + ` so the author can reply "1a, 2c".
- At most 5 questions. If more gaps exist, MERGE related gaps into one question — never drop one. Every ambiguity that lowered the score must stay answerable from the list.
- Under 200 words total.
- Nothing else: no preamble, no restatement of the issue, no account of what you read or explored, no code blocks, no closing pleasantries.

` + goldenEntryRoutes
	check(t, "entryPrompt(threshold=70)", entryPrompt("ISSUE BODY", 70), want)
}

func TestGoldenEntryPromptWithoutThreshold(t *testing.T) {
	want := goldenEntryRole + ` Every "question" is answered by the automated product-owner proxy, not a human.

` + goldenEntryRoutes
	check(t, "entryPrompt(threshold=0)", entryPrompt("ISSUE BODY", 0), want)
}

func TestGoldenEntryResumePrompt(t *testing.T) {
	want := `TRIGGER MSG

HEADLESS MODE reminder: your interlocutor is an automated product-owner agent, not a human.
This resumed session keeps the same contract, scoped to THIS issue only: do not
work on other issues — planning and implementation of a designed feature happen
in separate pipeline sessions. If the fix or spec was already committed in an
earlier turn, report that outcome now instead of redoing the work.

` + goldenEntryOutcomes
	check(t, "entryResumePrompt", entryResumePrompt("TRIGGER MSG"), want)
}

const goldenPOPreamble = `You are the product owner's proxy in an automated development pipeline.
You are read-only: do not modify, create, or commit any file in the repository.

The GitHub issue being implemented:
ISSUE BODY

Product owner preferences (persona):
PERSONA TEXT
`

func TestGoldenAnswererPrompt(t *testing.T) {
	want := goldenPOPreamble + `
The architect agent said:
ARCHITECT MSG

If the architect asked questions, answer them decisively. If it presented a
design or spec for approval, approve it or give concise feedback. Stay on this
issue: never direct the architect to implement, merge, or pick up other issues —
planning and implementation are handled by separate pipeline sessions.
Report your reply in your final structured output: if the message asks for no
answer and no approval (e.g. a status or progress update), report has_answer
false. Otherwise report has_answer true with your answer — and nothing else —
in answer.`
	check(t, "answererPrompt", answererPrompt("ISSUE BODY", "PERSONA TEXT", "ARCHITECT MSG"), want)
}

func TestGoldenBrainstormResumePrompt(t *testing.T) {
	want := `TRIGGER MSG

HEADLESS MODE reminder: your interlocutor is an automated product-owner agent, not a human.
This resumed design session keeps the same contract, scoped to THIS issue only:
do not merge or work on other issues — planning and implementation of a
designed feature happen in separate pipeline sessions. If the spec was already
committed in an earlier turn, report "spec_ready" now instead of redoing the work.

` + goldenEntryOutcomes
	check(t, "brainstormResumePrompt", brainstormResumePrompt("TRIGGER MSG"), want)
}

func TestGoldenQANudgePrompt(t *testing.T) {
	want := `No decision was requested, so there is nothing to answer. Continue toward this
issue's terminal outcome — commit the fix, or finish and commit the spec — and
do not start work on other issues in this session.

` + goldenEntryOutcomes
	check(t, "qaNudgePrompt", qaNudgePrompt(), want)
}

func TestGoldenDoneConfirmPrompt(t *testing.T) {
	want := goldenPOPreamble + `
The architect claims this issue is ALREADY fully implemented, for this reason:
REASON TEXT

Judge whether that claim is consistent with the issue and the product owner's
intent, and report the verdict in your final structured output: confirmed true
if you agree the work is already done; otherwise confirmed false, with one
concise sentence in objection telling the architect what is still missing or
must be designed.`
	check(t, "doneConfirmPrompt", doneConfirmPrompt("ISSUE BODY", "PERSONA TEXT", "REASON TEXT"), want)
}

func TestGoldenPlanPrompt(t *testing.T) {
	want := `/superpowers:writing-plans Read the approved spec at docs/spec.md and
write a detailed implementation plan for it. Commit the plan into this branch;
do not push.
HEADLESS MODE: do not ask questions; the spec is approved and complete — make
reasonable calls and note any assumptions in the plan.
When the implementation plan file is written and committed, end the session:
your final structured output must report status "ready" and the plan file path
(relative to the repository root) as plan_path. Report status "incomplete"
(with a brief detail) only if you could not write and commit the plan.`
	check(t, "planPrompt", planPrompt("docs/spec.md"), want)
}

func TestGoldenExecutePrompt(t *testing.T) {
	want := `/superpowers:executing-plans Execute the plan at docs/plan.md inline in this session.
If tasks are already committed on this branch (check ` + "`git log`" + `), continue from the first incomplete task; do not redo completed tasks.
Follow TDD per the plan. Commit after each task; do not push.
HEADLESS MODE: do not ask questions; make reasonable calls and note them in commit messages.
When every task in the plan is implemented and committed, end the session: your
final structured output must report status "complete". Report status
"incomplete" (with a brief detail naming what remains and why) only if you
could not finish every task — never report "complete" with work left undone.`
	check(t, "executePrompt", executePrompt("docs/plan.md"), want)
}

func TestGoldenPickupComment(t *testing.T) {
	check(t, "pickupComment", pickupComment("ai/issue-12"),
		"🤖 Picked up. Branch: `ai/issue-12`\n\n"+shared.BotMarker)
}

func TestGoldenAlreadyDoneComment(t *testing.T) {
	check(t, "alreadyDoneComment", alreadyDoneComment("The flag already exists."),
		"🤖 Already implemented — closing. The flag already exists.\n\n"+shared.BotMarker)
}

func TestGoldenNeedsInfoComment(t *testing.T) {
	check(t, "needsInfoComment", needsInfoComment(42, "ai-needs-info", "Which database?"),
		"🤖 Not confident enough to implement (confidence 42/100). Answer the numbered questions below in a comment, then remove the `ai-needs-info` label to re-queue:\n\nWhich database?")

	check(t, "needsInfoComment (stalled, no score)", needsInfoComment(noConfidenceScore, "ai-needs-info", "Want me to proceed?"),
		"🤖 The session ended without committing a fix. Its closing note or questions are below — reply in a comment (answering any questions), then remove the `ai-needs-info` label to re-queue:\n\nWant me to proceed?")
}

const parkHead = "\U0001f916 Parked as `ai-rework` — this issue will not be retried automatically.\n\n" +
	"Remove the `ai-rework` label to queue a fresh attempt — any worktree, branch and logs this run produced are preserved and reused, so no work is lost."

// parkTail is the hidden marker every status comment ends with, so
// FetchIssueContent can strip it back out of the next run's issue content.
const parkTail = "\n\n" + shared.BotMarker

func TestGoldenParkCommentFull(t *testing.T) {
	check(t, "parkComment(guidance+error)", parkComment("ai-rework", "Cause: network outage. Re-queue once connectivity is back.", "dial tcp: i/o timeout"),
		parkHead+"\n\nCause: network outage. Re-queue once connectivity is back."+
			"\n\n<details><summary>Error detail</summary>\n\n````\ndial tcp: i/o timeout\n````\n\n</details>"+parkTail)
}

func TestGoldenParkCommentNoGuidance(t *testing.T) {
	check(t, "parkComment(error only)", parkComment("ai-rework", "", "boom"),
		parkHead+"\n\n<details><summary>Error detail</summary>\n\n````\nboom\n````\n\n</details>"+parkTail)
}

func TestGoldenParkCommentNoError(t *testing.T) {
	check(t, "parkComment(guidance only)", parkComment("ai-rework", "Cause: x.", ""),
		parkHead+"\n\nCause: x."+parkTail)
}

func TestGoldenParkCommentBare(t *testing.T) {
	check(t, "parkComment(bare)", parkComment("ai-rework", "", ""), parkHead+parkTail)
}

func TestGoldenPRComment(t *testing.T) {
	check(t, "prComment", prComment("https://example.test/pr/1"), "🤖 PR: https://example.test/pr/1\n\n"+shared.BotMarker)
}

func TestGoldenPlanComment(t *testing.T) {
	check(t, "planComment", planComment("docs/superpowers/plans/2026-plan.md"),
		"🤖 Updated plan: `docs/superpowers/plans/2026-plan.md`\n\n"+shared.BotMarker)
}

func TestGoldenPRTitle(t *testing.T) {
	check(t, "prTitle", prTitle("Externalize prompts", 12), "Externalize prompts (#12)")
}

func TestGoldenPRBody(t *testing.T) {
	check(t, "prBody", prBody(12, "feature"),
		"Closes #12\n\nAutomated by loope (feature flow). Spec and plan, if any, are committed in this branch under docs/.")
}

func TestGoldenClassifyCauseGuidance(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"session limit reached", "Cause: Claude usage/rate limit. Re-queue once the limit resets."},
		{"hit max_turns", "Cause: hit the turn/budget ceiling mid-run. Re-queue to continue from the recorded session."},
		{"dial tcp: i/o timeout", "Cause: network outage. Re-queue once connectivity is back."},
	}
	for _, tc := range cases {
		check(t, "classifyCause("+tc.msg+")", classifyCause(tc.msg), tc.want)
	}
}

func TestGoldenUATSection(t *testing.T) {
	check(t, "uatSection", uatSection("- [ ] Run the thing and see the thing."),
		"<!-- loope:uat -->\n\n## 🤖 UAT checklist\n\n- [ ] Run the thing and see the thing.")
}

func TestGoldenUATFeaturePrompt(t *testing.T) {
	want := `Read the approved spec at docs/spec.md and write a UAT (user acceptance test)
checklist for a human who will verify the shipped feature by hand.

Return ONLY the checklist, as the checklist field of your final structured
output — nothing before or after it inside that field.

Rules for the checklist:
- Two group headings only, in this order: ` + "`### Happy path`" + `, then ` + "`### Edge cases`" + `. Omit a group with no items; never invent items to fill it. No other headings, no intro line, no closing line.
- Each item is a GitHub task-list checkbox: ` + "`- [ ] `" + ` followed by ` + "`Action → expected result`" + `: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover every behavior the spec describes, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.`
	check(t, "uatFeaturePrompt", uatFeaturePrompt("docs/spec.md"), want)
}

func TestGoldenUATBugPrompt(t *testing.T) {
	want := `A bug fix has just been committed on this branch. Write a UAT (user acceptance
test) checklist for a human who will verify the fix by hand.

The GitHub issue being fixed:
ISSUE BODY

Read the issue above, then inspect what actually changed with
` + "`git diff origin/main...HEAD`" + ` and ` + "`git log origin/main..HEAD`" + `, so the checklist
describes the real fix. If that diff is empty — nothing was committed — return
an empty checklist: no items, no explanation.

Return ONLY the checklist, as the checklist field of your final structured
output — nothing before or after it inside that field.

Rules for the checklist:
- Two group headings only, in this order: ` + "`### Happy path`" + `, then ` + "`### Edge cases`" + `. Omit a group with no items; never invent items to fill it. No other headings, no intro line, no closing line.
- Each item is a GitHub task-list checkbox: ` + "`- [ ] `" + ` followed by ` + "`Action → expected result`" + `: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover the reported bug and every behavior the fix touches, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.`
	check(t, "uatBugPrompt", uatBugPrompt("ISSUE BODY", "main"), want)
}

func TestGoldenCodeReviewPrompt(t *testing.T) {
	want := `Run /code-review against origin/main...HEAD with --fix applied (round 1 of 2), then commit any changes it makes.

Report the outcome in your final structured output:
- status "clean" — /code-review found nothing to fix (empty summary).
- status "fixed" — with a short bullet summary of what was fixed in summary.
- status "blocked" — with a short explanation of what can't be safely
  auto-fixed in summary.

HEADLESS MODE: do not ask questions; make reasonable calls.`
	check(t, "codeReviewPrompt", codeReviewPrompt(1, 2, "main"), want)
}

func TestGoldenCodeReviewComment(t *testing.T) {
	check(t, "codeReviewComment", codeReviewComment(1, 2, codeReviewFixed, "- Fixed a null check."),
		"<!-- loope:codereview:1 -->\n🤖 Code review round 1/2: fixed\n\n- Fixed a null check.")
}

func TestGoldenMergeResolvePickupComment(t *testing.T) {
	check(t, "mergeResolvePickupComment", mergeResolvePickupComment("main", "ai/issue-12"),
		"🤖 Merge-resolve requested: merging `origin/main` into `ai/issue-12`.\n\n"+shared.BotMarker)
}

func TestGoldenMergeResolveDoneComment(t *testing.T) {
	check(t, "mergeResolveDoneComment(summary)", mergeResolveDoneComment("main", "ai/issue-12", "resolved"),
		"🤖 Merged `origin/main` into `ai/issue-12` and pushed.\n\nresolved\n\n"+shared.BotMarker)
	check(t, "mergeResolveDoneComment(no summary)", mergeResolveDoneComment("main", "ai/issue-12", ""),
		"🤖 Merged `origin/main` into `ai/issue-12` and pushed.\n\n"+shared.BotMarker)
}

const mergeResolveParkHead = "🤖 Merge-resolve failed — parked as `ai-rework`, and it will not be retried automatically.\n\n" +
	"The `ai-resolve-merge` label has been removed so the merge is not re-attempted every cycle. " +
	"The worktree is left exactly as the failure left it (including any partially resolved conflict), so no work is lost. " +
	"To retry, re-add `ai-resolve-merge` — the next run continues from that state. " +
	"Do NOT remove `ai-rework` expecting a merge retry: that instead queues a fresh, unrelated pipeline run against this same worktree."

func TestGoldenMergeResolveParkCommentFull(t *testing.T) {
	check(t, "mergeResolveParkComment(guidance+error)",
		mergeResolveParkComment("ai-rework", "ai-resolve-merge", "Cause: x.", "boom"),
		mergeResolveParkHead+"\n\nCause: x."+
			"\n\n<details><summary>Error detail</summary>\n\n````\nboom\n````\n\n</details>\n\n"+shared.BotMarker)
}

func TestGoldenMergeResolveParkCommentErrorOnly(t *testing.T) {
	check(t, "mergeResolveParkComment(error only)",
		mergeResolveParkComment("ai-rework", "ai-resolve-merge", "", "boom"),
		mergeResolveParkHead+"\n\n<details><summary>Error detail</summary>\n\n````\nboom\n````\n\n</details>\n\n"+shared.BotMarker)
}
