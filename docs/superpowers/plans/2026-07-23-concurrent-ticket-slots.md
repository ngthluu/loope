# Concurrent Ticket Slots Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `ticketsPerCycle` into a live concurrency budget so every poll cycle tops the in-flight pipeline set back up to the limit instead of blocking on a batch.

**Architecture:** Add an in-process slot ledger (`active` set + `sync.WaitGroup`) to `Orchestrator`, guarded by the existing `mu`. `ProcessOnce` and `ResumeParked` acquire a slot per issue, launch a goroutine, and return immediately; the goroutine logs its own outcome and releases the slot. `runLoop` drains the WaitGroup before returning (on `-once` and on shutdown) so the workDir lock still outlives every pipeline.

**Tech Stack:** Go 1.25.5, standard library only (`sync`, `context`, `errors`, `log`). Tests are stdlib `testing` with the existing `fakeRunner`/`fakeEnv` fakes.

**Spec:** `docs/superpowers/specs/2026-07-23-concurrent-ticket-slots-design.md`

## Global Constraints

- Module is `loope`, a single `package main` at the repo root. New source files go at the repo root, not in subdirectories.
- Go 1.25.5 (`go.mod`). Standard library only — do not add dependencies.
- No new config key. `ticketsPerCycle` keeps its name and its default of `1`; values below 1 are treated as 1.
- The in-process ledger is authoritative for slots. Never derive slot occupancy by counting `ai-wip` issues on GitHub.
- Label semantics are unchanged: `ai-wip` stays the durable marker of an in-flight run, and `SweepOrphans` still depends on it.
- `handleIssue`, `park`, `ship`, `finishDone`, `finishNeedsInfo`, `abort`, `Rework`, `SweepOrphans`, `shouldResume`, `noteResumeFailure`, `clearResumeState`, and `serve.go` are **not** modified by this plan.
- Every test run must pass under `-race`: `go test ./... -race`.
- Commit after every task with a conventional-commit message.

## Assumptions (headless-mode calls not spelled out in the spec)

1. **`slots()` does not take `mu`.** The spec says all four helpers take `mu`, but Go's `sync.Mutex` is not reentrant and `tryAcquire`/`freeSlots` both need `slots()` while holding the lock. `cfg` is immutable after construction, so `slots()` reads it lock-free and the callers hold `mu`. The invariant the spec wants — every ledger mutation happens under `mu` — is preserved.
2. **The ledger lives in a new file `slots.go`** rather than being appended to `loop.go` (which is already ~500 lines). Its tests live in `slots_test.go`. The `active`/`inFlight` struct fields stay on `Orchestrator` in `loop.go`, next to the existing `mu`.
3. **A bulk `filterInactive([]Issue) []Issue` helper** is added alongside the four spec helpers, so `ProcessOnce` filters the eligible list under one lock acquisition instead of N.
4. **A test-only `gateRunner`** is introduced because `fakeRunner` invokes its handler while holding its own mutex — blocking inside a handler would serialize every other command and deadlock any multi-pipeline test. `gateRunner` wraps `*fakeRunner` and blocks *before* delegating, so it holds no lock while blocked.
5. **Test helpers `runCycle` / `resumeCycle`** (call + `Wait()`) make the migration of existing tests mechanical.

---

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `slots.go` | Create | Slot ledger: `slots`, `tryAcquire`, `release`, `freeSlots`, `filterInactive`, `Wait`. |
| `slots_test.go` | Create | Unit tests for the ledger. |
| `loop.go` | Modify | `Orchestrator` gains `active`/`inFlight`; `ProcessOnce` and `ResumeParked` become non-blocking; `selectIssues` takes a limit. |
| `concurrency_helpers_test.go` | Create | `gateRunner`, `slotEnv`, `runCycle`, `resumeCycle` test scaffolding. |
| `slots_flow_test.go` | Create | Concurrency behavior tests (top-up, budget, short-circuit, in-flight filter, shared budget with resumes). |
| `loop_test.go` | Modify | Existing `ProcessOnce*`/`ResumeParked*` tests migrated to `Wait()` + observable-state assertions. |
| `main.go` | Modify | `runLoop` drains in-flight work before returning. |
| `main_test.go` | Modify | Drain-on-exit test. |
| `README.md` | Modify | `ticketsPerCycle` row + "How it works" note. |

---

### Task 1: Slot ledger

**Files:**
- Create: `slots.go`
- Create: `slots_test.go`
- Modify: `loop.go:16-29` (add `active` and `inFlight` fields to `Orchestrator`)

**Interfaces:**
- Consumes: `Orchestrator` (`loop.go`), `Config.TicketsPerCycle` (`config.go`), `Issue` (`github.go`, has field `Number int`).
- Produces:
  - `func (o *Orchestrator) slots() int` — effective budget, min 1. Caller must hold `mu`.
  - `func (o *Orchestrator) tryAcquire(n int) bool` — takes `mu`.
  - `func (o *Orchestrator) release(n int)` — takes `mu`.
  - `func (o *Orchestrator) freeSlots() int` — takes `mu`, never negative.
  - `func (o *Orchestrator) filterInactive(issues []Issue) []Issue` — takes `mu`.
  - `func (o *Orchestrator) Wait()` — blocks until every acquired slot is released.

- [ ] **Step 1: Write the failing ledger tests**

Create `slots_test.go`:

```go
package main

import "testing"

// testOrch builds a bare Orchestrator with just the config the ledger reads.
func testOrch(budget int) *Orchestrator {
	return &Orchestrator{cfg: &Config{TicketsPerCycle: budget}}
}

func TestTryAcquireRespectsBudget(t *testing.T) {
	o := testOrch(2)
	if !o.tryAcquire(7) {
		t.Fatal("first acquire must succeed")
	}
	if !o.tryAcquire(8) {
		t.Fatal("second acquire must succeed within budget 2")
	}
	if o.tryAcquire(9) {
		t.Fatal("third acquire must fail: budget is 2")
	}
}

func TestTryAcquireRefusesIssueAlreadyInFlight(t *testing.T) {
	o := testOrch(3)
	if !o.tryAcquire(7) {
		t.Fatal("first acquire must succeed")
	}
	if o.tryAcquire(7) {
		t.Fatal("an issue already in flight must not be acquired twice")
	}
	if o.freeSlots() != 2 {
		t.Fatalf("freeSlots = %d, want 2 (the refused acquire must not consume a slot)", o.freeSlots())
	}
}

func TestReleaseFreesExactlyOneSlot(t *testing.T) {
	o := testOrch(2)
	o.tryAcquire(7)
	o.tryAcquire(8)
	if o.freeSlots() != 0 {
		t.Fatalf("freeSlots = %d, want 0", o.freeSlots())
	}
	o.release(7)
	if o.freeSlots() != 1 {
		t.Fatalf("freeSlots after one release = %d, want 1", o.freeSlots())
	}
	if !o.tryAcquire(9) {
		t.Fatal("the freed slot must be reusable")
	}
	if o.freeSlots() != 0 {
		t.Fatalf("freeSlots = %d, want 0", o.freeSlots())
	}
}

func TestBudgetBelowOneClampsToOne(t *testing.T) {
	for _, budget := range []int{0, -5} {
		o := testOrch(budget)
		if got := o.freeSlots(); got != 1 {
			t.Fatalf("budget %d: freeSlots = %d, want 1", budget, got)
		}
		if !o.tryAcquire(7) {
			t.Fatalf("budget %d: one acquire must succeed", budget)
		}
		if got := o.freeSlots(); got != 0 {
			t.Fatalf("budget %d: freeSlots = %d, want 0", budget, got)
		}
	}
}

func TestFreeSlotsNeverNegative(t *testing.T) {
	o := testOrch(1)
	o.tryAcquire(7)
	// Budget shrinks under a live pipeline (e.g. a hand-edited config reload).
	o.cfg.TicketsPerCycle = 1
	o.mu.Lock()
	o.active[8] = struct{}{} // simulate an over-subscribed ledger
	o.mu.Unlock()
	if got := o.freeSlots(); got != 0 {
		t.Fatalf("freeSlots = %d, want 0 (must floor at zero)", got)
	}
}

func TestFilterInactiveDropsInFlightIssues(t *testing.T) {
	o := testOrch(3)
	o.tryAcquire(7)
	got := o.filterInactive([]Issue{{Number: 7}, {Number: 8}, {Number: 9}})
	if len(got) != 2 || got[0].Number != 8 || got[1].Number != 9 {
		t.Fatalf("filterInactive = %+v, want issues 8 and 9", got)
	}
}

func TestWaitReturnsAfterEveryReleaseAndIsSafeWhenIdle(t *testing.T) {
	o := testOrch(2)
	o.Wait() // no acquires: must return immediately
	o.tryAcquire(7)
	done := make(chan struct{})
	go func() {
		o.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Wait returned while a slot was still held")
	default:
	}
	o.release(7)
	<-done
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'TestTryAcquire|TestRelease|TestBudget|TestFreeSlots|TestFilterInactive|TestWait' -v`
Expected: FAIL to build — `o.tryAcquire undefined`, `o.freeSlots undefined`, `o.active undefined`, etc.

- [ ] **Step 3: Add the ledger fields to Orchestrator**

In `loop.go`, replace the comment block and fields at lines 22-29 with:

```go
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
```

- [ ] **Step 4: Write the ledger**

Create `slots.go`:

```go
package main

// The slot ledger turns ticketsPerCycle into a live concurrency budget. A cycle
// tops the in-flight set back up to the budget and returns; pipelines started in
// different cycles run side by side. The in-process ledger is authoritative —
// slots are NOT derived from counting ai-wip issues on GitHub, because the
// daemon holds an exclusive workDir lock (so no other instance can own live
// pipelines) and the label can lag or fail to apply.

// slots is the effective budget: ticketsPerCycle, clamped to a minimum of 1.
// Callers must hold mu (cfg is immutable, but every caller is already inside the
// critical section and mu is not reentrant).
func (o *Orchestrator) slots() int {
	n := o.cfg.TicketsPerCycle
	if n < 1 {
		n = 1
	}
	return n
}

// tryAcquire claims a slot for issue n, reporting whether it got one. It refuses
// when n is already in flight or the budget is full.
//
// The already-in-flight check is not redundant with the ai-wip label check. It
// closes two real windows: between launching a pipeline and its AddLabel(ai-wip)
// landing the issue still looks eligible to ListEligibleIssues, and park swaps
// ai-wip->ai-rework before the pipeline goroutine returns, so ResumeParked in the
// same cycle could otherwise resume an issue whose goroutine still holds its
// worktree.
func (o *Orchestrator) tryAcquire(n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.active == nil {
		o.active = map[int]struct{}{}
	}
	if _, busy := o.active[n]; busy {
		return false
	}
	if len(o.active) >= o.slots() {
		return false
	}
	o.active[n] = struct{}{}
	o.inFlight.Add(1)
	return true
}

// release returns issue n's slot. Every successful tryAcquire must be paired
// with exactly one release, deferred first in the goroutine so it runs last.
func (o *Orchestrator) release(n int) {
	o.mu.Lock()
	delete(o.active, n)
	o.mu.Unlock()
	o.inFlight.Done()
}

// freeSlots reports how many pipelines may still be started, floored at zero.
func (o *Orchestrator) freeSlots() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	free := o.slots() - len(o.active)
	if free < 0 {
		return 0
	}
	return free
}

// filterInactive drops issues that already have a pipeline in flight, so a stale
// listing can't start a second run for one.
func (o *Orchestrator) filterInactive(issues []Issue) []Issue {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := issues[:0:0]
	for _, is := range issues {
		if _, busy := o.active[is.Number]; busy {
			continue
		}
		out = append(out, is)
	}
	return out
}

// Wait blocks until every in-flight pipeline and resume has finished. runLoop
// calls it before returning so the workDir lock outlives all work, exactly as it
// did when ProcessOnce blocked on its own WaitGroup.
func (o *Orchestrator) Wait() { o.inFlight.Wait() }
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./... -run 'TestTryAcquire|TestRelease|TestBudget|TestFreeSlots|TestFilterInactive|TestWait' -race -v`
Expected: PASS (7 tests).

- [ ] **Step 6: Verify nothing else broke**

Run: `go build ./... && go test ./... -race`
Expected: PASS — the ledger is not wired up yet, so all existing tests still pass.

- [ ] **Step 7: Commit**

```bash
git add slots.go slots_test.go loop.go
git commit -m "feat: add slot ledger for concurrent ticket budget"
```

---

### Task 2: Concurrency test harness

**Files:**
- Create: `concurrency_helpers_test.go`

**Interfaces:**
- Consumes: `fakeRunner` (`helpers_test.go`), `fakeEnv` (`loop_test.go`), `Orchestrator`, `Runner` (`runner.go`, methods `Run(ctx, dir string, env []string, stdin, name string, args ...string) (string, string, error)` and `RunStream(ctx, dir string, env []string, stdin string, w io.Writer, name string, args ...string) (string, error)`).
- Produces:
  - `type gateRunner struct { inner *fakeRunner; gate func(dir, name, stdin string) chan struct{} }` implementing `Runner`.
  - `func gatePipelines(o *Orchestrator, f *fakeRunner) (started chan int, release chan struct{})` — blocks each pipeline's non-triage `claude` call until `release` closes, announcing the blocked issue number on `started`.
  - `func newSlotEnv(t *testing.T, eligible ...int) *slotEnv` with fields `*fakeEnv`, methods `setEligible(nums ...int)`, `setRework(nums ...int)`.
  - `func runCycle(o *Orchestrator) error` — `ProcessOnce` + `Wait`.
  - `func resumeCycle(o *Orchestrator) error` — `ResumeParked` + `Wait`.
  - `func awaitStarted(t *testing.T, started chan int, n int) []int` — reads exactly n issue numbers or fails after 5s.

**Why a new runner wrapper:** `fakeRunner.Run` calls its `handler` while holding `fakeRunner.mu` (`helpers_test.go:36-40`). Blocking inside a handler therefore blocks every other command, so two pipelines can never be blocked at once. `gateRunner` blocks *before* delegating and holds no lock.

**Why gating `o.runner` is enough:** `handleIssue` builds its `Claude` with `o.runner`, and `selectIssues` builds the triage `Claude` with `o.runner` too, while `o.gh` and `o.wt` keep the raw `*fakeRunner`. Gating only `o.runner` (and skipping triage prompts) blocks pipelines while `gh`/`git` calls keep flowing.

- [ ] **Step 1: Write the harness**

Create `concurrency_helpers_test.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
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
```

- [ ] **Step 2: Write a smoke test for the harness**

Append to `concurrency_helpers_test.go`:

```go
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
```

- [ ] **Step 3: Run the smoke tests**

Run: `go test ./... -run 'TestGateRunner|TestFirstIssueIn' -race -v`
Expected: PASS (2 tests). If the build fails on `runCycle`/`resumeCycle` being unused, that's fine — Go only rejects unused *locals*, not unused package-level funcs.

- [ ] **Step 4: Commit**

```bash
git add concurrency_helpers_test.go
git commit -m "test: add concurrency harness for slot-based cycles"
```

---

### Task 3: Non-blocking `ProcessOnce`

**Files:**
- Modify: `loop.go:66-139` (`ProcessOnce`, `selectIssues`)
- Create: `slots_flow_test.go` (first test only; more land in Task 4)
- Modify: `loop_test.go` (migrate existing `ProcessOnce*` tests)

**Interfaces:**
- Consumes: `tryAcquire`, `release`, `freeSlots`, `filterInactive`, `Wait` (Task 1); `runCycle`, `newSlotEnv`, `gatePipelines`, `awaitStarted` (Task 2).
- Produces:
  - `func (o *Orchestrator) ProcessOnce(ctx context.Context) error` — returns only listing/selection/`DefaultBranch` errors; never a pipeline error.
  - `func (o *Orchestrator) selectIssues(ctx context.Context, issues []Issue, limit int) ([]pick, error)` — the limit is now a parameter.

- [ ] **Step 1: Write the failing test — `ProcessOnce` returns before pipelines finish**

Create `slots_flow_test.go`:

```go
package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ProcessOnce must launch pipelines and return, so the next poll cycle can top
// the in-flight set back up instead of waiting for the batch.
func TestProcessOnceReturnsBeforePipelinesFinish(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	returned := make(chan error, 1)
	go func() { returned <- o.ProcessOnce(context.Background()) }()

	awaitStarted(t, started, 1)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("ProcessOnce error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessOnce did not return while its pipeline was still running")
	}

	close(release)
	o.Wait()
	if n := len(env.callsMatching("gh", "pr create")); n != 1 {
		t.Fatalf("pr create count = %d, want 1 after Wait", n)
	}
	if free := o.freeSlots(); free != 1 {
		t.Fatalf("freeSlots after drain = %d, want 1 (slot released)", free)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./... -run TestProcessOnceReturnsBeforePipelinesFinish -race -v`
Expected: FAIL — `ProcessOnce did not return while its pipeline was still running` (it currently blocks on `wg.Wait()`).

- [ ] **Step 3: Rewrite `ProcessOnce`**

In `loop.go`, replace the whole `ProcessOnce` function (lines 62-107, comment included) with:

```go
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
```

- [ ] **Step 4: Make `selectIssues` take the limit**

In `loop.go`, replace the head of `selectIssues` (its doc comment and the first three lines of the body) with:

```go
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
```

The rest of the function body (`remaining := issues` onward) is unchanged.

- [ ] **Step 5: Fix the `sync` import if the compiler complains**

`ProcessOnce` no longer uses `sync.WaitGroup` directly, but `Orchestrator` still has `sync.Mutex` and `sync.WaitGroup` fields, so the `sync` import in `loop.go` stays. Run: `go build ./...`
Expected: build succeeds. If it reports `"errors" imported and not used`, restore it — `errors.Join`, `errors.As`, and `errors.New` are all still used elsewhere in the file.

- [ ] **Step 6: Run the new test**

Run: `go test ./... -run TestProcessOnceReturnsBeforePipelinesFinish -race -v`
Expected: PASS.

- [ ] **Step 7: See which existing tests now fail**

Run: `go test ./... -race`
Expected: FAIL in `loop_test.go` — tests that asserted on the returned pipeline error, and tests that assert state written by a pipeline that hasn't finished yet.

- [ ] **Step 8: Migrate the ProcessOnce tests that expect success**

In `loop_test.go`, these call sites must wait for the pipeline before asserting. Replace each occurrence of the pattern `<orch>.ProcessOnce(context.Background())` with `runCycle(<orch>)`:

- `TestProcessOnceLowConfidenceEscalatesToNeedsInfo` (line ~101):
  ```go
	o := env.orchestrator()
	o.cfg.ConfidenceThreshold = 70
	if err := runCycle(o); err != nil {
		t.Fatalf("needs-info is a clean outcome, want nil error, got %v", err)
	}
  ```
- `TestProcessOnceHappyPathBug`, `TestProcessOnceUsesConfiguredStateLabels`, `TestProcessOnceRecordsLocalStateDone`, `TestProcessOnceAlreadyDoneClosesIssue`: each becomes
  ```go
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
  ```
- `TestProcessOnceHandlesMultipleTickets` (line ~485):
  ```go
	if err := runCycle(o); err != nil {
		t.Fatal(err)
	}
  ```
- `TestProcessOnceNoIssuesIsNoop`: unchanged in shape, but use `runCycle` for consistency.

- [ ] **Step 9: Migrate the ProcessOnce tests that expected a pipeline error**

Pipeline errors are no longer returned. Each of these drops its `err` assertion and keeps (or gains) the observable-state assertions.

`TestProcessOnceFailurePathParksForRework` — replace its first three lines:

```go
func TestProcessOnceFailurePathParksForRework(t *testing.T) {
	env := newFakeEnv(t)
	env.failClaude = true
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("a failing pipeline must not be returned from the cycle, got %v", err)
	}
```

The rest of the test body (label-swap, worktree, PR, push assertions) is unchanged.

`TestParkWritesCauseAndShipClearsIt` — replace both `ProcessOnce` blocks:

```go
	env := newFakeEnv(t)
	env.failClaude = true
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	logDir := filepath.Join(env.wtDir, "logs", "issue-7")
	if got := readParkCause(logDir); got == "" {
		t.Fatal("park must record the failure cause")
	}

	// A later successful run through ship() clears the stale cause.
	env2 := newFakeEnv(t)
	logDir2 := filepath.Join(env2.wtDir, "logs", "issue-7")
	recordParkCause(logDir2, "old cause")
	if err := runCycle(env2.orchestrator()); err != nil {
		t.Fatal(err)
	}
	if got := readParkCause(logDir2); got != "" {
		t.Errorf("ship success must clear park-cause, got %q", got)
	}
```

`TestProcessOnceRecordsLocalStateRework`:

```go
func TestProcessOnceRecordsLocalStateRework(t *testing.T) {
	env := newFakeEnv(t)
	env.failClaude = true
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	if got := env.readLocalState(7); got != "ai-rework" {
		t.Fatalf("local state marker = %q, want ai-rework", got)
	}
}
```

`TestToolingFailureDoesNotMarkFailed` — replace its `ProcessOnce` block with:

```go
	if err := runCycle(env.orchestrator()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
```

The remaining assertions in that test are unchanged.

`TestHandleIssuePanicParksIssue` — the panic is no longer returned, so assert on the recorded park cause instead:

```go
// A panic anywhere inside a single issue's pipeline must not take down the
// daemon or its sibling pipelines: the goroutine recovers, parks the issue
// (panic text is non-resumable, so it waits for a human), and releases its slot.
func TestHandleIssuePanicParksIssue(t *testing.T) {
	env := newFakeEnv(t)
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		prompt := c.stdin
		if c.name == "claude" && !strings.Contains(prompt, "triage agent") {
			panic("pipeline bug")
		}
		return base(c)
	}
	o := env.orchestrator()
	if err := runCycle(o); err != nil {
		t.Fatalf("cycle error = %v, want nil (the panic is handled in the pipeline)", err)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) == 0 {
		t.Error("a panicking pipeline must park the issue for rework")
	}
	cause := readParkCause(filepath.Join(env.wtDir, "logs", "issue-7"))
	if !strings.Contains(cause, "panic") {
		t.Errorf("park cause = %q, want it to record the panic", cause)
	}
	if free := o.freeSlots(); free != 1 {
		t.Errorf("freeSlots after a panicking pipeline = %d, want 1 (slot must be released)", free)
	}
}
```

- [ ] **Step 10: Run the full suite**

Run: `go test ./... -race`
Expected: PASS. If a test still fails because a pipeline hasn't finished, it is missing its `runCycle`/`Wait()` — fix that call site the same way.

- [ ] **Step 11: Commit**

```bash
git add loop.go loop_test.go slots_flow_test.go
git commit -m "feat: make ProcessOnce launch pipelines without blocking the cycle"
```

---

### Task 4: Cycle-level slot behavior tests

**Files:**
- Modify: `slots_flow_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-3. No production code changes are expected; if a test fails, the fix belongs in `slots.go`/`loop.go`.

- [ ] **Step 1: Write the top-up test (the issue's scenario)**

Append to `slots_flow_test.go`:

```go
// The reported scenario: budget 3, two pipelines already in flight from an
// earlier cycle, a third issue labelled while they run. The next cycle must
// start exactly that third issue without waiting for the first two.
func TestProcessOnceTopsUpAcrossCycles(t *testing.T) {
	env := newSlotEnv(t, 7, 8)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 3
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("first cycle error = %v, want nil", err)
	}
	first := awaitStarted(t, started, 2)
	if len(first) != 2 {
		t.Fatalf("first cycle started %v, want two pipelines", first)
	}

	// A third issue becomes eligible while the first two are still blocked. The
	// listing still contains the two in-flight ones (their labels are applied,
	// but a stale listing is exactly the case the filter must handle).
	env.setEligible(7, 8, 9)
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("second cycle error = %v, want nil", err)
	}
	third := awaitStarted(t, started, 1)
	if third[0] != 9 {
		t.Fatalf("second cycle started issue #%d, want #9", third[0])
	}
	if free := o.freeSlots(); free != 0 {
		t.Fatalf("freeSlots = %d, want 0 with three pipelines in flight", free)
	}

	close(release)
	o.Wait()
	if n := len(env.callsMatching("gh", "pr create")); n != 3 {
		t.Fatalf("pr create count = %d, want 3", n)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./... -run TestProcessOnceTopsUpAcrossCycles -race -v`
Expected: PASS.

- [ ] **Step 3: Write the budget-enforcement and short-circuit tests**

Append to `slots_flow_test.go`:

```go
// Budget 2 with three eligible issues: only two pipelines start; the third
// waits for a completion to free a slot.
func TestProcessOnceRespectsBudget(t *testing.T) {
	env := newSlotEnv(t, 7, 8, 9)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 2
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	awaitStarted(t, started, 2)
	assertNoStart(t, started, 200*time.Millisecond)

	// A cycle at a full budget starts nothing.
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("second cycle error = %v, want nil", err)
	}
	assertNoStart(t, started, 200*time.Millisecond)

	close(release)
	o.Wait()

	// Slots freed: the third issue now starts.
	env.setEligible(9)
	if err := runCycle(o); err != nil {
		t.Fatalf("third cycle error = %v, want nil", err)
	}
	wip := env.callsMatching("gh", "--add-label ai-wip")
	if len(wip) != 3 {
		t.Fatalf("ai-wip labels = %d, want 3 (all three issues eventually run)", len(wip))
	}
}

// With no free slot, a cycle must not even ask GitHub for the queue.
func TestProcessOnceFullBudgetSkipsListing(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator() // budget clamps to 1
	if !o.tryAcquire(42) {
		t.Fatal("setup: acquire must succeed")
	}
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	if got := env.callsMatching("gh", "issue list"); len(got) != 0 {
		t.Fatalf("a full budget must make no gh issue list call, got %v", got)
	}
	o.release(42)
}

// A stale listing that still includes an in-flight issue must not start a
// second pipeline for it.
func TestProcessOnceFiltersInFlightIssues(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 3
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("first cycle error = %v, want nil", err)
	}
	awaitStarted(t, started, 1)

	// Same listing again: #7 is in flight, nothing else is eligible.
	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("second cycle error = %v, want nil", err)
	}
	assertNoStart(t, started, 200*time.Millisecond)

	close(release)
	o.Wait()
	if wip := env.callsMatching("gh", "--add-label ai-wip"); len(wip) != 1 {
		t.Fatalf("ai-wip labels = %d, want 1 (no duplicate pipeline for #7)", len(wip))
	}
	// The second cycle must not have burned a triage call either: the filtered
	// list was empty, so selection was skipped.
	if triage := env.callsMatching("claude", ""); len(triage) == 0 {
		t.Fatal("sanity: the first cycle should have made claude calls")
	}
}
```

- [ ] **Step 4: Run them**

Run: `go test ./... -run 'TestProcessOnceRespectsBudget|TestProcessOnceFullBudgetSkipsListing|TestProcessOnceFiltersInFlightIssues' -race -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Run the whole suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add slots_flow_test.go
git commit -m "test: cover slot budget, top-up, and in-flight filtering"
```

---

### Task 5: `ResumeParked` shares the budget

**Files:**
- Modify: `loop.go:304-333` (`ResumeParked`)
- Modify: `loop_test.go` (migrate `TestResumeParked*`)
- Modify: `slots_flow_test.go` (new shared-budget tests)

**Interfaces:**
- Consumes: `tryAcquire`, `release`, `freeSlots` (Task 1); `shouldResume`, `noteResumeFailure`, `clearResumeState`, `Rework`, `park` (unchanged); `resumeCycle`, `newSlotEnv`, `gatePipelines` (Task 2).
- Produces: `func (o *Orchestrator) ResumeParked(ctx context.Context) error` — returns only the listing error.

- [ ] **Step 1: Write the failing shared-budget tests**

Append to `slots_flow_test.go`:

```go
// Resumes draw from the same budget as new work: with every slot taken by a
// pipeline, no resume starts.
func TestResumeParkedYieldsToFullBudget(t *testing.T) {
	env := newSlotEnv(t, 7)
	env.setRework(11)
	prepParkedIn(t, env.fakeEnv, 11, "api status 429: usage limit")
	o := env.orchestrator() // budget clamps to 1
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	awaitStarted(t, started, 1)

	if err := o.ResumeParked(context.Background()); err != nil {
		t.Fatalf("ResumeParked error = %v, want nil", err)
	}
	if got := env.callsMatching("claude", "--resume"); len(got) != 0 {
		t.Fatalf("no slot free, want no resume, got %v", got)
	}

	close(release)
	o.Wait()
}

// An issue whose pipeline is still in flight must not be resumed, even after
// park has already swapped its label to ai-rework.
func TestResumeParkedSkipsIssueStillInFlight(t *testing.T) {
	env := newSlotEnv(t, 7)
	env.setRework(7)
	prepParkedIn(t, env.fakeEnv, 7, "api status 429: usage limit")
	o := env.orchestrator()
	o.cfg.TicketsPerCycle = 3
	started, release := gatePipelines(o, env.f)

	if err := o.ProcessOnce(context.Background()); err != nil {
		t.Fatalf("cycle error = %v, want nil", err)
	}
	awaitStarted(t, started, 1)

	if err := o.ResumeParked(context.Background()); err != nil {
		t.Fatalf("ResumeParked error = %v, want nil", err)
	}
	if got := env.callsMatching("claude", "--resume"); len(got) != 0 {
		t.Fatalf("issue #7 is in flight, want no resume, got %v", got)
	}

	close(release)
	o.Wait()
}

// With a free slot and no competing work, a resume starts and returns before
// the resume session finishes.
func TestResumeParkedRunsConcurrently(t *testing.T) {
	env := newSlotEnv(t) // nothing eligible
	env.setRework(11)
	prepParkedIn(t, env.fakeEnv, 11, "api status 429: usage limit")
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	returned := make(chan error, 1)
	go func() { returned <- o.ResumeParked(context.Background()) }()
	awaitStarted(t, started, 1)
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("ResumeParked error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ResumeParked did not return while its resume was still running")
	}
	close(release)
	o.Wait()
	if got := env.callsMatching("claude", "--resume"); len(got) == 0 {
		t.Fatal("want the saved session resumed")
	}
	if free := o.freeSlots(); free != 1 {
		t.Fatalf("freeSlots after drain = %d, want 1", free)
	}
}
```

The tests need a park-state seeder that takes an issue number. The existing
`prepParked` (`loop_test.go:643`) hardcodes issue 7, so add a number-taking
sibling to `concurrency_helpers_test.go`, mirroring it exactly:

```go
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
```

Add `"os"` and `"path/filepath"` to that file's imports.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./... -run 'TestResumeParkedYieldsToFullBudget|TestResumeParkedSkipsIssueStillInFlight|TestResumeParkedRunsConcurrently' -race -v`
Expected: FAIL — resumes currently run sequentially inside `ResumeParked` and ignore the ledger, so `--resume` calls appear where the tests expect none, and `ResumeParked` blocks until the session finishes.

- [ ] **Step 3: Rewrite `ResumeParked`**

In `loop.go`, replace the whole `ResumeParked` function (its doc comment included) with:

```go
// ResumeParked scans ai-rework issues and re-runs Rework on the ones parked for
// a transient, resumable cause (usage/rate limit, turn/budget ceiling, network
// outage). Genuine errors have no resumable park cause and stay parked for a
// human. Each issue backs off exponentially between attempts (5m doubling to
// 60m) so a still-active usage limit isn't hammered every poll cycle.
//
// Resumes draw from the same TicketsPerCycle budget as new work and run
// concurrently with it. ProcessOnce runs first in a cycle and so gets first
// claim on free slots; resumes take what's left and, having backoff, come back.
// Only the listing error is returned — each resume logs its own outcome.
func (o *Orchestrator) ResumeParked(ctx context.Context) error {
	if o.freeSlots() == 0 {
		return nil
	}
	issues, err := o.gh.ListIssuesWithLabel(ctx, o.cfg.StateLabels.Rework)
	if err != nil {
		return err
	}
	for _, is := range issues {
		if ctx.Err() != nil {
			break
		}
		if o.freeSlots() == 0 {
			break
		}
		n := is.Number
		if !o.shouldResume(n) {
			continue
		}
		// park swaps ai-wip->ai-rework before its pipeline goroutine returns, so
		// an issue can look parked while its worktree is still owned by a live
		// pipeline. tryAcquire is what refuses that.
		if !o.tryAcquire(n) {
			continue
		}
		go func(n int) {
			defer o.release(n)
			defer func() {
				if r := recover(); r != nil {
					log.Printf("issue #%d: resume panic: %v\n%s", n, r, debug.Stack())
					_ = o.park(ctx, n, o.cfg.StateLabels.Rework, fmt.Errorf("panic: %v", r))
				}
			}()
			log.Printf("issue #%d: auto-resuming parked work", n)
			if err := o.Rework(ctx, n); err != nil {
				log.Printf("auto-resume #%d failed: %v", n, err)
				o.noteResumeFailure(n)
				return
			}
			o.clearResumeState(n)
		}(n)
	}
	return nil
}
```

`noteResumeFailure` and `clearResumeState` take `mu` themselves, so they are safe here — just never call them while `mu` is already held.

- [ ] **Step 4: Run the new tests**

Run: `go test ./... -run 'TestResumeParkedYieldsToFullBudget|TestResumeParkedSkipsIssueStillInFlight|TestResumeParkedRunsConcurrently' -race -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Migrate the existing `ResumeParked` tests**

In `loop_test.go`, each `ResumeParked` call must be followed by a drain, and the backoff test can no longer read the failure off the return value.

`TestResumeParkedResumesAndShips`:

```go
	o := env.orchestrator()
	if err := resumeCycle(o); err != nil {
		t.Fatal(err)
	}
```

`TestResumeParkedSkipsNonResumable` and `TestResumeParkedSkipsMissingWorktree`:

```go
	if err := resumeCycle(env.orchestrator()); err != nil {
		t.Fatal(err)
	}
```

`TestResumeParkedBacksOffAfterFailure` — replace the three `ResumeParked` calls and the first assertion:

```go
	if err := resumeCycle(o); err != nil {
		t.Fatalf("listing succeeded, want nil error, got %v", err)
	}
	first := len(env.callsMatching("claude", "--resume"))
	if first != 1 {
		t.Fatalf("claude resume calls = %d, want 1", first)
	}

	// Same instant: still inside the 5-minute backoff window.
	_ = resumeCycle(o)
	if got := len(env.callsMatching("claude", "--resume")); got != first {
		t.Errorf("resume inside backoff window: calls = %d, want %d", got, first)
	}

	// Past the window: retries.
	now = now.Add(6 * time.Minute)
	_ = resumeCycle(o)
	if got := len(env.callsMatching("claude", "--resume")); got != first+1 {
		t.Errorf("resume after backoff: calls = %d, want %d", got, first+1)
	}
```

- [ ] **Step 6: Run the full suite**

Run: `go test ./... -race`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add loop.go loop_test.go slots_flow_test.go concurrency_helpers_test.go
git commit -m "feat: run auto-resumes concurrently on the shared slot budget"
```

---

### Task 6: Drain on exit, and docs

**Files:**
- Modify: `main.go:94-120` (`runLoop`)
- Modify: `main_test.go` (drain test)
- Modify: `README.md:172` and `README.md:36-59`

**Interfaces:**
- Consumes: `Orchestrator.Wait` (Task 1), `runLoop(ctx context.Context, o *Orchestrator, cfg *Config, once, sweep bool)`, `newSlotEnv`/`gatePipelines`/`awaitStarted` (Task 2).
- Produces: no new symbols; `runLoop` keeps its signature and gains the drain.

- [ ] **Step 1: Write the failing drain test**

Append to `main_test.go`:

```go
// The workDir lock is released by main's deferred release() after runLoop
// returns, so runLoop must not return while pipelines are still running — a
// second daemon could otherwise steal live ai-wip work. Before slots, this held
// because ProcessOnce blocked; now it must be explicit.
func TestRunLoopOnceDrainsInFlightPipelines(t *testing.T) {
	env := newSlotEnv(t, 7)
	o := env.orchestrator()
	started, release := gatePipelines(o, env.f)

	done := make(chan struct{})
	go func() {
		runLoop(context.Background(), o, o.cfg, true /* once */, false /* sweep */)
		close(done)
	}()

	awaitStarted(t, started, 1)
	select {
	case <-done:
		t.Fatal("runLoop returned while a pipeline was still in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop did not return after pipelines drained")
	}
	if n := len(env.callsMatching("gh", "pr create")); n != 1 {
		t.Fatalf("pr create count = %d, want 1 (the pipeline must have completed)", n)
	}
}
```

Ensure `main_test.go` imports `context`, `testing`, and `time`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./... -run TestRunLoopOnceDrainsInFlightPipelines -race -v`
Expected: FAIL — `runLoop returned while a pipeline was still in flight`.

- [ ] **Step 3: Add the drains to `runLoop`**

In `main.go`, update the doc comment and the two exit points:

```go
// runLoop drives the poll cycle forever: one startup orphan sweep (retried
// until it succeeds once), then top the in-flight pipeline set up from the
// eligible queue and auto-resume resumable parked ones, waiting one interval
// between cycles. Cycles no longer block on the pipelines they start, so both
// exit paths drain in-flight work with o.Wait() before returning — main's
// deferred workDir-lock release must not run while a pipeline is live. Every
// stage runs under guard, so a panic is one bad cycle, not a dead daemon.
// Returns when the context is cancelled or after a single cycle when once is set.
func runLoop(ctx context.Context, o *Orchestrator, cfg *Config, once, sweep bool) {
	log.Printf("watching %s for label %q every %ds", cfg.RepoSlug, cfg.EligibleLabel, cfg.PollIntervalSec)
	for {
		if sweep {
			if err := guard("orphan sweep", func() error { return o.SweepOrphans(ctx) }); err != nil {
				log.Printf("orphan sweep failed (will retry next cycle): %v", err)
			} else {
				sweep = false
			}
		}
		if err := guard("cycle", func() error { return o.ProcessOnce(ctx) }); err != nil {
			log.Printf("cycle error: %v", err)
		}
		if err := guard("auto-resume", func() error { return o.ResumeParked(ctx) }); err != nil {
			log.Printf("auto-resume error: %v", err)
		}
		if once {
			// -once fills slots once and drains them; it does not top up as
			// pipelines complete.
			o.Wait()
			return
		}
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			// Pipelines see the cancelled context and unwind through their
			// existing context.WithoutCancel cleanup paths, exactly as they did
			// when a Ctrl-C landed during the old in-cycle wg.Wait().
			o.Wait()
			return
		case <-time.After(time.Duration(cfg.PollIntervalSec) * time.Second):
		}
	}
}
```

- [ ] **Step 4: Run the drain test**

Run: `go test ./... -run TestRunLoopOnceDrainsInFlightPipelines -race -v`
Expected: PASS.

- [ ] **Step 5: Update the `ticketsPerCycle` README row**

In `README.md`, replace line 172 with:

```markdown
| `ticketsPerCycle` | no       | `1`        | Maximum number of pipelines running concurrently. Each poll cycle tops the in-flight set back up to this limit from the eligible queue, so a newly labelled issue starts within one poll interval whenever a slot is free. Auto-resumes of parked issues draw from the same limit. Values below 1 are treated as 1 |
```

- [ ] **Step 6: Update the "How it works" section**

In `README.md`, after the numbered list that ends with the **Ship** step (around line 59), insert:

```markdown
A poll cycle does **not** wait for the pipelines it starts. It fills the free
`ticketsPerCycle` slots, returns, and polls again one interval later — so work
labelled while other pipelines are running is picked up as soon as a slot frees,
rather than at the end of a batch. `-once` fills the slots one time, waits for
them to drain, and exits.
```

- [ ] **Step 7: Run the whole suite and build**

Run: `go build ./... && go vet ./... && go test ./... -race`
Expected: PASS, no vet findings.

- [ ] **Step 8: Commit**

```bash
git add main.go main_test.go README.md
git commit -m "feat: drain in-flight pipelines before runLoop returns"
```

---

## Verification checklist (run after Task 6)

- [ ] `go build ./...` succeeds.
- [ ] `go vet ./...` is clean.
- [ ] `go test ./... -race` passes with no skipped or flaky tests. Run it three times (`go test ./... -race -count=3`) — the concurrency tests are the ones most likely to be timing-sensitive.
- [ ] `grep -n "wg.Wait" loop.go` returns nothing: the per-cycle WaitGroup is gone.
- [ ] `grep -n "o.cfg.TicketsPerCycle" loop.go slots.go` shows the budget read only in `slots()`.
