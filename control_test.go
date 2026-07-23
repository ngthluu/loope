package main

import (
	"context"
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

	if !reg.register(7, wrapped) {
		t.Fatal("first register should succeed")
	}
	if !reg.running(7) {
		t.Fatal("registered issue should report running")
	}
	if reg.register(7, wrapped) {
		t.Fatal("second register of the same issue must be refused")
	}
	if !reg.cancel(7) {
		t.Fatal("cancel of a registered issue should report found")
	}
	if !cancelled {
		t.Fatal("cancel must invoke the registered cancel func")
	}
	reg.deregister(7)
	if reg.running(7) {
		t.Fatal("deregistered issue must not report running")
	}
	if reg.cancel(7) {
		t.Fatal("cancel of an unregistered issue should report not found")
	}
}

func TestRunRegistryNumbers(t *testing.T) {
	var reg runRegistry
	reg.register(3, func() {})
	reg.register(9, func() {})
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
	o.registry.register(7, func() { close(cancelled) })

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
	o.registry.register(7, func() { close(cancelled) })

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
	o.registry.register(7, func() { close(cancelled) })

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
	o.registry.register(7, func() {})

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
