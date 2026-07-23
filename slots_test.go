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
