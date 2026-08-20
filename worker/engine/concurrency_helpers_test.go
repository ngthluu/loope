package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ngthluu/loope/worker/shared"
	"github.com/ngthluu/loope/worker/testkit"
)

// gateRunner wraps testkit.FakeRunner and can block a call BEFORE delegating. testkit.FakeRunner
// invokes its handler under its own mutex, so blocking inside a handler would
// serialize every other command; gating here holds no lock while blocked, which
// is what lets a test hold several pipelines in flight at once.
type gateRunner struct {
	inner *testkit.FakeRunner
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

// pipelineIssueRe pulls the issue number out of the worktree directory a
// pipeline's Claude call runs in — worktreePath is <workDir>/issue-<N>.
var pipelineIssueRe = regexp.MustCompile(`issue-(\d+)`)

// gatePipelines makes every pipeline claude call block until release is
// closed, announcing the issue number it belongs to on started. Selection is
// deterministic and makes no claude call, so it always completes ungated.
func gatePipelines(o *Orchestrator, f *testkit.FakeRunner) (started chan int, release chan struct{}) {
	started = make(chan int, 64)
	release = make(chan struct{})
	seen := map[int]bool{}
	var mu sync.Mutex
	rewire(o, &gateRunner{inner: f, gate: func(dir, name, stdin string) chan struct{} {
		if name != "claude" {
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
	}})
	return started, release
}

// slotEnv is a fakeEnv whose eligible list and ai-rework list are settable
// between cycles; selection picks the lowest-numbered eligible issue.
type slotEnv struct {
	*fakeEnv
	mu       sync.Mutex
	eligible []int
	wip      []int
}

func (s *slotEnv) setEligible(nums ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eligible = nums
}

func (s *slotEnv) setWIP(nums ...int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wip = nums
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
	s := &slotEnv{fakeEnv: &fakeEnv{f: &testkit.FakeRunner{}, wtDir: t.TempDir()}, eligible: eligible}
	s.f.Handler = func(c testkit.RCall) (string, string, error) {
		joined := strings.Join(c.Args, " ")
		switch c.Name {
		case "gh":
			switch {
			case strings.HasPrefix(joined, "issue list") && strings.Contains(joined, "--label ai-wip"):
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.listJSON(s.wip, "ai-wip"), "", nil
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
			return testkit.ClaudeEntry("d", "fix_committed", "fixed"), "", nil
		}
		return "", "", nil
	}
	return s
}

// lockedBuf is an io.Writer safe for the concurrent pipeline goroutines that
// now do their own logging.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog redirects the standard logger for the duration of the test and
// returns an accessor for what was written. Pipeline outcomes are logged by the
// goroutine that runs them, so the daemon log is where a failed pipeline now
// surfaces.
func captureLog(t *testing.T) func() string {
	t.Helper()
	var b lockedBuf
	log.SetOutput(&b)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return b.String
}

// runCycle runs one ProcessOnce and drains the pipelines it started, so tests
// can assert on observable state the way they did when ProcessOnce blocked.
func runCycle(o *Orchestrator) error {
	err := o.ProcessOnce(context.Background())
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

// The gate must block a pipeline claude call without holding testkit.FakeRunner's
// mutex — a `gh` call issued while a pipeline is blocked must still complete.
func TestGateRunnerBlocksWithoutHoldingRunnerLock(t *testing.T) {
	f := &testkit.FakeRunner{}
	f.Handler = func(c testkit.RCall) (string, string, error) { return "ok", "", nil }
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

func TestSelectIssuesPicksOldestFirst(t *testing.T) {
	issues := []shared.Issue{{Number: 9}, {Number: 7}, {Number: 8}}
	got := selectIssues(issues, 2)
	if len(got) != 2 || got[0].Number != 7 || got[1].Number != 8 {
		t.Fatalf("selectIssues = %+v, want issues 7 then 8", got)
	}
	if got := selectIssues(nil, 3); len(got) != 0 {
		t.Fatalf("selectIssues(nil) = %+v, want empty", got)
	}
	// The input slice must not be reordered in place.
	if issues[0].Number != 9 {
		t.Fatalf("selectIssues reordered its input: %+v", issues)
	}
}
