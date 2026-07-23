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

	if !reg.register(7, "", wrapped) {
		t.Fatal("first register should succeed")
	}
	if !reg.running(7) {
		t.Fatal("registered issue should report running")
	}
	if reg.register(7, "", wrapped) {
		t.Fatal("second register of the same issue must be refused")
	}
	if !reg.cancel(7) {
		t.Fatal("cancel of a registered issue should report found")
	}
	if !cancelled {
		t.Fatal("cancel must invoke the registered cancel func")
	}
	reg.deregister(7, "")
	if reg.running(7) {
		t.Fatal("deregistered issue must not report running")
	}
	if reg.cancel(7) {
		t.Fatal("cancel of an unregistered issue should report not found")
	}
}

// The claim is recorded on disk as well as in memory: only the file can tell a
// `loope -stop` in another process that this issue has a live run behind it,
// and only its removal can tell that process the run is over.
func TestRunRegistryPublishesTheClaimOnDisk(t *testing.T) {
	var reg runRegistry
	logDir := t.TempDir()

	if !reg.register(7, logDir, func() {}) {
		t.Fatal("first register should succeed")
	}
	alive, isSelf := runOwnerAlive(logDir)
	if !alive || !isSelf {
		t.Fatalf("a registered run must publish this process as the owner, got alive=%v self=%v", alive, isSelf)
	}
	if otherProcessRunning(logDir) {
		t.Fatal("our own run must never read as another process's")
	}

	// The loser of a claim must not touch the winner's file.
	reg.register(7, filepath.Join(logDir, "nope"), func() {})
	if _, err := os.Stat(filepath.Join(logDir, "nope")); err == nil {
		t.Fatal("a refused register must not record ownership")
	}

	reg.deregister(7, logDir)
	if alive, _ := runOwnerAlive(logDir); alive {
		t.Fatal("deregistering must retract the on-disk claim")
	}
}

// A dead owner is not an owner: a crashed run leaves its file behind, and Stop
// must do the labeling itself rather than wait forever for a process that is
// gone. This is the hole the workDir lock could not see — it reports "a daemon
// is up", not "this issue has a run behind it".
func TestRunOwnerOfADeadProcessReadsAsNotRunning(t *testing.T) {
	logDir := t.TempDir()
	// pid 1 is alive but is not us; a pid that cannot exist stands in for a
	// crashed owner.
	if err := os.WriteFile(runOwnerPath(logDir), []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}
	if alive, _ := runOwnerAlive(logDir); alive {
		t.Fatal("a dead pid must read as no live run")
	}
	if otherProcessRunning(logDir) {
		t.Fatal("a dead pid must not be treated as another process's live run")
	}
	if err := os.WriteFile(runOwnerPath(logDir), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if alive, _ := runOwnerAlive(logDir); alive {
		t.Fatal("an unparseable owner file must read as no live run")
	}
}

func TestRunRegistryNumbers(t *testing.T) {
	var reg runRegistry
	reg.register(3, "", func() {})
	reg.register(9, "", func() {})
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
	o.registry.register(7, o.issueLogDir(7), func() { close(cancelled) })

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
	if stopRequested(o.issueLogDir(7)) {
		t.Fatal("the stop landed, so its marker is spent: the ai-stopped label is the durable record")
	}
	if env.readLocalState(7) != "ai-stopped" {
		t.Fatalf("local state = %q, want ai-stopped", env.readLocalState(7))
	}
}

// The marker means "a stop is pending", so it must outlive a stop that could
// not be completed — otherwise the issue is left labelled wip with nothing
// anywhere recording that an operator asked for it to halt, and the orphan
// sweep would recover it for auto-resume instead of as stopped.
func TestAFailedStopKeepsTheRequestPending(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "gh" && strings.Contains(strings.Join(c.args, " "), "--add-label ai-stopped") {
			return "", "", errors.New("gh: 503")
		}
		return base(c)
	}

	if err := o.Stop(context.Background(), 7); err == nil {
		t.Fatal("a stop whose labeling failed must report the failure")
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("an incomplete stop must stay pending on disk")
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
	if stopRequested(o.issueLogDir(7)) {
		t.Fatal("a stop that landed retires its marker; the label is what makes it durable")
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
	o.registry.register(7, o.issueLogDir(7), func() { close(cancelled) })

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
	o.registry.register(7, o.issueLogDir(7), func() { close(cancelled) })

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
	o.registry.register(7, o.issueLogDir(7), func() {})

	err := o.Continue(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "#7 is already running") {
		t.Fatalf("want '#7 is already running', got %v", err)
	}
}

// A resume is a multi-minute Claude session, so a hold has to be honoured
// before it starts, not after — and `loope -rework <N>` skipped every gate a
// continue goes through, so it drove stopped tickets. Only continue lifts a
// hold.
func TestReworkRefusesAStoppedIssue(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-stopped")
	seedResumable(t, o, 7, "sess-42")

	err := o.Rework(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("want a refusal naming the hold, got %v", err)
	}
	if len(env.callsMatching("claude", "--resume")) != 0 {
		t.Fatal("no session may be spent on a ticket under an operator hold")
	}
	if len(env.callsMatching("gh", "--remove-label ai-rework")) != 0 {
		t.Fatal("a stopped issue carries no ai-rework label to swap away from")
	}
}

// The same for a stop that lands between the state check and the session: the
// resume is claimed first, then the marker checked, so the hold is caught before
// the Claude call rather than by the park that would have followed it.
func TestResumeHonoursAStopThatLandsBeforeTheSession(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-rework")
	seedResumable(t, o, 7, "sess-42")
	recordStopRequest(o.issueLogDir(7))

	if err := o.resume(context.Background(), 7, "ai-rework"); err != nil {
		t.Fatalf("a stop is a clean outcome, got %v", err)
	}
	if len(env.callsMatching("claude", "--resume")) != 0 {
		t.Fatal("no session may be spent on a ticket under an operator hold")
	}
	swaps := env.callsMatching("gh", "--remove-label ai-rework")
	if len(swaps) == 0 || !strings.Contains(swaps[0], "--add-label ai-stopped") {
		t.Fatalf("want a rework->stopped swap, got %v", swaps)
	}
}

// Lifting a hold is only real once the issue is off ai-stopped. Clearing the
// marker first would, on a failed transition, leave the issue labelled stopped
// with nothing pending anywhere — and Stop short-circuits on an already-stopped
// issue, so no later stop would re-create the marker either.
func TestContinueKeepsTheHoldWhenTheTransitionFails(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-stopped")
	seedResumable(t, o, 7, "sess-42")
	recordStopRequest(o.issueLogDir(7))
	base := env.f.handler
	env.f.handler = func(c rcall) (string, string, error) {
		if c.name == "gh" && strings.Contains(strings.Join(c.args, " "), "--add-label ai-wip") {
			return "", "", errors.New("gh: 503")
		}
		return base(c)
	}

	if err := o.Continue(context.Background(), 7); err == nil {
		t.Fatal("a continue whose label swap failed must report the failure")
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("the hold is still in force: the issue is still labelled ai-stopped")
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
	if env.readLocalState(7) != "ai-stopped" {
		t.Fatalf("local state = %q, want ai-stopped", env.readLocalState(7))
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

// The durable record of a completed stop is the ai-stopped LABEL. A human
// removing it re-queues the ticket, and because the stop retired its own marker
// when it landed, the fresh run finds nothing left over from the issue's earlier
// life — no claim-time guess at whether a hold is "stale" is needed, or
// possible, since no process can tell a leftover from one being written right
// now.
func TestAStoppedThenRequeuedIssueRunsCleanly(t *testing.T) {
	env, o := stopEnv(t, "ai-agent")
	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	// The human removes ai-stopped, so the issue is eligible again — which is
	// what stopEnv's stub (no state label) already reports.
	if err := o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main"); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "--add-label ai-done")) == 0 {
		t.Fatal("the fresh run must ship normally, not finish as stopped")
	}
}

// A marker present when a run claims the issue is a stop that has NOT completed
// — the only kind there is now. Honouring it here is what keeps `loope -stop`
// from being outrun by a pickup, and it costs no Claude session.
func TestAPendingStopIsHonouredBeforeTheRunStarts(t *testing.T) {
	env, o := stopEnv(t, "ai-agent")
	recordStopRequest(o.issueLogDir(7))

	if err := o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main"); err != nil {
		t.Fatalf("a stop is a clean outcome, got %v", err)
	}
	if len(env.callsMatching("claude", "")) != 0 {
		t.Fatal("no Claude session may be spent on a ticket that is already held")
	}
	if len(env.callsMatching("gh", "--add-label ai-wip")) != 0 {
		t.Fatal("a held ticket must not be marked wip")
	}
	if len(env.callsMatching("gh", "--add-label ai-stopped")) == 0 {
		t.Fatal("the pending stop must be completed, not just skipped")
	}
}

// The other half of the handshake: a stop that completed entirely between the
// eligible listing and this pickup leaves no marker to find, only the label. The
// claim re-reads it rather than starting a run on a ticket the operator holds.
func TestAStopThatLandedBeforePickupPreventsTheRun(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-stopped")

	if err := o.handleIssue(context.Background(), Issue{Number: 7, Title: "Fix crash"}, "bug", "origin/main"); err != nil {
		t.Fatalf("declining to start is a clean outcome, got %v", err)
	}
	if len(env.callsMatching("gh", "--add-label ai-wip")) != 0 {
		t.Fatal("a stopped ticket must not be relabelled wip on top of the hold")
	}
	if len(env.callsMatching("claude", "")) != 0 {
		t.Fatal("no Claude session may be spent on a stopped ticket")
	}
}

// Stop must route on whether THIS ISSUE has a live run, not on whether a daemon
// happens to be up. An ai-wip label with no live pipeline behind it — a crashed
// run, a `-rework` that died — was handed to a daemon that had no such run to
// halt, so the marker was never consumed and the issue stayed wip forever:
// continue refused it ("not stopped"), auto-resume refused it ("stopped by
// request"), and Stop kept reporting success.
func TestStopFinishesAWipIssueNoProcessIsActuallyRunning(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	// A daemon owns the workDir...
	release, err := acquireLock(o.cfg.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// ...but nothing is running #7: its owner is a pid that died.
	if err := os.MkdirAll(o.issueLogDir(7), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runOwnerPath(o.issueLogDir(7)), []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	swaps := env.callsMatching("gh", "--remove-label ai-wip")
	if len(swaps) == 0 || !strings.Contains(swaps[0], "--add-label ai-stopped") {
		t.Fatalf("nobody else will finish this stop, so Stop must: want a wip->stopped swap, got %v", swaps)
	}
}

// The converse: a live run in another process gets the marker and halts itself,
// so Stop must not label the issue underneath it — that would leave the ticket
// carrying both ai-wip and ai-stopped while the session ran on to a PR.
func TestStopDefersToARunInAnotherProcess(t *testing.T) {
	env, o := stopEnv(t, "ai-agent", "ai-wip")
	// pid 1 is alive and is not us: a pipeline in another process.
	if err := os.MkdirAll(o.issueLogDir(7), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runOwnerPath(o.issueLogDir(7)), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := o.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if len(env.callsMatching("gh", "--add-label ai-stopped")) != 0 {
		t.Fatal("the owning process labels as it unwinds; Stop must not label underneath it")
	}
	if !stopRequested(o.issueLogDir(7)) {
		t.Fatal("the marker is how the owning process learns of the stop and must survive")
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
