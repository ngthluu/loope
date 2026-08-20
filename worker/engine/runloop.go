package engine

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"time"

	"github.com/ngthluu/loope/worker/shared"
)

// RunLoop drives the poll cycle forever: one startup orphan sweep (retried
// until it succeeds once), then top the in-flight pipeline set up from the
// eligible queue, waiting one interval between cycles. There is no resume stage:
// ai-rework is terminal, so the only way work continues is a human removing the
// label, which puts the issue back in the eligible queue this cycle already
// reads. Cycles no longer block on the pipelines they start, so both
// exit paths drain in-flight work with o.Wait() before returning — main's
// deferred workDir-lock release must not run while a pipeline is live. Every
// stage runs under guard, so a panic is one bad cycle, not a dead daemon.
// Returns only when the context is cancelled, draining in-flight pipelines
// via o.Wait() on that path before returning.
func RunLoop(ctx context.Context, o *Orchestrator, cfg *shared.Config, sweep bool) {
	log.Printf("watching %s for label %q every %ds", cfg.RepoSlug, cfg.EligibleLabel, cfg.PollIntervalSec)
	for {
		if sweep {
			if err := guard("orphan sweep", func() error { return o.SweepOrphans(ctx) }); err != nil {
				log.Printf("orphan sweep failed (will retry next cycle): %v", err)
			} else {
				sweep = false
			}
		}
		// Merge-resolve scans before the pipeline cycle so a merge request is
		// never starved by a full eligible queue; both draw on the same slot
		// budget, so the ordering is the priority.
		if err := guard("merge-resolve scan", func() error { return o.ProcessMergeResolves(ctx) }); err != nil {
			log.Printf("merge-resolve scan error: %v", err)
		}
		if err := guard("cycle", func() error { return o.ProcessOnce(ctx) }); err != nil {
			log.Printf("cycle error: %v", err)
		}
		select {
		case <-ctx.Done():
			log.Println("shutting down: draining in-flight pipelines (signal again to force quit)")
			// Pipelines see the cancelled context and unwind through their
			// existing context.WithoutCancel cleanup paths, exactly as they did
			// when a Ctrl-C landed during the old in-cycle wg.Wait().
			o.Wait()
			return
		case <-time.After(time.Duration(cfg.PollIntervalSec) * time.Second):
		}
	}
}

// guard runs fn, converting a panic into an error so a bug in one stage can
// never kill the daemon. The stack is logged at recovery time.
func guard(what string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s panic: %v\n%s", what, r, debug.Stack())
			err = fmt.Errorf("%s panic: %v", what, r)
		}
	}()
	return fn()
}
