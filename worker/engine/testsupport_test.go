package engine

import (
	"context"

	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// testStateLabels mirrors the config defaults without importing them.
func testStateLabels() shared.StateLabels {
	return shared.StateLabels{WIP: "ai-wip", Done: "ai-done", Rework: "ai-rework", NeedsInfo: "ai-needs-info", Stopped: "ai-stopped"}
}

// testDeps wires the real infra adapters over r with near-instant retries, so
// engine tests exercise the same seams main wires in production.
func testDeps(cfg *shared.Config, r shared.Runner) Deps {
	return Deps{
		Cfg:       cfg,
		Host:      infra.NewGitHubWithRetry(r, cfg, testkit.TestRetry),
		Workspace: infra.NewWorktreeAt(r, cfg.RepoPath, testkit.TestRetry),
		NewAgent: func(logDir string) shared.Agent {
			return infra.NewClaude(r, logDir, cfg.ClaudeConfigDir)
		},
		DownloadImages: func(ctx context.Context, content, destDir string) string {
			return infra.DownloadIssueImages(ctx, r, content, destDir)
		},
	}
}

func newTestOrch(cfg *shared.Config, r shared.Runner) *Orchestrator {
	return NewOrchestrator(testDeps(cfg, r))
}

// rewire swaps the Orchestrator's runner-backed ports for ones over r, used by
// gatePipelines to interpose a blocking runner mid-test.
func rewire(o *Orchestrator, r shared.Runner) {
	d := testDeps(o.cfg, r)
	o.gh, o.wt, o.newAgent, o.images = d.Host, d.Workspace, d.NewAgent, d.DownloadImages
}
