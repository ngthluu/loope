package main

import (
	"context"
	"testing"
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
