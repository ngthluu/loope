package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type Orchestrator struct {
	cfg    *Config
	runner Runner
	gh     *GitHub
	wt     *Worktree

	// Auto-resume bookkeeping: per-issue backoff between resume attempts and
	// once-per-process skip logging. In-memory only — a restart retrying
	// immediately costs at most one extra attempt.
	//
	// mu also guards the slot ledger (active): ticketsPerCycle is a live
	// concurrency budget, not a batch size, so cycles start work and return
	// while earlier pipelines are still running. See slots.go.
	mu            sync.Mutex
	active        map[int]struct{} // issue numbers with a pipeline in flight
	inFlight      sync.WaitGroup   // one Add per acquired slot; drained on shutdown
	resumeBackoff map[int]backoffState
	skipLogged    map[int]bool
	now           func() time.Time // test seam; nil means time.Now
}

type backoffState struct {
	next  time.Time
	delay time.Duration
}

const (
	resumeBackoffMin = 5 * time.Minute
	resumeBackoffMax = 60 * time.Minute
)

// interruptedCause is the park cause SweepOrphans records for a run a daemon
// restart interrupted mid-pipeline. classifyCause treats it as resumable so the
// preserved worktree/session is auto-resumed (with backoff) rather than re-run.
const interruptedCause = "interrupted mid-run by a daemon restart"

func (o *Orchestrator) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

type pick struct {
	issue  Issue
	kind   string
	reason string
}

func (o *Orchestrator) issueLogDir(n int) string {
	return filepath.Join(o.cfg.WorkDir, "logs", fmt.Sprintf("issue-%d", n))
}

// ProcessOnce runs one poll cycle: top the in-flight pipeline set back up to the
// TicketsPerCycle budget from whatever is eligible right now. It selects
// sequentially (reusing single-pick Triage), launches each pick in its own
// goroutine — its own worktree/branch to its own PR — and RETURNS without
// waiting for them. Pipelines started in earlier cycles keep running alongside.
// Only listing/selection errors are returned; a pipeline logs its own outcome,
// because it now finishes long after the cycle that started it has returned.
func (o *Orchestrator) ProcessOnce(ctx context.Context) error {
	free := o.freeSlots()
	if free == 0 {
		return nil // budget full: don't even ask GitHub for the queue
	}
	issues, err := o.gh.ListEligibleIssues(ctx, o.cfg.EligibleLabel)
	if err != nil {
		return err
	}
	// A listing can still show an issue whose pipeline is running but whose
	// ai-wip label hasn't landed yet.
	issues = o.filterInactive(issues)
	if len(issues) == 0 {
		return nil
	}
	picks, selectErr := o.selectIssues(ctx, issues, free)
	if len(picks) == 0 {
		return selectErr
	}

	// Every pick runs a pipeline in its own worktree off the default branch.
	base, err := o.wt.DefaultBranch(ctx)
	if err != nil {
		return errors.Join(selectErr, err)
	}

	for i := range picks {
		if !o.tryAcquire(picks[i].issue.Number) {
			continue
		}
		go func(p pick) {
			// release is deferred FIRST so it runs LAST: a panicking pipeline
			// parks the issue in the recover handler below and still returns
			// its slot.
			defer o.release(p.issue.Number)
			// A panic in one pipeline must not kill the daemon or the sibling
			// pipelines: park the issue with the panic as its (non-resumable)
			// cause, preserving worktree and logs for a human.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("issue #%d: pipeline panic: %v\n%s", p.issue.Number, r, debug.Stack())
					_ = o.park(ctx, p.issue.Number, o.cfg.StateLabels.WIP, fmt.Errorf("panic: %v", r))
				}
			}()
			log.Printf("issue #%d (%s): %s", p.issue.Number, p.kind, p.reason)
			if err := o.handleIssue(ctx, p.issue, p.kind, base); err != nil {
				log.Printf("issue #%d: pipeline failed: %v", p.issue.Number, err)
			}
		}(picks[i])
	}
	return selectErr
}

// selectIssues picks up to limit distinct issues by calling the single-pick
// Triage repeatedly, removing each chosen issue from the candidate set. The
// limit is the caller's free-slot count, not the raw config value, so a cycle
// only asks for what it can actually start. A triage error stops selection and
// is returned alongside whatever was already picked, so the cycle can still act
// on earlier picks.
func (o *Orchestrator) selectIssues(ctx context.Context, issues []Issue, limit int) ([]pick, error) {
	n := limit
	if n < 1 {
		n = 1
	}
	triageClaude := &Claude{runner: o.runner, logDir: filepath.Join(o.cfg.WorkDir, "logs", "triage"), configDir: o.cfg.ClaudeConfigDir}
	remaining := issues
	var picks []pick
	for len(picks) < n && len(remaining) > 0 {
		dec, err := Triage(ctx, triageClaude, o.cfg.Models.Triage, o.cfg.RepoPath, remaining)
		if err != nil {
			return picks, err
		}
		var chosen Issue
		var rest []Issue
		for _, is := range remaining {
			if is.Number == dec.IssueNumber {
				chosen = is
			} else {
				rest = append(rest, is)
			}
		}
		picks = append(picks, pick{issue: chosen, kind: dec.Kind, reason: dec.Reason})
		remaining = rest
	}
	return picks, nil
}

func (o *Orchestrator) handleIssue(ctx context.Context, issue Issue, kind, base string) error {
	n := issue.Number
	branch := branchName(n)
	if err := o.gh.AddLabel(ctx, n, o.cfg.StateLabels.WIP); err != nil {
		return err
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.WIP)
	_ = o.gh.Comment(ctx, n, fmt.Sprintf("🤖 Picked up (%s flow). Branch: `%s`", kind, branch))

	wtPath, err := o.wt.Create(ctx, o.cfg.WorkDir, n, base)
	if err != nil {
		return o.abort(ctx, n, "", "", err)
	}
	content, err := o.gh.FetchIssueContent(ctx, n)
	if err != nil {
		return o.abort(ctx, n, wtPath, branch, err)
	}
	content = DownloadIssueImages(ctx, o.runner, content, o.issueLogDir(n))

	c := &Claude{runner: o.runner, logDir: o.issueLogDir(n), configDir: o.cfg.ClaudeConfigDir}
	var perr error
	if kind == "bug" {
		perr = RunBugPipeline(ctx, c, o.cfg, wtPath, content)
	} else {
		perr = RunFeaturePipeline(ctx, c, o.cfg, wtPath, content, readPersona(o.cfg.PersonaPath))
	}
	var done *alreadyDoneError
	if errors.As(perr, &done) {
		return o.finishDone(ctx, n, wtPath, branch, o.cfg.StateLabels.WIP, done.reason)
	}
	var lowConf *lowConfidenceError
	if errors.As(perr, &lowConf) {
		return o.finishNeedsInfo(ctx, n, wtPath, branch, o.cfg.StateLabels.WIP, lowConf)
	}
	if perr != nil {
		return o.park(ctx, n, o.cfg.StateLabels.WIP, perr)
	}
	return o.ship(ctx, issue, wtPath, branch, base, kind, o.cfg.StateLabels.WIP)
}

// finishDone closes an issue a pipeline judged already implemented. It runs on
// the handleIssue path, so ai-wip is already applied and a worktree exists:
// clean both up, comment the reason, swap WIP->Done, and close the issue. Uses a
// cancellation-proof context so a Ctrl-C still finishes cleanup and labeling.
// The Done label is swapped in before the close, so even if the close fails the
// issue is de-queued (hasStateLabel) and won't be re-picked.
func (o *Orchestrator) finishDone(ctx context.Context, n int, wtPath, branch, fromLabel, reason string) error {
	cctx := context.WithoutCancel(ctx)
	if wtPath != "" {
		_ = o.wt.Remove(cctx, wtPath)
	}
	if branch != "" {
		_ = o.wt.DeleteBranch(cctx, branch)
	}
	_ = o.gh.Comment(cctx, n, fmt.Sprintf("🤖 Already implemented — closing. %s", reason))
	if err := o.gh.SwapLabels(cctx, n, fromLabel, o.cfg.StateLabels.Done); err != nil {
		return fmt.Errorf("issue #%d: already implemented but marking done failed: %w", n, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Done)
	clearParkCause(o.issueLogDir(n))
	return o.gh.CloseIssue(cctx, n)
}

// finishNeedsInfo escalates an issue the brainstorm session judged too
// under-specified to implement. Modeled on finishDone: nothing was built, so
// remove the worktree and branch, comment the score and the architect's
// questions, swap fromLabel->NeedsInfo, and record state. It does NOT close the
// issue and records no park cause, so the auto-resume scan never touches it —
// the issue waits out of the queue until a human removes the needs-info label,
// which re-queues it. Returns nil: escalation is a clean terminal outcome, not a
// pipeline failure. Uses a cancellation-proof context so a Ctrl-C mid-pipeline
// still records the state.
func (o *Orchestrator) finishNeedsInfo(ctx context.Context, n int, wtPath, branch, fromLabel string, lc *lowConfidenceError) error {
	cctx := context.WithoutCancel(ctx)
	if wtPath != "" {
		_ = o.wt.Remove(cctx, wtPath)
	}
	if branch != "" {
		_ = o.wt.DeleteBranch(cctx, branch)
	}
	body := fmt.Sprintf("🤖 Not confident enough to implement (confidence %d/100). Please clarify and remove the `%s` label to re-queue:\n\n%s",
		lc.score, o.cfg.StateLabels.NeedsInfo, lc.feedback)
	_ = o.gh.Comment(cctx, n, body)
	if err := o.gh.SwapLabels(cctx, n, fromLabel, o.cfg.StateLabels.NeedsInfo); err != nil {
		return fmt.Errorf("issue #%d: low confidence but marking needs-info failed: %w", n, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.NeedsInfo)
	clearParkCause(o.issueLogDir(n))
	return nil
}

// classifyCause inspects a park cause and reports whether the daemon may
// auto-resume it (usage/rate limits, turn/budget ceilings, network outages),
// plus a one-line human explanation for the park comment. Non-resumable causes
// get no guidance — a genuine error the operator should investigate. It matches
// on the failure text produced by ClaudeResult.failureSummary and the runner.
// A panic is never resumable, full stop: it's checked first so a panic message
// that happens to embed a transient-looking substring (e.g. "i/o timeout")
// can't slip through to auto-resume.
func classifyCause(msg string) (guidance string, resumable bool) {
	m := strings.ToLower(strings.TrimSpace(msg))
	if strings.HasPrefix(m, "panic: ") {
		return "", false
	}
	switch {
	case strings.Contains(m, "session limit") || strings.Contains(m, "usage limit") ||
		strings.Contains(m, "rate limit") || strings.Contains(m, "api status 429"):
		return "Cause: Claude usage/rate limit — the loop auto-resumes it (with backoff) once the limit resets.", true
	case strings.Contains(m, "max_turns") || strings.Contains(m, "max turns") ||
		strings.Contains(m, "max-budget") || strings.Contains(m, "budget"):
		return "Cause: hit the turn/budget ceiling mid-run — the loop auto-resumes where it stopped (raise the execute maxTurns/maxBudgetUSD if this recurs).", true
	case strings.Contains(m, "interrupted mid-run"):
		return "Cause: the daemon restarted while this issue was mid-run — the loop auto-resumes the preserved session.", true
	}
	for _, sig := range transientSignatures {
		if strings.Contains(m, sig) {
			return "Cause: network outage — the loop auto-resumes when connectivity returns.", true
		}
	}
	return "", false
}

// failureGuidance returns the one-line explanation of a parked issue's cause,
// or "" when the cause is not a recognized transient one.
func failureGuidance(cause error) string {
	if cause == nil {
		return ""
	}
	g, _ := classifyCause(cause.Error())
	return g
}

// park moves an issue into the rework state and PRESERVES all progress so
// `loop -rework <N>` can resume it: comment guidance plus the error, then swap
// fromLabel->Rework (skipped when already in rework, to avoid a self-relabel).
// The comment itself is skipped on a repeated resumable re-park (already parked
// with guidance posted and nothing new for the operator to see), but a
// non-resumable cause always comments. The worktree, branch, logs, and session
// file are left untouched. Uses a cancellation-proof context so a Ctrl-C
// mid-pipeline still records the state.
func (o *Orchestrator) park(ctx context.Context, n int, fromLabel string, cause error) error {
	cctx := context.WithoutCancel(ctx)
	guidance, resumable := classifyCause(cause.Error())
	// A repeated resumable failure while already parked re-parks silently: the
	// guidance is on the issue from the first park, and the auto-resume scan
	// retrying on backoff would otherwise post a comment per attempt. A
	// non-resumable cause is new information for the operator, so it comments.
	if !(fromLabel == o.cfg.StateLabels.Rework && resumable) {
		comment := fmt.Sprintf("🤖 Parked for rework — run `loop -rework %d -config <cfg>`.", n)
		if guidance != "" {
			comment += "\n" + guidance
		}
		comment += fmt.Sprintf("\nError: %s", tail(cause.Error(), 800))
		_ = o.gh.Comment(cctx, n, comment)
	}
	if fromLabel != o.cfg.StateLabels.Rework {
		_ = o.gh.SwapLabels(cctx, n, fromLabel, o.cfg.StateLabels.Rework)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Rework)
	recordParkCause(o.issueLogDir(n), cause.Error())
	return cause
}

// ResumeParked scans ai-rework issues and re-runs Rework on the ones parked for
// a transient, resumable cause (usage/rate limit, turn/budget ceiling, network
// outage). Genuine errors have no resumable park cause and stay parked for a
// human. Each issue backs off exponentially between attempts (5m doubling to
// 60m) so a still-active usage limit isn't hammered every poll cycle. Resumes
// run sequentially: they're expensive Claude sessions.
func (o *Orchestrator) ResumeParked(ctx context.Context) error {
	issues, err := o.gh.ListIssuesWithLabel(ctx, o.cfg.StateLabels.Rework)
	if err != nil {
		return err
	}
	var errs []error
	for _, is := range issues {
		if ctx.Err() != nil {
			break
		}
		n := is.Number
		if !o.shouldResume(n) {
			continue
		}
		log.Printf("issue #%d: auto-resuming parked work", n)
		if err := o.Rework(ctx, n); err != nil {
			o.noteResumeFailure(n)
			errs = append(errs, fmt.Errorf("auto-resume #%d: %w", n, err))
			continue
		}
		o.clearResumeState(n)
	}
	return errors.Join(errs...)
}

// shouldResume reports whether issue n is auto-resumable right now: parked for
// a resumable cause, with its worktree and session intact, and past its backoff
// window. Missing prerequisites are logged once per process per issue, so a
// permanently human-owned park doesn't spam the daemon log every cycle.
func (o *Orchestrator) shouldResume(n int) bool {
	logDir := o.issueLogDir(n)
	reason := ""
	if cause := readParkCause(logDir); cause == "" {
		reason = "no recorded park cause; waiting for a human (`loop -rework`)"
	} else if _, resumable := classifyCause(cause); !resumable {
		reason = "cause needs a human; fix it and run `loop -rework`"
	} else if _, err := os.Stat(worktreePath(o.cfg.WorkDir, n)); err != nil {
		reason = "no preserved worktree (remove the rework label to re-queue)"
	} else if si, err := readSession(logDir); err != nil || si.SessionID == "" {
		reason = "no saved session (remove the rework label to re-queue)"
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if reason != "" {
		if o.skipLogged == nil {
			o.skipLogged = map[int]bool{}
		}
		if !o.skipLogged[n] {
			o.skipLogged[n] = true
			log.Printf("issue #%d: parked, not auto-resuming: %s", n, reason)
		}
		return false
	}
	if b, ok := o.resumeBackoff[n]; ok && o.clock().Before(b.next) {
		return false
	}
	return true
}

// noteResumeFailure starts or doubles issue n's backoff window.
func (o *Orchestrator) noteResumeFailure(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.resumeBackoff == nil {
		o.resumeBackoff = map[int]backoffState{}
	}
	b, ok := o.resumeBackoff[n]
	if !ok {
		b.delay = resumeBackoffMin
	} else {
		b.delay *= 2
		if b.delay > resumeBackoffMax {
			b.delay = resumeBackoffMax
		}
	}
	b.next = o.clock().Add(b.delay)
	o.resumeBackoff[n] = b
}

// clearResumeState forgets issue n's backoff and skip-log marks after a
// successful resume, so a future park starts fresh.
func (o *Orchestrator) clearResumeState(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.resumeBackoff, n)
	delete(o.skipLogged, n)
}

// SweepOrphans recovers issues stranded in ai-wip by a crashed previous run.
// It prefers to build on whatever the crash left behind: if the worktree
// survived and a Claude session was recorded, the run is resumable, so it parks
// the issue for rework (worktree, branch, logs, and session left intact) with a
// resumable cause the auto-resume scan continues — rather than re-running the
// whole pipeline from zero. Only when there is no resumable state left does it
// reclaim: force-remove the leftover worktree/branch (best-effort — they may
// already be gone) and strip the WIP label so the normal cycle re-queues the
// issue from scratch. Only safe while this process holds the workDir lock, which
// proves no live pipeline can own an ai-wip label. Returns an error (e.g.
// offline at boot) so runLoop can retry next cycle until one full sweep
// succeeds.
func (o *Orchestrator) SweepOrphans(ctx context.Context) error {
	issues, err := o.gh.ListIssuesWithLabel(ctx, o.cfg.StateLabels.WIP)
	if err != nil {
		return err
	}
	for _, is := range issues {
		n := is.Number
		logDir := o.issueLogDir(n)
		// Reuse before reclaim: a surviving worktree plus a recorded session is
		// exactly what rework resumes from, so park it (which relabels WIP->rework
		// and records the cause) and let the resume machinery continue the work.
		if _, statErr := os.Stat(worktreePath(o.cfg.WorkDir, n)); statErr == nil {
			if si, sErr := readSession(logDir); sErr == nil && si.SessionID != "" {
				log.Printf("issue #%d: stale %s from a crashed run — worktree and session intact, parking for resume", n, o.cfg.StateLabels.WIP)
				_ = o.park(ctx, n, o.cfg.StateLabels.WIP, errors.New(interruptedCause))
				continue
			}
		}
		log.Printf("issue #%d: stale %s from a crashed run — no resumable state, cleaning up and re-queueing", n, o.cfg.StateLabels.WIP)
		_ = o.wt.Remove(ctx, worktreePath(o.cfg.WorkDir, n))
		_ = o.wt.DeleteBranch(ctx, branchName(n))
		if err := o.gh.RemoveLabel(ctx, n, o.cfg.StateLabels.WIP); err != nil {
			return err
		}
		clearState(logDir)
		clearParkCause(logDir)
	}
	return nil
}

// ship pushes the branch, opens (or recovers) the PR, comments the URL, and
// swaps fromLabel->Done. Shared by the normal loop (fromLabel=WIP) and rework
// (fromLabel=Rework) so both finish identically. A deterministic tooling failure
// here (commit count, push, PR create) happens AFTER the pipeline has already
// produced commits, so both flows park for rework — preserving the worktree,
// branch, and session so the run resumes instead of re-running the whole
// pipeline from zero (which, for a non-transient failure, would loop every
// cycle and burn the full pipeline cost each time). A pipeline that produced no
// commits also parks. Returns nil only when fully shipped.
func (o *Orchestrator) ship(ctx context.Context, issue Issue, wtPath, branch, base, kind, fromLabel string) error {
	n := issue.Number
	onInfra := func(err error) error {
		return o.park(ctx, n, fromLabel, err)
	}
	count, err := o.wt.CommitCount(ctx, wtPath, base)
	if err != nil {
		return onInfra(err)
	}
	if count == 0 {
		return o.park(ctx, n, fromLabel, errors.New("pipeline finished but produced no commits"))
	}
	if err := o.wt.Push(ctx, wtPath, branch); err != nil {
		return onInfra(err)
	}
	url, err := o.gh.CreatePR(ctx, branch,
		fmt.Sprintf("%s (#%d)", issue.Title, n),
		fmt.Sprintf("Closes #%d\n\nAutomated by loope (%s flow). Spec and plan, if any, are committed in this branch under docs/.", n, kind))
	if err != nil {
		return onInfra(err)
	}
	_ = o.gh.Comment(ctx, n, "🤖 PR: "+url)
	recordPR(o.issueLogDir(n), url)
	if err := o.gh.SwapLabels(ctx, n, fromLabel, o.cfg.StateLabels.Done); err != nil {
		// PR is up but the Done swap failed. Surface it; leave fromLabel in place
		// so the issue isn't re-run just to retry a label swap (CreatePR is
		// idempotent). Clean up the worktree regardless.
		_ = o.wt.Remove(ctx, wtPath)
		return fmt.Errorf("issue #%d: PR created (%s) but marking done failed: %w", n, url, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Done)
	clearParkCause(o.issueLogDir(n))
	_ = o.wt.Remove(ctx, wtPath)
	return nil
}

// abort backs out after a deterministic tooling failure (git/gh: worktree,
// fetch, push, PR create, ...). These are infrastructure or transient problems,
// not the AI failing the task, so the issue is NOT marked ai-failed. Instead the
// WIP label is removed to leave the issue eligible for a fresh attempt next
// cycle, any worktree/branch is cleaned up, and the error is returned so the
// cycle logs it. No issue comment is posted, to avoid spamming the issue on a
// persistent infra failure that retries every poll.
func (o *Orchestrator) abort(ctx context.Context, n int, wtPath, branch string, cause error) error {
	cctx := context.WithoutCancel(ctx)
	log.Printf("issue #%d: tooling error, leaving eligible to retry next cycle: %v", n, cause)
	_ = o.gh.RemoveLabel(cctx, n, o.cfg.StateLabels.WIP)
	clearState(o.issueLogDir(n))
	clearParkCause(o.issueLogDir(n))
	if wtPath != "" {
		_ = o.wt.Remove(cctx, wtPath)
	}
	if branch != "" {
		_ = o.wt.DeleteBranch(cctx, branch)
	}
	return cause
}
