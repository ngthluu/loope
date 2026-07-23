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

func TestGoldenReworkPrompt(t *testing.T) {
	want := `Continue the work on this issue where the previous session left off.
Complete the remaining implementation, make the full test suite pass, and commit
all changes. HEADLESS: do not ask questions; make reasonable calls and note them
in commit messages.

If you find the work is already fully implemented, do not fabricate changes:
print PIPELINE_ALREADY_DONE: <one-sentence reason> on its own line and stop.`
	check(t, "reworkPrompt", reworkPrompt(), want)
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
		"🤖 Picked up (feature flow). Branch: `ai/issue-12`")
}

func TestGoldenAlreadyDoneComment(t *testing.T) {
	check(t, "alreadyDoneComment", alreadyDoneComment("The flag already exists."),
		"🤖 Already implemented — closing. The flag already exists.")
}

func TestGoldenNeedsInfoComment(t *testing.T) {
	check(t, "needsInfoComment", needsInfoComment(42, "ai-needs-info", "Which database?"),
		"🤖 Not confident enough to implement (confidence 42/100). Please clarify and remove the `ai-needs-info` label to re-queue:\n\nWhich database?")
}

func TestGoldenParkCommentFull(t *testing.T) {
	check(t, "parkComment(guidance+error)", parkComment(12, "Cause: network outage — the loop auto-resumes when connectivity returns.", "dial tcp: i/o timeout"),
		"🤖 Parked for rework — run `loop -rework 12 -config <cfg>`.\nCause: network outage — the loop auto-resumes when connectivity returns.\nError: dial tcp: i/o timeout")
}

func TestGoldenParkCommentNoGuidance(t *testing.T) {
	check(t, "parkComment(error only)", parkComment(12, "", "boom"),
		"🤖 Parked for rework — run `loop -rework 12 -config <cfg>`.\nError: boom")
}

func TestGoldenParkCommentNoError(t *testing.T) {
	check(t, "parkComment(guidance only)", parkComment(12, "Cause: x.", ""),
		"🤖 Parked for rework — run `loop -rework 12 -config <cfg>`.\nCause: x.")
}

func TestGoldenParkCommentBare(t *testing.T) {
	check(t, "parkComment(bare)", parkComment(12, "", ""),
		"🤖 Parked for rework — run `loop -rework 12 -config <cfg>`.")
}

func TestGoldenPRComment(t *testing.T) {
	check(t, "prComment", prComment("https://example.test/pr/1"), "🤖 PR: https://example.test/pr/1")
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
		{"session limit reached", "Cause: Claude usage/rate limit — the loop auto-resumes it (with backoff) once the limit resets."},
		{"hit max_turns", "Cause: hit the turn/budget ceiling mid-run — the loop auto-resumes where it stopped (raise the execute maxTurns/maxBudgetUSD if this recurs)."},
		{"interrupted mid-run", "Cause: the daemon restarted while this issue was mid-run — the loop auto-resumes the preserved session."},
		{"dial tcp: i/o timeout", "Cause: network outage — the loop auto-resumes when connectivity returns."},
	}
	for _, tc := range cases {
		got, resumable := classifyCause(tc.msg)
		if !resumable {
			t.Errorf("classifyCause(%q) resumable = false, want true", tc.msg)
		}
		check(t, "classifyCause("+tc.msg+")", got, tc.want)
	}
}
