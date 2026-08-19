package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mergeEnv simulates gh/git/claude for merge-resolve flow tests. The git-side
// merge state is a tiny state machine the handler consults: conflict decides
// whether `git merge` stops on conflicts (setting inProgress), and
// claudeResolves decides whether the resolve session concludes the merge
// (clearing inProgress). Handler calls run under fakeRunner's lock, so the
// flags need no extra synchronization.
type mergeEnv struct {
	f       *fakeRunner
	workDir string

	labels         []string // issue 7's GitHub labels
	conflict       bool     // git merge stops on conflicts
	claudeResolves bool     // the resolve session commits the merge
	inProgress     bool     // MERGE_HEAD exists (seed true for a parked prior attempt)
}

func newMergeEnv(t *testing.T, labels ...string) *mergeEnv {
	t.Helper()
	env := &mergeEnv{f: &fakeRunner{}, workDir: t.TempDir(), labels: labels, claudeResolves: true}
	if err := os.MkdirAll(worktreePath(env.workDir, 7), 0o755); err != nil {
		t.Fatal(err)
	}
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		switch c.name {
		case "gh":
			if strings.HasPrefix(joined, "issue list") {
				var ls []string
				for _, l := range env.labels {
					ls = append(ls, `{"name": "`+l+`"}`)
				}
				return `[{"number": 7, "title": "Fix crash", "body": "boom", "labels": [` + strings.Join(ls, ", ") + `]}]`, "", nil
			}
			return "", "", nil
		case "git":
			switch {
			case strings.Contains(joined, "symbolic-ref"):
				return "origin/main\n", "", nil
			case strings.Contains(joined, "rev-parse -q --verify MERGE_HEAD"):
				if env.inProgress {
					return "abc123\n", "", nil
				}
				return "", "", errors.New("exit 1")
			case strings.Contains(joined, "merge --no-edit"):
				if env.conflict {
					env.inProgress = true
					return "", "CONFLICT (content): Merge conflict in main.go", errors.New("exit 1")
				}
				return "", "", nil
			case strings.Contains(joined, "ls-files --unmerged"):
				if env.inProgress {
					return "100644 abc 1\tmain.go\n", "", nil
				}
				return "", "", nil
			}
			return "", "", nil
		case "claude":
			if env.claudeResolves {
				env.inProgress = false
				return claudeJSON("Resolved both files.\n"+mergeResolveSentinel+" resolved", "mr1"), "", nil
			}
			return claudeJSON("Two incompatible designs.\n"+mergeResolveSentinel+" blocked needs a human call", "mr1"), "", nil
		}
		return "", "", nil
	}
	return env
}

func (e *mergeEnv) orchestrator() *Orchestrator {
	cfg := &Config{
		RepoPath: "/clone", RepoSlug: "org/repo", EligibleLabel: "ai-agent",
		MergeResolveLabel: "ai-resolve-merge",
		WorkDir:           e.workDir, StateLabels: defaultStateLabels(),
		Models: Models{Architect: ModelConfig{Model: "opus", Effort: "high"}},
	}
	o := &Orchestrator{cfg: cfg, runner: e.f, gh: NewGitHub(e.f, cfg), wt: &Worktree{runner: e.f, repoPath: cfg.RepoPath}}
	o.gh.retry = testRetry
	o.wt.retry = testRetry
	return o
}

func (e *mergeEnv) callsMatching(name, substr string) []string {
	var out []string
	for _, c := range e.f.calls {
		joined := strings.Join(c.args, " ")
		if c.name == name && strings.Contains(joined, substr) {
			out = append(out, joined)
		}
	}
	return out
}

func (e *mergeEnv) logDir() string {
	return filepath.Join(e.workDir, "logs", "issue-7")
}

func runMergeCycle(o *Orchestrator) error {
	err := o.ProcessMergeResolves(context.Background())
	o.Wait()
	return err
}

func TestMergeResolveCleanMergeSkipsClaudeAndRestoresPrior(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-done", "ai-resolve-merge")
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if n := len(env.callsMatching("claude", "")); n != 0 {
		t.Errorf("clean merge ran %d claude sessions, want 0", n)
	}
	if len(env.callsMatching("git", "push -u origin ai/issue-7")) != 1 {
		t.Error("clean merge must still push the merged branch")
	}
	// Pickup swapped the prior state out for wip, finish swapped it back.
	if got := env.callsMatching("gh", "--remove-label ai-done --add-label ai-wip"); len(got) != 1 {
		t.Errorf("want prior->wip swap at pickup, got %v", got)
	}
	if got := env.callsMatching("gh", "--remove-label ai-wip --add-label ai-done"); len(got) != 1 {
		t.Errorf("want wip->prior swap on success, got %v", got)
	}
	if got := env.callsMatching("gh", "--remove-label ai-resolve-merge"); len(got) != 1 {
		t.Errorf("want the trigger label removed exactly once, got %v", got)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) != 0 {
		t.Error("success must not park")
	}
}

func TestMergeResolveConflictRunsOneClaudeSessionThenPushes(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-done", "ai-resolve-merge")
	env.conflict = true
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	claudes := env.f.calls[:0:0]
	for _, c := range env.f.calls {
		if c.name == "claude" {
			claudes = append(claudes, c)
		}
	}
	if len(claudes) != 1 {
		t.Fatalf("conflicted merge ran %d claude sessions, want exactly 1", len(claudes))
	}
	if want := worktreePath(env.workDir, 7); claudes[0].dir != want {
		t.Errorf("resolve session dir = %q, want the worktree %q", claudes[0].dir, want)
	}
	if !hasArg(claudes[0].args, "--dangerously-skip-permissions") {
		t.Error("resolve session must skip permissions (headless)")
	}
	if !strings.Contains(claudes[0].stdin, "origin/main") {
		t.Error("resolve prompt must name the merged ref")
	}
	if len(env.callsMatching("git", "push -u origin ai/issue-7")) != 1 {
		t.Error("resolved merge must push")
	}
	if got := env.callsMatching("gh", "--remove-label ai-wip --add-label ai-done"); len(got) != 1 {
		t.Errorf("want wip->prior swap on success, got %v", got)
	}
}

// The merge-resolve session must never clobber the issue's persisted pipeline
// session: that file is what a later pipeline re-entry resumes.
func TestMergeResolveDoesNotTouchPipelineSession(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-done", "ai-resolve-merge")
	env.conflict = true
	if err := os.MkdirAll(env.logDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"sessionId":"pipeline-1","kind":"bug","stage":"debug"}`
	if err := os.WriteFile(filepath.Join(env.logDir(), sessionFile), []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(env.logDir(), sessionFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != seed {
		t.Errorf("session file changed to %q, want untouched %q", got, seed)
	}
}

func TestMergeResolveUnresolvedConflictParksWithoutPush(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-done", "ai-resolve-merge")
	env.conflict = true
	env.claudeResolves = false
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("git", "push")) != 0 {
		t.Error("an unconcluded merge must not be pushed")
	}
	if got := env.callsMatching("gh", "--remove-label ai-wip --add-label ai-rework"); len(got) != 1 {
		t.Errorf("want wip->rework park swap, got %v", got)
	}
	// The anti-retry-loop invariant: the trigger label is stripped even on
	// failure, so the scan can't re-fire the same failing merge every cycle.
	if got := env.callsMatching("gh", "--remove-label ai-resolve-merge"); len(got) != 1 {
		t.Errorf("want the trigger label removed exactly once on failure, got %v", got)
	}
	if cause := readParkCause(env.logDir()); !strings.Contains(cause, "did not conclude") {
		t.Errorf("park cause = %q, want the unconcluded-merge explanation", cause)
	}
	// The half-resolved merge is preserved for the next attempt: no abort.
	if len(env.callsMatching("git", "merge --abort")) != 0 {
		t.Error("a failed resolve must never abort the in-progress merge")
	}
}

func TestMergeResolveMissingWorktreeParks(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-done", "ai-resolve-merge")
	if err := os.RemoveAll(worktreePath(env.workDir, 7)); err != nil {
		t.Fatal(err)
	}
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("git", "merge")) != 0 {
		t.Error("no worktree: nothing must be merged")
	}
	if got := env.callsMatching("gh", "--remove-label ai-wip --add-label ai-rework"); len(got) != 1 {
		t.Errorf("want park swap, got %v", got)
	}
	if got := env.callsMatching("gh", "--remove-label ai-resolve-merge"); len(got) != 1 {
		t.Errorf("want the trigger label removed, got %v", got)
	}
	if cause := readParkCause(env.logDir()); !strings.Contains(cause, "no worktree") {
		t.Errorf("park cause = %q, want the missing-worktree explanation", cause)
	}
}

// A queued issue with no state label wears a plain-added ai-wip during the run
// and a plain remove afterwards, returning it to the eligible queue.
func TestMergeResolveNoPriorLabelAddsAndRemovesWIP(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-resolve-merge")
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := env.callsMatching("gh", "--add-label ai-wip"); len(got) != 1 || strings.Contains(got[0], "--remove-label") {
		t.Errorf("want a plain ai-wip add (no swap), got %v", got)
	}
	var bareRemove int
	for _, c := range env.callsMatching("gh", "--remove-label ai-wip") {
		if !strings.Contains(c, "--add-label") {
			bareRemove++
		}
	}
	if bareRemove != 1 {
		t.Errorf("want a plain ai-wip remove on success, got calls %v", env.callsMatching("gh", "--remove-label ai-wip"))
	}
}

// A stale ai-wip (crashed run the sweep hasn't cleared) is waited out, never
// fought over.
func TestMergeResolveSkipsWIPIssue(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-wip", "ai-resolve-merge")
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "issue edit")) != 0 {
		t.Errorf("wip issue must not have its labels touched, got %v", env.callsMatching("gh", "issue edit"))
	}
	if len(env.callsMatching("git", "merge")) != 0 {
		t.Error("wip issue must not be merged")
	}
}

func TestProcessMergeResolvesDisabledWhenLabelEmpty(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-resolve-merge")
	o := env.orchestrator()
	o.cfg.MergeResolveLabel = ""
	if err := runMergeCycle(o); err != nil {
		t.Fatal(err)
	}
	if len(env.f.calls) != 0 {
		t.Errorf("disabled flow made %d calls, want 0", len(env.f.calls))
	}
}

// After a crash, GitHub shows no state label (the sweep stripped the orphaned
// ai-wip) — the prior-label marker is what still knows the issue was ai-done.
func TestMergeResolveRecoversPriorFromMarker(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-resolve-merge")
	if err := os.MkdirAll(env.logDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	recordMergeResolvePrior(env.logDir(), "ai-done")
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := env.callsMatching("gh", "--remove-label ai-wip --add-label ai-done"); len(got) != 1 {
		t.Errorf("want the marker's ai-done restored on success, got %v", got)
	}
	if readMergeResolvePrior(env.logDir()) != "" {
		t.Error("the prior marker must be cleared on the terminal outcome")
	}
}

// A parked prior attempt left MERGE_HEAD and its conflicts in place: the next
// run continues that merge (straight to the resolve session), never re-merges
// or aborts.
func TestMergeResolveContinuesInProgressMerge(t *testing.T) {
	env := newMergeEnv(t, "ai-agent", "ai-rework", "ai-resolve-merge")
	env.inProgress = true
	if err := runMergeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("git", "merge --no-edit")) != 0 {
		t.Error("an in-progress merge must be continued, not re-run")
	}
	var claudes int
	for _, c := range env.f.calls {
		if c.name == "claude" {
			claudes++
		}
	}
	if claudes != 1 {
		t.Errorf("want exactly 1 resolve session, got %d", claudes)
	}
	if len(env.callsMatching("git", "push -u origin ai/issue-7")) != 1 {
		t.Error("the continued, resolved merge must push")
	}
	if got := env.callsMatching("gh", "--remove-label ai-wip --add-label ai-rework"); len(got) != 1 {
		t.Errorf("want the prior ai-rework restored on success, got %v", got)
	}
}
