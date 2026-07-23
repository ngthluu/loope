package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateRunner wraps fakeRunner and can block a call BEFORE delegating. fakeRunner
// invokes its handler under its own mutex, so blocking inside a handler would
// serialize every other command; gating here holds no lock while blocked, which
// is what lets a test hold several pipelines in flight at once.
type gateRunner struct {
	inner *fakeRunner
	gate  func(dir, name, stdin string) chan struct{} // nil channel = don't block
}

func (g *gateRunner) wait(dir, name, stdin string) {
	if g.gate == nil {
		return
	}
	if ch := g.gate(dir, name, stdin); ch != nil {
		<-ch
	}
}

func (g *gateRunner) Run(ctx context.Context, dir string, env []string, stdin, name string, args ...string) (string, string, error) {
	g.wait(dir, name, stdin)
	return g.inner.Run(ctx, dir, env, stdin, name, args...)
}

func (g *gateRunner) RunStream(ctx context.Context, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (string, error) {
	g.wait(dir, name, stdin)
	return g.inner.RunStream(ctx, dir, env, stdin, w, name, args...)
}

var issueNumRe = regexp.MustCompile(`"number":\s*(\d+)`)

// firstIssueIn returns the lowest-numbered issue mentioned in a triage prompt,
// or 0. Triage marshals the candidate list as JSON, so a prompt's "number"
// fields are exactly the still-eligible candidates.
func firstIssueIn(prompt string) int {
	best := 0
	for _, m := range issueNumRe.FindAllStringSubmatch(prompt, -1) {
		n, _ := strconv.Atoi(m[1])
		if best == 0 || n < best {
			best = n
		}
	}
	return best
}

// pipelineIssueRe pulls the issue number out of the worktree directory a
// pipeline's Claude call runs in — worktreePath is <workDir>/issue-<N>.
var pipelineIssueRe = regexp.MustCompile(`issue-(\d+)`)

// gatePipelines makes every pipeline (non-triage) claude call block until
// release is closed, announcing the issue number it belongs to on started.
// Triage calls are never gated, so selection still completes.
func gatePipelines(o *Orchestrator, f *fakeRunner) (started chan int, release chan struct{}) {
	started = make(chan int, 64)
	release = make(chan struct{})
	seen := map[int]bool{}
	var mu sync.Mutex
	o.runner = &gateRunner{inner: f, gate: func(dir, name, stdin string) chan struct{} {
		if name != "claude" || strings.Contains(stdin, "triage agent") {
			return nil
		}
		n := 0
		if m := pipelineIssueRe.FindStringSubmatch(dir); m != nil {
			n, _ = strconv.Atoi(m[1])
		}
		mu.Lock()
		first := !seen[n]
		seen[n] = true
		mu.Unlock()
		if first {
			started <- n
		}
		return release
	}}
	return started, release
}

// slotEnv is a fakeEnv whose eligible list and ai-rework list are settable
// between cycles, and whose triage picks the lowest-numbered candidate still in
// the prompt.
type slotEnv struct {
	*fakeEnv
	mu       sync.Mutex
	eligible []int
	rework   []int
}

func (s *slotEnv) setEligible(nums ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eligible = nums
}

func (s *slotEnv) setRework(nums ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rework = nums
}

func (s *slotEnv) listJSON(nums []int, label string) string {
	var parts []string
	for _, n := range nums {
		parts = append(parts, fmt.Sprintf(`{"number": %d, "title": "Issue %d", "body": "b", "labels": [{"name": %q}]}`, n, n, label))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func newSlotEnv(t *testing.T, eligible ...int) *slotEnv {
	t.Helper()
	s := &slotEnv{fakeEnv: &fakeEnv{f: &fakeRunner{}, wtDir: t.TempDir()}, eligible: eligible}
	s.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		switch c.name {
		case "gh":
			switch {
			case strings.HasPrefix(joined, "issue list") && strings.Contains(joined, "--label ai-rework"):
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.listJSON(s.rework, "ai-rework"), "", nil
			case strings.HasPrefix(joined, "issue list") && strings.Contains(joined, "--label ai-wip"):
				return "[]", "", nil
			case strings.HasPrefix(joined, "issue list"):
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.listJSON(s.eligible, "ai-agent"), "", nil
			case strings.HasPrefix(joined, "issue view"):
				return `{"title": "T", "body": "b", "comments": []}`, "", nil
			case strings.HasPrefix(joined, "pr create"):
				return "https://github.com/org/repo/pull/99\n", "", nil
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
			if strings.Contains(c.stdin, "triage agent") {
				return claudeJSON(fmt.Sprintf(`{"issueNumber": %d, "kind": "bug", "reason": "r"}`, firstIssueIn(c.stdin)), "t"), "", nil
			}
			return claudeJSON("Fixed and committed.", "d"), "", nil
		}
		return "", "", nil
	}
	return s
}

// prepParkedIn seeds the on-disk residue of a parked issue — preserved
// worktree, recorded session, park cause — so shouldResume accepts it. Same
// shape as prepParked, but for any issue number.
func prepParkedIn(t *testing.T, env *fakeEnv, n int, cause string) {
	t.Helper()
	if err := os.MkdirAll(worktreePath(env.wtDir, n), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(env.wtDir, "logs", fmt.Sprintf("issue-%d", n))
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logDir, "session"), []byte(`{"sessionId":"s1","kind":"bug"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	recordParkCause(logDir, cause)
}

// runCycle runs one ProcessOnce and drains the pipelines it started, so tests
// can assert on observable state the way they did when ProcessOnce blocked.
func runCycle(o *Orchestrator) error {
	err := o.ProcessOnce(context.Background())
	o.Wait()
	return err
}

// resumeCycle is runCycle for the auto-resume path.
func resumeCycle(o *Orchestrator) error {
	err := o.ResumeParked(context.Background())
	o.Wait()
	return err
}

// awaitStarted reads exactly n issue numbers off started, failing the test if
// they don't arrive within 5s.
func awaitStarted(t *testing.T, started chan int, n int) []int {
	t.Helper()
	var got []int
	for i := 0; i < n; i++ {
		select {
		case v := <-started:
			got = append(got, v)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for pipeline %d/%d to start (got %v)", i+1, n, got)
		}
	}
	return got
}

// assertNoStart fails if another pipeline starts within d.
func assertNoStart(t *testing.T, started chan int, d time.Duration) {
	t.Helper()
	select {
	case n := <-started:
		t.Fatalf("pipeline for issue #%d started, want none", n)
	case <-time.After(d):
	}
}

// The gate must block a pipeline claude call without holding fakeRunner's
// mutex — a `gh` call issued while a pipeline is blocked must still complete.
func TestGateRunnerBlocksWithoutHoldingRunnerLock(t *testing.T) {
	f := &fakeRunner{}
	f.handler = func(c rcall) (string, string, error) { return "ok", "", nil }
	release := make(chan struct{})
	entered := make(chan struct{})
	g := &gateRunner{inner: f, gate: func(dir, name, stdin string) chan struct{} {
		if name != "claude" {
			return nil
		}
		close(entered)
		return release
	}}
	go func() { _, _, _ = g.Run(context.Background(), "", nil, "prompt", "claude") }()
	<-entered
	done := make(chan struct{})
	go func() {
		_, _, _ = g.Run(context.Background(), "", nil, "", "gh", "issue", "list")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a gh call was blocked while a claude call sat in the gate")
	}
	close(release)
}

func TestFirstIssueInPicksLowestCandidate(t *testing.T) {
	prompt := `{"number": 9, "title": "a"} {"number": 7, "title": "b"}`
	if got := firstIssueIn(prompt); got != 7 {
		t.Fatalf("firstIssueIn = %d, want 7", got)
	}
	if got := firstIssueIn("no issues here"); got != 0 {
		t.Fatalf("firstIssueIn on empty = %d, want 0", got)
	}
}
