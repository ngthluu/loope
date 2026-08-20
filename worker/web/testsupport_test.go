package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ngthluu/loope/worker/engine"
	"github.com/ngthluu/loope/worker/infra"
	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// fakeEnv simulates gh/git/claude for the dashboard's orchestrator-backed
// routes — a slim copy of the engine test env, kept local so web tests stay
// co-located with the package they exercise.
type fakeEnv struct {
	f     *testkit.FakeRunner
	wtDir string
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	env := &fakeEnv{f: &testkit.FakeRunner{}, wtDir: t.TempDir()}
	env.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		switch c.Name {
		case "gh":
			switch {
			case strings.HasPrefix(joined, "issue list"):
				return `[{"number": 7, "title": "Fix crash", "body": "boom", "labels": [{"name": "ai-agent"}]}]`, "", nil
			case strings.HasPrefix(joined, "issue view"):
				return `{"title": "Fix crash", "body": "boom", "comments": []}`, "", nil
			case strings.HasPrefix(joined, "pr create"):
				return "https://github.com/org/repo/pull/99\n", "", nil
			case strings.HasPrefix(joined, "pr view"):
				return `{"number": 99, "url": "https://github.com/org/repo/pull/99"}`, "", nil
			}
			return "", "", nil
		case "git":
			switch {
			case strings.Contains(joined, "symbolic-ref"):
				return "origin/main\n", "", nil
			case strings.Contains(joined, "rev-list --count"):
				return "2\n", "", nil
			}
			return "", "", nil
		case "claude":
			return testkit.ClaudeJSON("FIX_COMMITTED: fixed and committed", "d1"), "", nil
		}
		return "", "", nil
	}
	return env
}

// callsMatching returns joined arg strings of calls whose name and args match.
func (e *fakeEnv) callsMatching(name, substr string) []string {
	var out []string
	for _, c := range e.f.Snapshot() {
		joined := strings.Join(c.Args, " ")
		if c.Name == name && strings.Contains(joined, substr) {
			out = append(out, joined)
		}
	}
	return out
}

// newOrch wires a real engine Orchestrator over the fake runner, the same way
// main does in production.
func newOrch(cfg *shared.Config, f *testkit.FakeRunner) *engine.Orchestrator {
	return engine.NewOrchestrator(engine.Deps{
		Cfg:       cfg,
		Host:      infra.NewGitHubWithRetry(f, cfg, testkit.TestRetry),
		Workspace: infra.NewWorktreeAt(f, cfg.RepoPath, testkit.TestRetry),
		NewAgent: func(logDir string) shared.Agent {
			return infra.NewClaude(f, logDir, cfg.ClaudeConfigDir)
		},
	})
}

var _ = fmt.Sprintf // keep fmt for future handlers
