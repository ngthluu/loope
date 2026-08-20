# Worker flow test matrix

Design matrix for whole-flow tests of the issue-processing worker: every phase
of the pipeline crossed with every way a run can be interrupted in the middle
of it, the expected recovery behavior, and the test that verifies it.

Flow-level tests (the `TestFlow*` family in `worker/engine/flow_test.go` and
`worker/engine/flow_matrix_test.go`) drive the REAL Orchestrator across
multiple poll cycles against a simulated GitHub whose label state is mutated
by the daemon's own calls, so eligibility, park, human label removal, and
resume all interact exactly as in production. Unit tests (per-file `*_test.go`)
verify each phase in isolation; the matrix lists both, preferring the
flow-level test where one exists.

## Phases

A feature issue passes through, in order:

| # | Phase | Claude session | Checkpointing |
|---|-------|----------------|---------------|
| P0 | Triage (pick issue + kind) | triage (ephemeral) | none |
| P1 | Pickup: `ai-wip` label, comment, worktree create, fetch content | — | state marker |
| P2 | Brainstorm (architect, confidence gate) | brainstorm-0 / brainstorm-resume | chain node `brainstorm` (in-flight, on session id) |
| P3 | Q&A round loop (PO-proxy answerer, done-confirm) | answer-N / done-confirm-N (ephemeral) | none (rounds resume the brainstorm session) |
| P4 | Spec→plan handoff: pending plan node, UAT spawn, spec push + PR + comment | UAT (ephemeral, background) | PENDING `plan` node (no id) before the plan call |
| P5 | Plan session (`PIPELINE_READY` + plan file) | plan | chain node `plan` (in-flight) |
| P6 | Plan→execute handoff: pending execute node, plan push + comment | — | PENDING `execute` node before the execute call |
| P7 | Execute session | execute | chain node `execute` (in-flight) |
| P8 | Ship: commit count, push, PR create/recover, PR comment | — | `pr` marker (HasPR suppresses duplicate comment) |
| P9 | Code review rounds | codereview-N | chain node `codereview` (in-flight) + round counter file |
| P10 | Done: WIP→Done swap, state marker | — | state marker |

A bug issue replaces P2–P7 with one debug session (chain stage `debug`), plus
the confidence gate, already-done gate, and zero-commit needs-info gate.

## Interruption modes

| Mode | Meaning | Recorded state |
|------|---------|----------------|
| M1 | CLI dies BEFORE its session id streams (instant crash) | no node for this session — head is the previous node or a pending node |
| M2 | CLI killed AFTER its session id streamed (usage limit kill, network drop) | chain node with the dead session's id |
| M3 | `is_error` result with a session id (HTTP 429) | chain node with the limited session's id |
| M4 | Session completes off-contract (missing sentinel, sentinel without file, no commits) | node for the completed session; failure surfaces downstream |
| M5 | Low confidence score | terminal `ai-needs-info`, session preserved |
| M6 | Architect claims already implemented | close (confirmed) or push back (objected) |
| M7 | Infra failure (git/gh call fails persistently) | park at the failing step |
| M8 | Daemon crash / restart (orphaned `ai-wip`) | SweepOrphans strips WIP, chain preserved |
| M9 | User Stop mid-run, then Continue | `ai-stopped`, chain preserved |
| M10 | Round budget exhausted (Q&A rounds) | park with the budget error |

Recovery invariants every scenario must satisfy:

- **Nothing retries automatically.** Every failure parks (`ai-rework`) or
  escalates (`ai-needs-info`) and waits for a human label change.
- **Re-entry resumes the chain head**: a node with an id gets `--resume <id>`
  (prompt `continue`, or the needs-info added-lines diff); a pending node
  (no id) re-runs its stage fresh from the recorded artifact; no head at all
  means a fully fresh pipeline.
- **Completed stages never re-run**: a re-entry past the spec must not
  brainstorm again; past the plan must not re-plan; a parked review re-enters
  ship directly (P2–P7 skipped).
- **Worktree, branch, logs, session files are never deleted.**

## Matrix — feature lane

| Phase | Interruption | Expected outcome | Verified by |
|-------|--------------|------------------|-------------|
| P0 triage | M1/M7 session fails | cycle returns the error; issue untouched (no label, no state), retried next cycle | `TestFlowTriageFailureLeavesIssueUntouched` (flow_matrix) |
| P0 triage | M4 bad JSON / unknown issue / bad kind | triage error, same as above | `TestTriageRejects*` (unit) |
| P1 pickup | M7 worktree create / fetch fails | park `ai-rework` (abort), artifacts preserved | `TestToolingFailureParksForRework`, `TestToolingFailureParksInsteadOfStayingEligible` |
| P2 brainstorm | M1 dies before session id | park; chain empty → re-entry runs a FULLY FRESH pipeline (no `--resume` anywhere) | `TestFlowBrainstormDiesBeforeSessionIdRerunsFresh` (flow_matrix) |
| P2 brainstorm | M2 killed after id streamed | park; head = brainstorm node → re-entry resumes THAT session with the sentinel-restating resume prompt; no second fresh brainstorm | `TestFlowBrainstormKilledMidSessionResumesArchitectSession` (flow_matrix) |
| P2 brainstorm | M5 low confidence | `ai-needs-info`, no design work; answer + label removal resumes the SAME session with the added-lines diff as prompt | `TestFlowNeedsInfoAnswerResumesBrainstormWithDiff` (flow_matrix); unit: `TestFeaturePipelineLowConfidenceEscalates`, `TestHandleIssueResumesWithDiffAfterNeedsInfo` |
| P2 brainstorm | M6 already done | answerer confirms → comment, WIP→Done, issue closed; objection hands back to architect | `TestProcessOnceAlreadyDoneClosesIssue`, `TestFeaturePipelineArchitectDonePushbackContinues` |
| P3 Q&A loop | M10 rounds exhausted | park with "exceeded N Q&A rounds" | `TestFlowQARoundsExhaustedParks` (flow_matrix); unit: `TestFeaturePipelineFailsAfterMaxRounds` |
| P3 Q&A loop | M4 SPEC_READY without a spec file | loop keeps prodding (round consumed), no crash | `TestFeaturePipelineSpecSentinelWithoutFileKeepsGoing` |
| P3 Q&A loop | M4 answerer has nothing to answer | nudge prompt pushes architect to a terminal sentinel | `TestBrainstormLoopNudgesArchitectWhenNothingToAnswer` |
| P4 spec handoff | M7 spec push / PR create fails | best-effort: logged and swallowed, pipeline continues | `TestBrainstormLoopContinuesWhenSpecPushFails` |
| P4 spec handoff | crash between spec and plan session | pending plan node → re-entry re-runs plan FRESH from the committed spec (no brainstorm resume — the issue-5 incident) | `TestFlowPlanDiesBeforeSessionStartsResumesFreshFromSpec` (flow) |
| P5 plan | M2/M3 killed after id | park; head = plan node → resume that plan session | `TestResumeFeaturePipelinePlanStage` (unit; same mechanism flow-verified for execute/brainstorm) |
| P5 plan | dead resume (session gone, no salvage) | fall back to a fresh plan run on the checkpointed spec | `TestResumeFeaturePipelinePlanStageDeadResumeFallsBackToArtifact` |
| P5 plan | M4 no `PIPELINE_READY` / no plan file | pipeline error → park | `TestRunPlanThenExecute*` error paths (unit, via `runPlanThenExecute` contract checks) |
| P6 execute handoff | M7 plan push/comment fails | best-effort, swallowed | `TestRunPlanThenExecutePlanPushFailureDoesNotFailPipeline` |
| P6 execute handoff | M1 execute dies before session starts | pending execute node → re-entry re-runs execute FRESH on the committed plan; plan stage NOT re-run | `TestFlowExecuteDiesBeforeSessionStartsRerunsFreshFromPlan` (flow_matrix); unit: `TestResumeFeaturePipelineExecuteStageFreshFromPlanCheckpoint` |
| P7 execute | M2 killed after id | park; head = execute node → re-entry `--resume` + `continue`, then ship | `TestFlowExecuteKilledParksThenReworkRemovalResumesSameSession` (flow) |
| P7 execute | M7 post-execute push fails | best-effort (ship's push is the backstop) | `TestExecuteStagePushFailureDoesNotFailPipeline` |
| P7 execute | M8 daemon crash mid-run | SweepOrphans strips WIP; next cycle resumes the execute session | `TestSweepOrphansThenNextCycleResumesSession` |
| P7 execute | M9 user Stop, then Continue | `ai-stopped` (worktree + chain preserved); Continue re-queues; next cycle resumes the SAME session | `TestFlowContinueAfterStopResumesChainHead` (flow_matrix); unit: `TestStopDuringPipelineParksAsStopped`, `TestPauseTransitionsToStoppedAndPreservesState` |
| P7 pipeline goroutine | panic | park with the panic cause, daemon and siblings unharmed, stop flag consumed | `TestHandleIssuePanicParksIssue`, `TestStopFlagConsumedWhenPipelinePanics` |

## Matrix — bug lane

| Phase | Interruption | Expected outcome | Verified by |
|-------|--------------|------------------|-------------|
| debug | M3 429 mid-session | park; re-entry resumes the limited session with `continue` | `TestFlowBugUsageLimitParksThenResumes` (flow) |
| debug | M2/M1 error | park; error propagates with checkpointed session (if id streamed) | `TestBugPipelinePropagatesError`, `TestBugPipelineRecordsSessionOnError` |
| debug | M5 low confidence | `ai-needs-info`; gate beats already-done claim | `TestBugPipelineLowConfidenceEscalates`, `TestBugPipelineLowConfidenceBeatsAlreadyDone` |
| debug | M4 no sentinel, no commits | `ai-needs-info` with the session's questions (not a "no commits" park) | `TestBugPipelineEscalatesToNeedsInfoWhenNoCommitsProduced`, `TestResumeBugPipelineEscalatesToNeedsInfoWhenStalled` |
| debug | M6 already done | issue closed | `TestBugPipelineReturnsAlreadyDone` |
| UAT | session fails / off-contract | swallowed, pipeline unaffected | `TestBugPipelineReturnsNilWhenUATFails`, `TestUATSkips*` |

## Matrix — ship & code review (both lanes)

| Phase | Interruption | Expected outcome | Verified by |
|-------|--------------|------------------|-------------|
| P8 ship | M4 zero commits | park "produced no commits" | `TestFlowFeatureNoCommitsParksAtShip` (flow_matrix) |
| P8 ship | M7 push fails | park; head = completed execute node → re-entry resumes it (`continue`), then ships; PR comment posted once | `TestFlowShipPushFailureParksThenResumesExecuteHead` (flow_matrix) |
| P8 ship | M7 PR create fails | park (same onInfra path) | `TestProcessOnceFailurePathParksForRework` |
| P8 ship | PR already announced by the spec stage | no duplicate PR comment | `TestShipSkipsPRCommentWhenAlreadyRecorded`, exercised in every flow re-entry test |
| P9 review | M2 round killed mid-run | park; head = codereview node → re-entry routes STRAIGHT to ship (no pipeline re-run) and resumes the cut-short round's session | `TestFlowCodeReviewKilledParksThenReentersShipOnly` (flow_matrix); unit: `TestHandleIssueRoutesCodeReviewStageToShip`, `TestCodeReviewResumesRecordedReviewSession` |
| P9 review | M8 restart mid-loop | round counter file resumes at the next round | `TestCodeReviewResumesFromRoundFile` |
| P9 review | M7 push/PR-lookup fails; comment fails | push/lookup park; comment failure survived | `TestCodeReviewPushFailureStopsLoop`, `TestCodeReviewPRLookupFailureSkipsLoop`, `TestCodeReviewSurvivesCommentFailure` |
| P10 done | M7 Done swap fails | surfaced error, stays `ai-wip` (no re-run just for a label swap) | `TestDoneSwapFailureIsSurfaced` |

## Matrix — orchestration invariants

| Concern | Expected | Verified by |
|---------|----------|-------------|
| Happy path, feature | one cycle to `ai-done`, 4-node lineage chain brainstorm→plan→execute→codereview | `TestFlowFeatureHappyPath` (flow) |
| Happy path, bug | one cycle to `ai-done` | `TestProcessOnceHappyPathBug` |
| Concurrency budget | slots respected across cycles, in-flight issues filtered from listing and sweep | `slots_flow_test.go`, `slots_sweep_test.go` |
| No auto-retry | parked issues never rescanned; requeue reuses the preserved worktree | `no_auto_retry_test.go` |
| Park handover | comment carries guidance + full error; cause cleared on ship | `TestParkComment*`, `TestParkWritesCauseAndShipClearsIt` |
