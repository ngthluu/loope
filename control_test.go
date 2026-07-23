package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunRegistryRegisterCancelDeregister(t *testing.T) {
	var reg runRegistry
	_, cancel := context.WithCancel(context.Background())
	cancelled := false
	wrapped := func() { cancelled = true; cancel() }

	if !reg.register(7, wrapped, nil) {
		t.Fatal("first register should succeed")
	}
	if !reg.running(7) {
		t.Fatal("registered issue should report running")
	}
	if reg.register(7, wrapped, nil) {
		t.Fatal("second register of the same issue must be refused")
	}
	if !reg.cancel(7, nil) {
		t.Fatal("cancel of a registered issue should report found")
	}
	if !cancelled {
		t.Fatal("cancel must invoke the registered cancel func")
	}
	reg.deregister(7)
	if reg.running(7) {
		t.Fatal("deregistered issue must not report running")
	}
	if reg.cancel(7, nil) {
		t.Fatal("cancel of an unregistered issue should report not found")
	}
}

// The claim hook runs for the winner only, and cancel's hook only on a hit.
// That pairing is what orders a fresh run's stale-marker clear against a Stop
// arriving in the same instant: both happen under the registry lock, so a stop
// that finds the run registered knows the clear is already behind it.
func TestRunRegistryHooksRunOnceAndOnlyOnAHit(t *testing.T) {
	var reg runRegistry
	claims := 0
	if !reg.register(7, func() {}, func() { claims++ }) {
		t.Fatal("first register should succeed")
	}
	if reg.register(7, func() {}, func() { claims++ }) {
		t.Fatal("second register of the same issue must be refused")
	}
	if claims != 1 {
		t.Fatalf("onClaim ran %d times, want exactly one (the winner's)", claims)
	}

	hits := 0
	if !reg.cancel(7, func() { hits++ }) || hits != 1 {
		t.Fatalf("cancel of a registered issue must run its hook once, got %d", hits)
	}
	reg.deregister(7)
	if reg.cancel(7, func() { hits++ }) || hits != 1 {
		t.Fatalf("cancel of an unregistered issue must not run the hook, got %d", hits)
	}
}

func TestRunRegistryNumbers(t *testing.T) {
	var reg runRegistry
	reg.register(3, func() {}, nil)
	reg.register(9, func() {}, nil)
	got := reg.numbers()
	if len(got) != 2 {
		t.Fatalf("numbers() = %v, want two entries", got)
	}
	seen := map[int]bool{}
	for _, n := range got {
		seen[n] = true
	}
	if !seen[3] || !seen[9] {
		t.Fatalf("numbers() = %v, want 3 and 9", got)
	}
}

// stopEnv wires a fakeEnv orchestrator whose gh `issue view --json labels`
// returns the labels the test wants, so Stop can read the current state.
func stopEnv(t *testing.T, labels ...string) (*fakeEnv, *Orchestrator) {
	t.Helper()
	env := newFakeEnv(t)
	base := env.f.handler
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, `{"name":"`+l+`"}`)
	}
	env.f.handler = func(c rcall) (string, string, error) {
		joined := strings.Join(c.args, " ")
		if c.name == "gh" && strings.HasPrefix(joined, "issue view") && strings.Contains(joined, "labels") {
			return `{"labels":[` + strings.Join(quoted, ",") + `]}`, "", nil
		}
		return base(c)
	}
	return env, env.orchestrator()
}

func TestStopRegisteredRunCancelsAndLeavesLabelingToThePipeline(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	cancelled := make(chan struct{})
	o.registry.register(7, func() { close(cancelled) }, nil)

	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("Stop must cancel a locally registered run")
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("Stop must write the marker first")
	}
	if len(env.callsMatching("gh", "--add-label ai-stopped")) != 0 {
		t.Fatal("the pipeline labels as it unwinds; Stop must not label a registered run")
	}
}

func TestStopQueuedTicketAddsStoppedLabel(t *testing.T) {
	env, o := stopEnv(t, "ai-agent")
	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "--add-label ai-stopped")) == 0 {
		t.Fatal("a queued ticket with no state label should get ai-stopped added")
	}
	if len(env.callsMatching("git", "worktree")) != 0 {
		t.Fatal("stopping a queued ticket must not touch any worktree")
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("marker missing")
	}
}

func TestStopParkedTicketSwapsAndClearsParkCause(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-rework")
	recordParkCause(o.issueLogDir(7), "usage limit")

	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	swaps := env.callsMatching("gh", "--remove-label ai-rework")
	if len(swaps) == 0 || !strings.Contains(swaps[0], "--add-label ai-stopped") {
		t.Fatalf("want a rework->stopped swap, got %v", swaps)
	}
	if readParkCause(o.issueLogDir(7)) != "" {
		t.Fatal("a stopped ticket must carry no park cause")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-stopped")
	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatalf("stopping a stopped ticket must be a no-op success, got %v", err)
	}
}

func TestStopDoneTicketErrors(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-done")
	if err := o.Stop(context.Background(), 7); err == nil {
		t.Fatal("stopping a done ticket must error: there is nothing to stop")
	}
}

func TestFinishStoppedPreservesEverything(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	recordStopRequest(o.issueLogDir(7))

	if err := o.finishStopped(context.Background(), 7, "ai-wip"); err != nil {
		t.Fatal(err)
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("finishStopped must LEAVE the marker: it is the durable record of the hold")
	}
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Fatal("finishStopped must not remove the worktree")
	}
	if len(env.callsMatching("git", "branch -D")) != 0 {
		t.Fatal("finishStopped must not delete the branch")
	}
	comments := env.callsMatching("gh", "issue comment")
	if len(comments) == 0 || !strings.Contains(strings.Join(comments, "\n"), "Stopped by request") {
		t.Fatalf("want a stop comment, got %v", comments)
	}
}

func TestWatchStopsCancelsWhenMarkerAppearsOutOfBand(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-wip")
	cancelled := make(chan struct{})
	o.registry.register(7, func() { close(cancelled) }, nil)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go o.watchStops(ctx, time.Millisecond)

	// Simulate `loope -stop 7` in a second process: it can only write the file.
	recordStopRequest(o.issueLogDir(7))

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("watchStops should cancel a registered run once its marker appears")
	}
}

func TestWatchStopsIgnoresUnmarkedRuns(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-wip")
	cancelled := make(chan struct{})
	o.registry.register(7, func() { close(cancelled) }, nil)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go o.watchStops(ctx, time.Millisecond)

	select {
	case <-cancelled:
		t.Fatal("watchStops must not cancel a run with no stop marker")
	case <-time.After(100 * time.Millisecond):
	}
}

// seedResumable puts a worktree dir and a session file on disk for issue n, so
// continue takes the real-resume path.
func seedResumable(t *testing.T, o *Orchestrator, n int, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(worktreePath(o.cfg.WorkDir, n), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Claude{logDir: o.issueLogDir(n)}
	c.RecordSession(sessionID, "bug")
}

func TestContinueResumesPersistedSessionAndShips(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-stopped")
	seedResumable(t, o, 7, "sess-42")
	recordStopRequest(o.issueLogDir(7))

	if err := o.Continue(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if stopRequested(o.issueLogDir(7)) {
		t.Fatal("continue must clear the stop marker")
	}
	swaps := env.callsMatching("gh", "--remove-label ai-stopped")
	if len(swaps) == 0 || !strings.Contains(swaps[0], "--add-label ai-wip") {
		t.Fatalf("want a stopped->wip swap, got %v", swaps)
	}
	resumed := env.callsMatching("claude", "--resume sess-42")
	if len(resumed) == 0 {
		t.Fatal("continue must resume the persisted session id")
	}
	if len(env.callsMatching("gh", "--add-label ai-done")) == 0 {
		t.Fatal("a successful continue ships: wip -> done")
	}
}

func TestContinueWithoutWorktreeRequeues(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-stopped")
	recordStopRequest(o.issueLogDir(7))
	recordState(o.issueLogDir(7), "ai-stopped")

	if err := o.Continue(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	removals := env.callsMatching("gh", "--remove-label ai-stopped")
	if len(removals) == 0 {
		t.Fatal("with nothing to resume, continue re-queues by removing ai-stopped")
	}
	if _, err := os.Stat(filepath.Join(o.issueLogDir(7), stateFile)); err == nil {
		t.Fatal("re-queueing must clear the local state marker")
	}
	if len(env.callsMatching("claude", "--resume")) != 0 {
		t.Fatal("there is nothing to resume, so no claude call may be made")
	}
	if stopRequested(o.issueLogDir(7)) {
		t.Fatal("continue must clear the stop marker")
	}
}

func TestContinueRefusesRunningIssue(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-stopped")
	seedResumable(t, o, 7, "sess-42")
	o.registry.register(7, func() {}, nil)

	err := o.Continue(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "#7 is already running") {
		t.Fatalf("want '#7 is already running', got %v", err)
	}
}

func TestContinueRefusesNonStoppedIssue(t *testing.T) {
	_, o := stopEnv(t, "ai-agent", "ai-rework")
	err := o.Continue(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "#7 is not stopped") {
		t.Fatalf("want '#7 is not stopped', got %v", err)
	}
}

// failGit makes every git call whose args contain substr fail, so a test can
// stand in for the tooling failure a cancelled context produces. before, when
// non-nil, runs just before the failure — the seam a test uses to land a stop
// mid-call, exactly as an out-of-band `loope -stop` does.
func failGit(env *fakeEnv, substr string, cause error, before func()) {
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "git" && strings.Contains(strings.Join(c.args, " "), substr) {
			if before != nil {
				before()
			}
			return "", "", cause
		}
		return base(c)
	}
}

// A stop cancels the pipeline's context, so whatever git/gh call is in flight
// during setup fails like any tooling error. Routing that into abort would
// delete the worktree and branch and re-queue the ticket — the exact opposite of
// the promise a stop makes.
func TestStopDuringSetupPreservesProgressInsteadOfAborting(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	// The stop lands while the worktree is being created: the marker appears and
	// the cancelled context fails the call in flight.
	failGit(env, "worktree add", context.Canceled, func() { recordStopRequest(o.issueLogDir(7)) })

	if err := o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main"); err != nil {
		t.Fatalf("a stop is a clean outcome, got %v", err)
	}
	swaps := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swaps) == 0 || !strings.Contains(swaps[0], "--add-label ai-stopped") {
		t.Fatalf("a stopped setup must swap wip->stopped, got %v", swaps)
	}
	if len(env.callsMatching("git", "branch -D")) != 0 {
		t.Fatal("a stop must never delete the branch")
	}
	if len(env.callsMatching("git", "worktree remove")) != 0 {
		t.Fatal("a stop must never remove the worktree")
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("the marker is the durable record of the hold and must survive")
	}
}

// Without a stop pending, the same setup failure still aborts: recovery belongs
// in the stop branch only.
func TestSetupFailureWithoutAStopStillAborts(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	failGit(env, "worktree add", errors.New("fatal: bad object"), nil)

	if err := o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main"); err == nil {
		t.Fatal("want the tooling error")
	}
	removals := env.callsMatching("gh", "--remove-label ai-wip")
	if len(removals) == 0 {
		t.Fatal("abort must strip ai-wip so the issue is retried next cycle")
	}
	if len(env.callsMatching("gh", "--add-label ai-stopped")) != 0 {
		t.Fatal("nothing was stopped; the issue must not be labelled stopped")
	}
}

// A stop landing after the pipeline succeeded cancels the context inside ship,
// so every git/gh call there fails. Parking that would strand the issue in
// rework WITH a stop marker — a state auto-resume refuses ("stopped by request")
// and continue refuses ("not stopped"), so nothing could ever move it again.
func TestStopDuringShipFinishesStoppedRatherThanParked(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	recordStopRequest(o.issueLogDir(7))

	if err := o.ship(context.Background(), Issue{Number: 7, Title: "Fix crash"},
		worktreePath(o.cfg.WorkDir, 7), branchName(7), "main", "bug", "ai-wip"); err != nil {
		t.Fatalf("a stop is a clean outcome, got %v", err)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) != 0 {
		t.Fatal("a stopped ticket must not be parked for rework")
	}
	if readParkCause(o.issueLogDir(7)) != "" {
		t.Fatal("a stopped ticket must carry no park cause, or auto-resume would fight the hold")
	}
	if len(env.callsMatching("git", "push")) != 0 || len(env.callsMatching("gh", "pr create")) != 0 {
		t.Fatal("a stop that lands before the push must not ship the ticket anyway")
	}
}

// park is the backstop for every path that does not check the marker itself —
// a mid-ship failure, a pipeline panic — so the invariant holds everywhere: an
// operator hold outranks a park.
func TestParkHonoursAPendingStop(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	recordStopRequest(o.issueLogDir(7))

	if err := o.park(context.Background(), 7, "ai-wip", errors.New("push: connection reset")); err != nil {
		t.Fatalf("a stop is a clean outcome, got %v", err)
	}
	if len(env.callsMatching("gh", "--add-label ai-rework")) != 0 {
		t.Fatal("park must defer to the pending stop")
	}
	if readParkCause(o.issueLogDir(7)) != "" {
		t.Fatal("a stopped ticket must carry no park cause")
	}
}

// Two processes race to finish the same stop: the one that ran -stop and the one
// that owns the run. The loser must be a no-op, not a duplicate comment and a
// swap of a label that has already moved.
func TestFinishStoppedIsANoOpWhenAlreadyStopped(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-stopped")

	if err := o.finishStopped(context.Background(), 7, "ai-wip"); err != nil {
		t.Fatalf("finishing an already-stopped issue must succeed, got %v", err)
	}
	if len(env.callsMatching("gh", "issue comment")) != 0 {
		t.Fatal("the second finisher must not post a duplicate stop comment")
	}
	if len(env.callsMatching("gh", "--remove-label ai-wip")) != 0 {
		t.Fatal("the second finisher must not swap a label that has already moved")
	}
	if env.readLocalState(7) != "ai-stopped" {
		t.Fatalf("local state = %q, want ai-stopped", env.readLocalState(7))
	}
}

// A marker that outlives its run — the ticket was stopped, then re-queued by a
// human removing the label rather than by continue — would make the next run
// finish as stopped the instant it reached ship or park.
func TestStaleMarkerDoesNotPoisonAFreshRun(t *testing.T) {
	env := newFakeEnv(t)
	o := env.orchestrator()
	recordStopRequest(o.issueLogDir(7))

	if err := o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main"); err != nil {
		t.Fatal(err)
	}
	if stopRequested(o.issueLogDir(7)) {
		t.Fatal("picking an eligible issue up is the decision to run it: the stale hold must be cleared")
	}
	if len(env.callsMatching("gh", "--add-label ai-done")) == 0 {
		t.Fatal("the fresh run must ship normally, not finish as stopped")
	}
}

// The terminal states clear the marker too, so a hold can never outlive the run
// it described and be read by the next one.
func TestTerminalOutcomesClearTheStopMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Orchestrator) error
	}{
		{"done", func(o *Orchestrator) error {
			return o.finishDone(context.Background(), 7, "", "", "ai-wip", "already implemented")
		}},
		{"needs-info", func(o *Orchestrator) error {
			return o.finishNeedsInfo(context.Background(), 7, "", "", "ai-wip", &lowConfidenceError{score: 20, feedback: "unclear"})
		}},
		{"abort", func(o *Orchestrator) error {
			_ = o.abort(context.Background(), 7, "", "", errors.New("boom"))
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, o := stopEnv(t, "ai-agent", "ai-wip")
			recordStopRequest(o.issueLogDir(7))
			if err := tc.run(o); err != nil {
				t.Fatal(err)
			}
			if stopRequested(o.issueLogDir(7)) {
				t.Fatalf("%s is terminal: the stop marker must not survive it", tc.name)
			}
		})
	}
}
