package main

import "testing"

// Golden expectations for every prompt builder, written against the original
// fmt.Sprintf implementations. Externalizing the text into ai/prompts/ must
// leave every one of them byte-identical.

func check(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestGoldenBrainstormPromptWithThreshold(t *testing.T) {
	want := `/superpowers:brainstorming ISSUE BODY

Before anything else, assess how confidently this issue can be implemented as
written and print CONFIDENCE: <0-100> as the FIRST line of your reply. If that score is
below 70, the issue is too under-specified or ambiguous to implement
responsibly: do NOT design or write a spec. Instead, list what is missing and
the specific questions the author must answer, then stop.

Write that reply as a short, skimmable list the author can answer in one comment:
- Open with ONE sentence naming the single thing that blocks you most.
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Write each question in short, plain sentences: common words, one idea per sentence, no jargon.
- Where plausible answers are guessable, offer them inline as ` + "`a) … b) … c) …`" + ` so the author can reply "1a, 2c".
- At most 5 questions. If more gaps exist, MERGE related gaps into one question — never drop one. Every ambiguity that lowered the score must stay answerable from the list.
- Under 200 words total.
- Nothing else: no preamble, no restatement of the issue, no account of what you read or explored, no code blocks, no closing pleasantries.

HEADLESS MODE: your interlocutor is an automated product-owner agent, not a human.
Ask clarifying questions as plain text (AskUserQuestion is disabled).
Follow the brainstorming flow to a committed spec: clarifying questions, design,
then write and commit the spec document into this branch. Do NOT invoke the
writing-plans skill — a separate session writes the implementation plan.
When the spec file is written and committed, print SPEC_READY: <path> on its own line,
where <path> is the spec file path relative to the repository root.

If during brainstorming you determine the feature is already fully implemented
in this codebase, do not invent work: print PIPELINE_ALREADY_DONE: <one-sentence reason> on its own
line instead of continuing.`
	check(t, "brainstormPrompt(threshold=70)", brainstormPrompt("ISSUE BODY", 70), want)
}

func TestGoldenBrainstormPromptWithoutThreshold(t *testing.T) {
	want := `/superpowers:brainstorming ISSUE BODY

HEADLESS MODE: your interlocutor is an automated product-owner agent, not a human.
Ask clarifying questions as plain text (AskUserQuestion is disabled).
Follow the brainstorming flow to a committed spec: clarifying questions, design,
then write and commit the spec document into this branch. Do NOT invoke the
writing-plans skill — a separate session writes the implementation plan.
When the spec file is written and committed, print SPEC_READY: <path> on its own line,
where <path> is the spec file path relative to the repository root.

If during brainstorming you determine the feature is already fully implemented
in this codebase, do not invent work: print PIPELINE_ALREADY_DONE: <one-sentence reason> on its own
line instead of continuing.`
	check(t, "brainstormPrompt(threshold=0)", brainstormPrompt("ISSUE BODY", 0), want)
}

func TestGoldenAnswererPrompt(t *testing.T) {
	want := `You are the product owner's proxy in an automated development pipeline.

The GitHub issue being implemented:
ISSUE BODY

Product owner preferences (persona):
PERSONA TEXT

The architect agent said:
ARCHITECT MSG

Instructions: if the architect asked questions, answer them decisively.
If it presented a design or spec for approval, approve it or give concise feedback.
Reply with your answer only.`
	check(t, "answererPrompt", answererPrompt("ISSUE BODY", "PERSONA TEXT", "ARCHITECT MSG"), want)
}

func TestGoldenDoneConfirmPrompt(t *testing.T) {
	want := `You are the product owner's proxy in an automated development pipeline.

The GitHub issue being implemented:
ISSUE BODY

Product owner preferences (persona):
PERSONA TEXT

The architect claims this issue is ALREADY fully implemented, for this reason:
REASON TEXT

Instructions: judge whether that claim is consistent with the issue and the
product owner's intent. If you agree the work is already done, reply with
exactly DONE_CONFIRMED and nothing else. If you disagree or have doubts, do NOT print that
token — instead reply with one concise sentence telling the architect what is
still missing or must be designed.`
	check(t, "doneConfirmPrompt", doneConfirmPrompt("ISSUE BODY", "PERSONA TEXT", "REASON TEXT"), want)
}

func TestGoldenPlanPrompt(t *testing.T) {
	want := `/superpowers:writing-plans Read the approved spec at docs/spec.md and
write a detailed implementation plan for it. Commit the plan into this branch.
HEADLESS MODE: do not ask questions; the spec is approved and complete — make
reasonable calls and note any assumptions in the plan.
When the implementation plan file is written and committed, print PIPELINE_READY on its own
line.`
	check(t, "planPrompt", planPrompt("docs/spec.md"), want)
}

func TestGoldenExecutePrompt(t *testing.T) {
	want := `/superpowers:executing-plans Execute the plan at docs/plan.md.
Use the execution style the plan recommends (subagent-driven or inline).
Follow TDD per the plan. Commit as you complete tasks.
HEADLESS: do not ask questions; make reasonable calls and note them in commit messages.`
	check(t, "executePrompt", executePrompt("docs/plan.md"), want)
}

func TestGoldenBugPromptWithThreshold(t *testing.T) {
	want := `/superpowers:systematic-debugging ISSUE BODY

You may read the codebase first to investigate — but do NOT write code, tests,
or commits yet. Once you understand the failure, assess how confidently this bug
can be fixed as reported and print CONFIDENCE: <0-100> as the FIRST line of your reply.
Score the report, not the repair: a bug described precisely enough to act on
scores high however large the fix, and it still scores high when investigation
shows the behavior is already correct — that is a finding about the code, not a
gap in the report. Score low only when you cannot tell what behavior is wrong.
If that score is below 70, the report is too vague or ambiguous to fix
responsibly: change no file. Instead, list what is missing and the specific
questions the author must answer, then stop.
The CONFIDENCE: line comes first even when an instruction below tells you to
print another sentinel and stop.

Write that reply as a short, skimmable list the author can answer in one comment:
- Open with ONE sentence naming the single thing that blocks you most.
- Then a numbered list of questions, most-blocking first, one sentence each, each ending in a question mark.
- Write each question in short, plain sentences: common words, one idea per sentence, no jargon.
- Where plausible answers are guessable, offer them inline as ` + "`a) … b) … c) …`" + ` so the author can reply "1a, 2c".
- At most 5 questions. If more gaps exist, MERGE related gaps into one question — never drop one. Every ambiguity that lowered the score must stay answerable from the list.
- Under 200 words total.
- Nothing else: no preamble, no restatement of the issue, no account of what you read or explored, no code blocks, no closing pleasantries.

Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
PIPELINE_ALREADY_DONE: <one-sentence reason> on its own line and stop.`
	check(t, "bugPrompt(threshold=70)", bugPrompt("ISSUE BODY", 70), want)
}

func TestGoldenBugPromptWithoutThreshold(t *testing.T) {
	want := `/superpowers:systematic-debugging ISSUE BODY

Reproduce the bug with a failing test first, then fix it, verify the full test
suite passes, and commit. HEADLESS: do not ask questions; make reasonable calls
and note them in commit messages.

If, while reproducing, you find the described bug is already fixed or the
behavior is already correct, do NOT fabricate a change: print
PIPELINE_ALREADY_DONE: <one-sentence reason> on its own line and stop.`
	check(t, "bugPrompt(threshold=0)", bugPrompt("ISSUE BODY", 0), want)
}

func TestGoldenTriagePrompt(t *testing.T) {
	want := `You are a triage agent for an automated development pipeline.

Open eligible issues:
[LIST]

Decide from the issue text alone — do NOT read the repository. Pick the single
best issue to work on next and classify it:
- "bug": a small, well-scoped defect that can be fixed by reproducing and debugging
- "feature": anything that needs design work (new functionality, refactors, unclear scope)

Respond with ONLY a JSON object, no other text:
{"issueNumber": <int>, "kind": "bug" or "feature", "reason": "<one sentence>"}`
	check(t, "triagePrompt", triagePrompt("[LIST]"), want)
}

func TestGoldenPickupComment(t *testing.T) {
	check(t, "pickupComment", pickupComment("feature", "ai/issue-12"),
		"🤖 Picked up (feature flow). Branch: `ai/issue-12`\n\n"+botMarker)
}

func TestGoldenAlreadyDoneComment(t *testing.T) {
	check(t, "alreadyDoneComment", alreadyDoneComment("The flag already exists."),
		"🤖 Already implemented — closing. The flag already exists.\n\n"+botMarker)
}

func TestGoldenNeedsInfoComment(t *testing.T) {
	check(t, "needsInfoComment", needsInfoComment(42, "ai-needs-info", "Which database?"),
		"🤖 Not confident enough to implement (confidence 42/100). Answer the numbered questions below in a comment, then remove the `ai-needs-info` label to re-queue:\n\nWhich database?")
}

const parkHead = "\U0001f916 Parked as `ai-rework` — this issue will not be retried automatically.\n\n" +
	"Remove the `ai-rework` label to queue a fresh attempt — any worktree, branch and logs this run produced are preserved and reused, so no work is lost."

// parkTail is the hidden marker every status comment ends with, so
// FetchIssueContent can strip it back out of the next run's issue content.
const parkTail = "\n\n" + botMarker

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
	check(t, "prComment", prComment("https://example.test/pr/1"), "🤖 PR: https://example.test/pr/1\n\n"+botMarker)
}

func TestGoldenPlanComment(t *testing.T) {
	check(t, "planComment", planComment("docs/superpowers/plans/2026-plan.md"),
		"🤖 Updated plan: `docs/superpowers/plans/2026-plan.md`\n\n"+botMarker)
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
		{"hit max_turns", "Cause: hit the turn/budget ceiling mid-run. Raise the execute maxTurns/maxBudgetUSD if this recurs."},
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

Output ONLY the checklist, between a line reading UAT_BEGIN and a line reading
UAT_END. Print nothing before or after those two lines.

Rules for the checklist:
- Two group headings only, in this order: ` + "`### Happy path`" + `, then ` + "`### Edge cases`" + `. Omit a group with no items; never invent items to fill it. No other headings, no intro line, no closing line.
- Each item is ` + "`Action → expected result`" + `: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
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
describes the real fix. If that diff is empty — nothing was committed — print
nothing at all: no markers, no checklist, no explanation.

Output ONLY the checklist, between a line reading UAT_BEGIN and a line reading
UAT_END. Print nothing before or after those two lines.

Rules for the checklist:
- Two group headings only, in this order: ` + "`### Happy path`" + `, then ` + "`### Edge cases`" + `. Omit a group with no items; never invent items to fill it. No other headings, no intro line, no closing line.
- Each item is ` + "`Action → expected result`" + `: what the human does, then the one thing they should see. 15 words or fewer. Not a sentence.
- No implementation detail, no file paths, no code.
- One item per behavior. Cover the reported bug and every behavior the fix touches, including its error and edge cases, but do not invent scope beyond it.
- Compress wording, never coverage. An item that runs long loses words, not the check it makes.
- Do not modify, create, or commit any file.`
	check(t, "uatBugPrompt", uatBugPrompt("ISSUE BODY", "main"), want)
}

func TestGoldenCodeReviewPrompt(t *testing.T) {
	want := `Run /code-review against origin/main...HEAD with --fix applied (round 1 of 2), then commit any changes it makes.

Output ONLY a status line and summary between a line reading CODEREVIEW_BEGIN and a line reading
CODEREVIEW_END. Print nothing before or after those two lines. The
first line between them is one of:
- STATUS: clean — /code-review found nothing to fix.
- STATUS: fixed — followed by a short bullet summary of what was fixed.
- STATUS: blocked — followed by a short explanation of what can't be safely auto-fixed.

HEADLESS MODE: do not ask questions; make reasonable calls.`
	check(t, "codeReviewPrompt", codeReviewPrompt(1, 2, "main"), want)
}

func TestGoldenCodeReviewComment(t *testing.T) {
	check(t, "codeReviewComment", codeReviewComment(1, 2, codeReviewFixed, "- Fixed a null check."),
		"<!-- loope:codereview:1 -->\n🤖 Code review round 1/2: fixed\n\n- Fixed a null check.")
}
