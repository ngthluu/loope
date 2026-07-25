package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
)

type Orchestrator struct {
	cfg    *Config
	runner Runner
	gh     *GitHub
	wt     *Worktree

	// mu guards the slot ledger (active): ticketsPerCycle is a live concurrency
	// budget, not a batch size, so cycles start work and return while earlier
	// pipelines are still running. See slots.go.
	mu       sync.Mutex
	active   map[int]struct{}           // issue numbers with a pipeline in flight
	inFlight sync.WaitGroup             // one Add per acquired slot; drained on shutdown
	cancels  map[int]context.CancelFunc // per-issue cancel for the in-flight ProcessOnce pipeline
	stopping map[int]bool               // issues whose current run was deliberately stopped
}

// errNotRunning is returned by Stop when no pipeline is in flight for the issue
// (never started, already finished, or a double Stop) — a no-op, surfaced to the
// dashboard as an inline message rather than an error.
var errNotRunning = errors.New("issue is not running")

// errAlreadyRunning is returned by Continue when the issue's pipeline is already
// in flight, so there is nothing to re-queue.
var errAlreadyRunning = errors.New("issue is already running")

// setCancel registers the in-flight pipeline's cancel func for issue n so Stop
// can cancel that one ticket's claude subprocess. Guarded by mu.
func (o *Orchestrator) setCancel(n int, cancel context.CancelFunc) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cancels == nil {
		o.cancels = map[int]context.CancelFunc{}
	}
	o.cancels[n] = cancel
}

// clearCancel forgets issue n's cancel func once its pipeline goroutine returns.
// Guarded by mu. The context's own resources are released by the goroutine's
// defer cancel(); this only removes the map entry Stop looks up.
func (o *Orchestrator) clearCancel(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.cancels, n)
}

// isStopping reports whether a Stop was requested for issue n's current run.
// Guarded by mu.
func (o *Orchestrator) isStopping(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stopping[n]
}

// consumeStopping reports whether a Stop was requested for issue n and clears the
// flag if so, so the pipeline goroutine transitions to ai-stopped exactly once.
// Guarded by mu.
func (o *Orchestrator) consumeStopping(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopping[n] {
		delete(o.stopping, n)
		return true
	}
	return false
}

// Stop cancels the in-flight pipeline for issue n mid-turn and flags the run so
// its goroutine parks the ticket as ai-stopped (via pause) as it unwinds. It
// returns immediately — the label transition is eventually consistent, surfacing
// on the dashboard's 3s poll a moment later. A ticket with no pipeline in flight
// (never started, already finished, double Stop) returns errNotRunning: a no-op.
func (o *Orchestrator) Stop(n int) error {
	o.mu.Lock()
	cancel, ok := o.cancels[n]
	if !ok {
		o.mu.Unlock()
		return errNotRunning
	}
	if o.stopping == nil {
		o.stopping = map[int]bool{}
	}
	o.stopping[n] = true
	o.mu.Unlock()
	cancel() // kills the claude subprocess via exec.CommandContext
	return nil
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
			n := p.issue.Number
			// release is deferred FIRST so it runs LAST: a panicking pipeline parks
			// the issue in the recover handler below and still returns its slot.
			defer o.release(n)
			// Derive a per-ticket child ctx and register its cancel so Stop can kill
			// this one pipeline's claude subprocess without touching its siblings.
			cctx, cancel := context.WithCancel(ctx)
			defer cancel() // release the context's resources when the goroutine ends
			o.setCancel(n, cancel)
			defer o.clearCancel(n)
			// One deferred handler owns BOTH panic recovery and the stop outcome, so
			// the stopping flag is consumed on every exit path. A panic in one
			// pipeline must not kill the daemon or the sibling pipelines: park the
			// issue with the panic as its (non-resumable) cause, preserving worktree
			// and logs for a human. Uses the LIVE parent ctx. Consuming the flag even
			// on the panic path is what stops it leaking: a flag left set would never
			// be cleared (clearCancel only clears cancels) and would spuriously stop
			// the issue's next run.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("issue #%d: pipeline panic: %v\n%s", n, r, debug.Stack())
					// The panic outcome wins over a pending stop — a human should look —
					// so consume the flag (preventing a leak) and park, don't pause.
					o.consumeStopping(n)
					_ = o.park(ctx, n, fmt.Errorf("panic: %v", r))
					return
				}
				// A Stop observed during the run transitions the ticket to ai-stopped
				// here, on the live parent ctx (the child ctx is cancelled). handleIssue
				// already skipped its normal outcome, leaving the ticket ai-wip for this.
				if o.consumeStopping(n) {
					o.pause(ctx, n)
				}
			}()
			log.Printf("issue #%d (%s): %s", n, p.kind, p.reason)
			if err := o.handleIssue(cctx, p.issue, p.kind, base); err != nil {
				log.Printf("issue #%d: pipeline failed: %v", n, err)
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
	triageClaude := &Claude{runner: o.runner, logDir: filepath.Join(o.cfg.WorkDir, "logs", "triage"), configDir: o.cfg.ClaudeConfigDir}
	remaining := issues
	var picks []pick
	for len(picks) < limit && len(remaining) > 0 {
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
	// Mirror the title next to the state marker: the dashboard otherwise knows
	// it only for as long as the issue keeps matching its label-scoped query.
	recordTitle(o.issueLogDir(n), issue.Title)
	_ = o.gh.Comment(ctx, n, pickupComment(kind, branch))

	wtPath, err := o.wt.Create(ctx, o.cfg.WorkDir, n, base)
	if err != nil {
		return o.abort(ctx, n, err)
	}
	content, err := o.gh.FetchIssueContent(ctx, n)
	if err != nil {
		return o.abort(ctx, n, err)
	}
	content = DownloadIssueImages(ctx, o.runner, content, o.issueLogDir(n))

	c := &Claude{runner: o.runner, logDir: o.issueLogDir(n), configDir: o.cfg.ClaudeConfigDir}
	uat := &UAT{Target: o.gh, Num: n}
	var perr error
	if kind == "bug" {
		perr = RunBugPipeline(ctx, c, o.cfg, wtPath, content, base, uat)
	} else {
		perr = RunFeaturePipeline(ctx, c, o.cfg, wtPath, content, readPersona(o.cfg.PersonaPath), uat)
	}
	// A Stop landed during the pipeline: skip the normal park/ship/finish outcome
	// and leave the ticket ai-wip. The launching goroutine's consumeStopping+pause
	// transitions it to ai-stopped on the live parent ctx.
	if o.isStopping(n) {
		return nil
	}
	var done *alreadyDoneError
	if errors.As(perr, &done) {
		return o.finishDone(ctx, n, wtPath, branch, done.reason)
	}
	var lowConf *lowConfidenceError
	if errors.As(perr, &lowConf) {
		return o.finishNeedsInfo(ctx, n, wtPath, branch, lowConf)
	}
	if perr != nil {
		return o.park(ctx, n, perr)
	}
	return o.ship(ctx, issue, wtPath, branch, base, kind)
}

// finishDone closes an issue a pipeline judged already implemented. It runs on
// the handleIssue path, so ai-wip is already applied and a worktree exists:
// clean both up, comment the reason, swap WIP->Done, and close the issue. Uses a
// cancellation-proof context so a Ctrl-C still finishes cleanup and labeling.
// The Done label is swapped in before the close, so even if the close fails the
// issue is de-queued (hasStateLabel) and won't be re-picked.
func (o *Orchestrator) finishDone(ctx context.Context, n int, wtPath, branch, reason string) error {
	cctx := context.WithoutCancel(ctx)
	if wtPath != "" {
		_ = o.wt.Remove(cctx, wtPath)
	}
	if branch != "" {
		_ = o.wt.DeleteBranch(cctx, branch)
	}
	_ = o.gh.Comment(cctx, n, alreadyDoneComment(reason))
	if err := o.gh.SwapLabels(cctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Done); err != nil {
		return fmt.Errorf("issue #%d: already implemented but marking done failed: %w", n, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Done)
	clearParkCause(o.issueLogDir(n))
	return o.gh.CloseIssue(cctx, n)
}

// finishNeedsInfo escalates an issue the brainstorm session judged too
// under-specified to implement. Modeled on finishDone: nothing was built, so
// remove the worktree and branch, comment the score and the architect's
// questions, swap WIP->NeedsInfo, and record state. It does NOT close the
// issue: it waits out of the queue until a human removes the needs-info label,
// which re-queues it. Returns nil: escalation is a clean terminal outcome, not a
// pipeline failure. Uses a cancellation-proof context so a Ctrl-C mid-pipeline
// still records the state.
func (o *Orchestrator) finishNeedsInfo(ctx context.Context, n int, wtPath, branch string, lc *lowConfidenceError) error {
	cctx := context.WithoutCancel(ctx)
	if wtPath != "" {
		_ = o.wt.Remove(cctx, wtPath)
	}
	if branch != "" {
		_ = o.wt.DeleteBranch(cctx, branch)
	}
	_ = o.gh.Comment(cctx, n, needsInfoComment(lc.score, o.cfg.StateLabels.NeedsInfo, lc.feedback))
	if err := o.gh.SwapLabels(cctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.NeedsInfo); err != nil {
		return fmt.Errorf("issue #%d: low confidence but marking needs-info failed: %w", n, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.NeedsInfo)
	clearParkCause(o.issueLogDir(n))
	return nil
}

// classifyCause inspects a park cause and reports a one-line human explanation
// for the park comment, or "" when the cause is not a recognized one.
//
// Nothing here decides whether to retry, because NOTHING is retried
// automatically. ai-rework is a terminal state: a parked issue waits for a
// human, who removes the rework label to queue another attempt (which reuses
// the preserved worktree and branch). Retrying a usage-limit or network park
// every cycle meant one broken issue re-ran the whole pipeline indefinitely,
// burning tokens on work nobody was watching. Recognizing the cause still
// matters — it is what the park comment explains.
//
// A panic is checked first and gets no guidance, so a panic message embedding a
// transient-looking substring can't be mislabelled as a network blip.
func classifyCause(msg string) (guidance string) {
	m := strings.ToLower(strings.TrimSpace(msg))
	if strings.HasPrefix(m, "panic: ") {
		return ""
	}
	switch {
	case strings.Contains(m, "session limit") || strings.Contains(m, "usage limit") ||
		strings.Contains(m, "rate limit") || strings.Contains(m, "api status 429"):
		return mustRender("guidance-usage-limit", promptData())
	case strings.Contains(m, "max_turns") || strings.Contains(m, "max turns") ||
		strings.Contains(m, "max-budget") || strings.Contains(m, "budget"):
		return mustRender("guidance-budget", promptData())
	}
	for _, sig := range transientSignatures {
		if strings.Contains(m, sig) {
			return mustRender("guidance-network", promptData())
		}
	}
	return ""
}

// park moves an issue into the rework state and PRESERVES all progress: comment
// the guidance plus the full error, then swap WIP->Rework. The worktree, branch,
// logs, and session file are left untouched. Uses a cancellation-proof context
// so a Ctrl-C mid-pipeline still records the state.
//
// Parking is TERMINAL: only a human removing the rework label moves the issue
// on, and the next run then reuses the preserved worktree. So every park
// comments — the error text IS the handover to the human.
func (o *Orchestrator) park(ctx context.Context, n int, cause error) error {
	cctx := context.WithoutCancel(ctx)
	_ = o.gh.Comment(cctx, n, parkComment(o.cfg.StateLabels.Rework, classifyCause(cause.Error()), clip(cause.Error(), 6000)))
	_ = o.gh.SwapLabels(cctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Rework)
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Rework)
	recordParkCause(o.issueLogDir(n), cause.Error())
	return cause
}

// pause is the terminal outcome for a user-stopped run: swap ai-wip->ai-stopped,
// record the state, and comment. It runs on the LIVE parent ctx (the pipeline's
// child ctx is already cancelled, so its GitHub calls would fail). It
// deliberately does NOT touch the worktree, branch, logs, or session file, and
// records NO park cause. ai-stopped is not ai-wip, so the orphan sweep — the
// daemon's only automatic state mover — can never see the ticket: it stays put,
// even across a restart, until the user hits Continue. If the swap fails, it records nothing and
// does not comment: local state must never claim ai-stopped while the real label
// still reads ai-wip.
func (o *Orchestrator) pause(ctx context.Context, n int) {
	logDir := o.issueLogDir(n)
	// The swap is the source of truth. It can fail because GitHub is unreachable,
	// or because the ticket already left ai-wip (a ship/park won the stop's narrow
	// race window). Either way, bail before recording state or commenting: writing
	// ai-stopped locally would diverge from the real label — the dashboard would
	// show stopped while GitHub still reads ai-wip, and SweepOrphans could act on
	// the "stopped" ticket — and the notice would falsely annotate a ticket that
	// never actually stopped.
	if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Stopped); err != nil {
		log.Printf("issue #%d: stop swap %s->%s failed, leaving state unchanged: %v", n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Stopped, err)
		return
	}
	recordState(logDir, o.cfg.StateLabels.Stopped)
	_ = o.gh.Comment(ctx, n, stoppedComment())
}

// stoppedComment is the fixed notice posted when a run is stopped by the user.
func stoppedComment() string {
	return "⏸ Stopped by user. Worktree, logs and session are preserved. Press Continue to re-queue it; the run continues in the same worktree.\n\n" + botMarker
}

// Continue re-queues a stopped issue: it only rewrites labels/state on disk —
// remove ai-stopped, clear the state marker and any park cause — so the issue is
// eligible again and the next runLoop cycle picks it up when a slot is free
// (never synchronously, never bypassing the concurrency budget). The run starts
// a fresh pipeline, but in the PRESERVED worktree: Worktree.Create reuses
// whatever is on the path, per the project's continue-not-reset rule, so the
// commits the stopped run produced are built on rather than discarded.
//
// Being label-driven, it survives a daemon restart. Returns errAlreadyRunning
// if the issue's pipeline is somehow already in flight.
func (o *Orchestrator) Continue(ctx context.Context, n int) error {
	o.mu.Lock()
	_, running := o.active[n]
	o.mu.Unlock()
	if running {
		return errAlreadyRunning
	}
	if err := o.gh.RemoveLabel(ctx, n, o.cfg.StateLabels.Stopped); err != nil {
		return err
	}
	logDir := o.issueLogDir(n)
	clearState(logDir)
	clearParkCause(logDir)
	return nil
}

// SweepOrphans recovers issues stranded in ai-wip by a crashed previous run:
// it strips the WIP label and clears the local state so the normal cycle
// re-queues the issue, and it PRESERVES everything the crash left on disk.
//
// The worktree and branch are deliberately not touched. Worktree.Create reuses
// whatever is on the path (and reclaims a bare leftover branch itself), so the
// re-queued run continues from the crashed run's commits instead of starting
// from zero. That makes the sweep a single uniform outcome — ai-wip -> eligible
// — with no session-resume special case: the daemon never moves an issue INTO
// ai-rework except on a genuine failure, and never out of it at all.
//
// Only safe while this process holds the workDir lock, which proves no OTHER
// process can own an ai-wip label. THIS process can: a sweep that failed at boot
// is retried on later cycles, by which time its own pipelines are running and
// wearing WIP, so the ledger's in-flight set is filtered out first — stripping a
// live pipeline's label would let a second run start for the same issue.
// Returns an error (e.g. offline at boot) so runLoop can retry next cycle until
// one full sweep succeeds.
func (o *Orchestrator) SweepOrphans(ctx context.Context) error {
	issues, err := o.gh.ListIssuesWithLabel(ctx, o.cfg.StateLabels.WIP)
	if err != nil {
		return err
	}
	for _, is := range o.filterInactive(issues) {
		n := is.Number
		log.Printf("issue #%d: stale %s from a crashed run — re-queueing, worktree preserved", n, o.cfg.StateLabels.WIP)
		if err := o.gh.RemoveLabel(ctx, n, o.cfg.StateLabels.WIP); err != nil {
			return err
		}
		logDir := o.issueLogDir(n)
		clearState(logDir)
		clearParkCause(logDir)
	}
	return nil
}

// ship pushes the branch, opens (or recovers) the PR, comments the URL, and
// swaps WIP->Done. A deterministic tooling failure here (commit count, push, PR
// create) happens AFTER the pipeline has already produced commits, so it parks
// for rework — preserving the worktree, branch, and session, so a human who
// removes the label gets a run that builds on those commits instead of
// re-running the whole pipeline from zero. A pipeline that produced no commits
// also parks. Returns nil only when fully shipped.
func (o *Orchestrator) ship(ctx context.Context, issue Issue, wtPath, branch, base, kind string) error {
	n := issue.Number
	onInfra := func(err error) error {
		return o.park(ctx, n, err)
	}
	count, err := o.wt.CommitCount(ctx, wtPath, base)
	if err != nil {
		return onInfra(err)
	}
	if count == 0 {
		return o.park(ctx, n, errors.New("pipeline finished but produced no commits"))
	}
	if err := o.wt.Push(ctx, wtPath, branch); err != nil {
		return onInfra(err)
	}
	url, err := o.gh.CreatePR(ctx, branch, prTitle(issue.Title, n), prBody(n, kind))
	if err != nil {
		return onInfra(err)
	}
	_ = o.gh.Comment(ctx, n, prComment(url))
	recordPR(o.issueLogDir(n), url)
	if err := o.gh.SwapLabels(ctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Done); err != nil {
		// PR is up but the Done swap failed. Surface it; leave ai-wip in place so
		// the issue isn't re-run just to retry a label swap (CreatePR is
		// idempotent). Clean up the worktree regardless.
		_ = o.wt.Remove(ctx, wtPath)
		return fmt.Errorf("issue #%d: PR created (%s) but marking done failed: %w", n, url, err)
	}
	recordState(o.issueLogDir(n), o.cfg.StateLabels.Done)
	clearParkCause(o.issueLogDir(n))
	_ = o.wt.Remove(ctx, wtPath)
	return nil
}

// abort backs out after a deterministic tooling failure (git/gh: worktree add,
// fetch, ...) before the pipeline ever started. Every gh/git call is already
// retried in-band (see githubRetry), so reaching here means the failure is
// persistent, not a blip.
//
// It used to strip ai-wip and leave the issue eligible, which re-triaged and
// re-attempted the issue every single cycle — the unattended retry loop this
// flow exists to avoid, paid for in triage tokens. So it parks like any other
// failure: labelled ai-rework with the full error commented, waiting for a human
// to remove the label. Whatever the failing step managed to create (worktree,
// branch) is preserved, not deleted, so the next attempt builds on it.
func (o *Orchestrator) abort(ctx context.Context, n int, cause error) error {
	log.Printf("issue #%d: tooling error, parking for a human: %v", n, cause)
	return o.park(ctx, n, cause)
}

// botMarker tags the daemon's own status chatter — pickup, park, PR link,
// already-done, stopped. Like uatMarker it is an HTML comment, so it is
// invisible on GitHub while staying greppable in the raw body, and it lets
// FetchIssueContent strip that chatter back out instead of feeding the model a
// transcript of its own past runs (see isBotStatusComment).
//
// The needs-info comment is deliberately NOT tagged: it carries the numbered
// questions a human answers in the next comment, so removing it would orphan
// the answer.
const botMarker = "<!-- loope:bot -->"

// legacyBotStatusPrefixes recognise status comments posted before botMarker
// existed. They are the exact opening text of each tagged template, so nothing
// a human writes is mistaken for chatter, and needs-info ("🤖 Not confident
// enough…") is left alone here too.
var legacyBotStatusPrefixes = []string{
	"🤖 Picked up (",
	"🤖 Parked as `",
	"🤖 PR: ",
	"🤖 Already implemented — closing.",
	"⏸ Stopped by user.",
}

// isBotStatusComment reports whether a comment is the daemon's own status
// chatter and so should be kept out of the issue content handed to Claude.
func isBotStatusComment(body string) bool {
	if strings.Contains(body, botMarker) {
		return true
	}
	trimmed := strings.TrimSpace(body)
	for _, p := range legacyBotStatusPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

func pickupComment(kind, branch string) string {
	d := promptData()
	d["Kind"] = kind
	d["Branch"] = branch
	return mustRender("pickup", d)
}

func alreadyDoneComment(reason string) string {
	d := promptData()
	d["Reason"] = reason
	return mustRender("already-done", d)
}

func needsInfoComment(score int, label, feedback string) string {
	d := promptData()
	d["Score"] = score
	d["Label"] = label
	d["Feedback"] = feedback
	return mustRender("needs-info", d)
}

func parkComment(label, guidance, errText string) string {
	d := promptData()
	d["Label"] = label
	d["Guidance"] = guidance
	d["Error"] = errText
	return mustRender("park", d)
}

func prComment(url string) string {
	d := promptData()
	d["URL"] = url
	return mustRender("pr-comment", d)
}

func prTitle(title string, n int) string {
	d := promptData()
	d["Title"] = title
	d["Number"] = n
	return mustRender("pr-title", d)
}

func prBody(n int, kind string) string {
	d := promptData()
	d["Number"] = n
	d["Kind"] = kind
	return mustRender("pr-body", d)
}
