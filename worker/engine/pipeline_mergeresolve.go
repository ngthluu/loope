package engine

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/ngthluu/loope/worker/shared"
)

// The merge-resolve flow: a human adds cfg.MergeResolveLabel to an issue whose
// worktree already exists (shipped, parked, waiting on info — any state), and
// the daemon merges origin/<default-branch> into the issue's branch, resolves
// conflicts with ONE Claude session if the merge stops on them, and pushes.
// It is a maintenance pass over existing work, not a pipeline: it never
// classifies, never resumes or records the issue's pipeline session, and restores
// whatever state label the issue had when it is done.

const (
	// mergeResolveSentinel prefixes the one status line the resolve session
	// prints last: "MERGE_RESOLVE_STATUS: resolved" or "MERGE_RESOLVE_STATUS:
	// blocked <reason>". It only improves the outbound comment — pass/fail is
	// decided by git state (MergeInProgress/HasUnmergedPaths), never by the
	// session's self-report, matching afterFix's distrust-and-verify stance.
	mergeResolveSentinel = "MERGE_RESOLVE_STATUS:"
	// mergeResolvePriorFile records the state label the issue carried when the
	// merge-resolve run picked it up, so the label survives a daemon crash:
	// SweepOrphans strips the orphaned ai-wip but has no idea this run swapped
	// out e.g. ai-done, and without the marker the re-picked run would restore
	// nothing. Cleared on every terminal outcome.
	mergeResolvePriorFile = "mergeresolve-prior"
)

// RunMergeResolve fetches origin, merges origin/base into wtPath's branch, and
// — only when that leaves conflicts — drives exactly one Claude session to
// resolve and commit them. A merge already in progress (a parked prior attempt
// left its conflicts half-resolved) is continued, never aborted or restarted,
// per the project's continue-from-existing-state rule. It deliberately never
// calls c.RecordSession/RecordSnapshot: this session must not clobber the
// issue's persisted pipeline session, which a later pipeline re-entry resumes.
// summary is the session's status line for the outbound comment, "" when no
// session ran (clean merge) or none was printed.
func RunMergeResolve(ctx context.Context, c shared.Agent, cfg *shared.Config, wt shared.Workspace, wtPath, base string) (summary string, err error) {
	if err := wt.Fetch(ctx); err != nil {
		return "", fmt.Errorf("fetch origin: %w", err)
	}
	if !wt.MergeInProgress(ctx, wtPath) {
		if err := wt.Merge(ctx, wtPath, "origin/"+base); err != nil {
			unmerged, uerr := wt.HasUnmergedPaths(ctx, wtPath)
			if uerr != nil || !unmerged {
				// The merge failed without leaving conflict entries: a genuine
				// git failure, not the conflict stop the session exists for.
				return "", fmt.Errorf("git merge origin/%s: %w", base, err)
			}
			// Conflicts — fall through to the resolve session.
		} else {
			return "", nil // clean merge (or already up to date): no session needed
		}
	}
	res, cerr := c.Call(ctx, shared.ClaudeCall{
		Dir: wtPath, Label: "mergeresolve", Prompt: mergeResolvePrompt(base),
		Model:           cfg.Models.MergeResolveConfig(),
		SkipPermissions: true,
		DisallowedTools: []string{"AskUserQuestion"},
	})
	if cerr != nil {
		return "", cerr
	}
	summary, _ = parseSentinelLine(res.Result, mergeResolveSentinel)
	// Git state, not the sentinel, decides the outcome. Both checks are needed:
	// unmerged paths alone misses a session that resolved every file but never
	// committed, and MERGE_HEAD alone misses one that committed with `git
	// commit -i` tricks leaving conflict entries staged.
	if wt.MergeInProgress(ctx, wtPath) {
		return summary, fmt.Errorf("merge-resolve session did not conclude the merge (no merge commit):\n%s", shared.Clip(res.Result, 4000))
	}
	if unmerged, err := wt.HasUnmergedPaths(ctx, wtPath); err != nil {
		return summary, fmt.Errorf("post-resolution conflict check: %w", err)
	} else if unmerged {
		return summary, fmt.Errorf("merge-resolve session ended with unresolved conflicts:\n%s", shared.Clip(res.Result, 4000))
	}
	return summary, nil
}

// ProcessMergeResolves is the per-cycle scan of the merge-resolve flow, run by
// runLoop just before ProcessOnce so a merge request is never starved by a full
// eligible queue. It shares ProcessOnce's slot ledger — a merge run and a
// pipeline run for the same issue can never coexist, and both compete for the
// same TicketsPerCycle budget — and its per-issue goroutine ceremony
// (runGuarded), so Stop and panic recovery work identically.
func (o *Orchestrator) ProcessMergeResolves(ctx context.Context) error {
	if o.cfg.MergeResolveLabel == "" {
		return nil
	}
	if o.freeSlots() == 0 {
		return nil // budget full: don't even ask GitHub for the queue
	}
	issues, err := o.gh.ListIssuesWithLabel(ctx, o.cfg.MergeResolveLabel)
	if err != nil {
		return err
	}
	issues = o.filterInactive(issues)
	if len(issues) == 0 {
		return nil
	}
	base, err := o.wt.DefaultBranch(ctx)
	if err != nil {
		return err
	}
	for _, is := range issues {
		issue, n := is, is.Number
		if !o.tryAcquire(n) {
			continue
		}
		log.Printf("issue #%d: merge-resolve requested", n)
		o.runGuarded(ctx, n,
			func(cctx context.Context) error { return o.handleMergeResolve(cctx, issue, base) },
			func(cause error) { _ = o.parkMergeResolve(ctx, n, cause) })
	}
	return nil
}

// handleMergeResolve is the per-issue driver of the merge-resolve flow,
// handleIssue's much simpler sibling: snapshot the state label to restore,
// wear ai-wip while running, run the fetch/merge/resolve/push sequence, then
// restore the prior label — or park as ai-rework on any failure.
func (o *Orchestrator) handleMergeResolve(ctx context.Context, issue shared.Issue, base string) error {
	n := issue.Number
	logDir := o.issueLogDir(n)
	prior := currentStateLabel(issue.Labels, o.cfg.StateLabels)
	if prior == o.cfg.StateLabels.WIP {
		// A stale ai-wip from a crashed run the startup sweep hasn't cleared
		// yet. Swapping it for our own would fight the sweep; wait it out.
		log.Printf("issue #%d: merge-resolve requested but issue is %s; skipping this cycle", n, prior)
		return nil
	}
	if prior == "" {
		// GitHub shows no state label: either a queued issue, or a crashed
		// merge-resolve run whose ai-wip the sweep stripped — the marker knows.
		prior = readMergeResolvePrior(logDir)
	}
	recordMergeResolvePrior(logDir, prior)
	if prior == "" {
		if err := o.gh.AddLabel(ctx, n, o.cfg.StateLabels.WIP); err != nil {
			return err
		}
	} else if err := o.gh.SwapLabels(ctx, n, prior, o.cfg.StateLabels.WIP); err != nil {
		return err
	}
	shared.RecordState(logDir, o.cfg.StateLabels.WIP)
	shared.RecordTitle(logDir, issue.Title)

	branch := shared.BranchName(n)
	wtPath := shared.WorktreePath(o.cfg.WorkDir, n)
	if _, err := os.Stat(wtPath); err != nil {
		return o.parkMergeResolve(ctx, n, fmt.Errorf("no worktree at %s — nothing to merge into (the issue was never picked up, or its worktree was removed)", wtPath))
	}
	_ = o.gh.Comment(ctx, n, mergeResolvePickupComment(base, branch))

	c := o.newAgent(logDir)
	summary, err := RunMergeResolve(ctx, c, o.cfg, o.wt, wtPath, base)
	// A Stop landed during the run: skip the normal outcome and leave the
	// ticket ai-wip; runGuarded's consumeStopping+pause transitions it to
	// ai-stopped on the live parent ctx. The trigger label stays on, so
	// Continue re-queues the merge, which picks up exactly where it stopped.
	if o.isStopping(n) {
		return nil
	}
	if err != nil {
		return o.parkMergeResolve(ctx, n, err)
	}
	if err := o.wt.Push(ctx, wtPath, branch); err != nil {
		return o.parkMergeResolve(ctx, n, err)
	}
	return o.finishMergeResolve(ctx, n, prior, base, branch, summary)
}

// finishMergeResolve is the success outcome: strip the trigger label, swap
// ai-wip back to the label the issue wore before the run (or bare-remove
// ai-wip when it wore none), and comment. The trigger label goes first: if a
// later step fails the issue is left ai-wip for a human, but the flow can no
// longer re-fire every cycle. Uses a cancellation-proof context, like every
// other terminal outcome.
func (o *Orchestrator) finishMergeResolve(ctx context.Context, n int, prior, base, branch, summary string) error {
	cctx := context.WithoutCancel(ctx)
	logDir := o.issueLogDir(n)
	if err := o.gh.RemoveLabel(cctx, n, o.cfg.MergeResolveLabel); err != nil {
		return err
	}
	if prior == "" {
		if err := o.gh.RemoveLabel(cctx, n, o.cfg.StateLabels.WIP); err != nil {
			return err
		}
		shared.ClearState(logDir)
	} else {
		if err := o.gh.SwapLabels(cctx, n, o.cfg.StateLabels.WIP, prior); err != nil {
			return err
		}
		shared.RecordState(logDir, prior)
	}
	clearMergeResolvePrior(logDir)
	_ = o.gh.Comment(cctx, n, mergeResolveDoneComment(base, branch, summary))
	return nil
}

// parkMergeResolve is the failure outcome, park's merge-resolve sibling: same
// ai-rework label, state record, and park cause, plus one extra move — it
// strips the trigger label, so a persistently failing merge is attempted once,
// not every poll cycle (the daemon's nothing-auto-retries invariant; normal
// parks get this for free because ai-rework de-queues the issue, but the
// merge-resolve scan doesn't filter on state labels). The park comment tells
// the human to re-add the trigger label to retry — NOT to remove ai-rework,
// which would instead route the issue into a fresh, unrelated pipeline.
func (o *Orchestrator) parkMergeResolve(ctx context.Context, n int, cause error) error {
	cctx := context.WithoutCancel(ctx)
	logDir := o.issueLogDir(n)
	_ = o.gh.RemoveLabel(cctx, n, o.cfg.MergeResolveLabel)
	_ = o.gh.Comment(cctx, n, mergeResolveParkComment(o.cfg.StateLabels.Rework, o.cfg.MergeResolveLabel, classifyCause(cause.Error()), shared.Clip(cause.Error(), 6000)))
	_ = o.gh.SwapLabels(cctx, n, o.cfg.StateLabels.WIP, o.cfg.StateLabels.Rework)
	shared.RecordState(logDir, o.cfg.StateLabels.Rework)
	shared.RecordParkCause(logDir, cause.Error())
	clearMergeResolvePrior(logDir)
	return cause
}

// currentStateLabel returns whichever mutually-exclusive lifecycle label the
// issue currently carries, or "". WIP first so handleMergeResolve's stale-wip
// guard sees it before anything else.
func currentStateLabel(labels []shared.Label, sl shared.StateLabels) string {
	for _, name := range []string{sl.WIP, sl.Rework, sl.NeedsInfo, sl.Stopped, sl.Done} {
		if shared.HasLabel(labels, name) {
			return name
		}
	}
	return ""
}

// recordMergeResolvePrior writes the state label to restore after the run to
// <logDir>/mergeresolve-prior. Best-effort, like the other log-writers.
func recordMergeResolvePrior(logDir, label string) {
	if logDir == "" || label == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(logDir, mergeResolvePriorFile), []byte(label), 0o644)
}

// readMergeResolvePrior reads the marker written by recordMergeResolvePrior,
// or "" if none is recorded.
func readMergeResolvePrior(logDir string) string {
	b, err := os.ReadFile(filepath.Join(logDir, mergeResolvePriorFile))
	if err != nil {
		return ""
	}
	return string(b)
}

// clearMergeResolvePrior removes the marker on every terminal outcome.
func clearMergeResolvePrior(logDir string) {
	if logDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(logDir, mergeResolvePriorFile))
}

func mergeResolvePrompt(base string) string {
	d := promptData()
	d["Base"] = base
	return mustRender("mergeresolve.md.tmpl", d)
}

func mergeResolvePickupComment(base, branch string) string {
	d := promptData()
	d["Base"] = base
	d["Branch"] = branch
	return mustRender("mergeresolve-pickup", d)
}

func mergeResolveDoneComment(base, branch, summary string) string {
	d := promptData()
	d["Base"] = base
	d["Branch"] = branch
	d["Summary"] = summary
	return mustRender("mergeresolve-done", d)
}

func mergeResolveParkComment(label, triggerLabel, guidance, errText string) string {
	d := promptData()
	d["Label"] = label
	d["TriggerLabel"] = triggerLabel
	d["Guidance"] = guidance
	d["Error"] = errText
	return mustRender("mergeresolve-park", d)
}
