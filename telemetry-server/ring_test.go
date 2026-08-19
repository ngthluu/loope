package main

import "testing"

func TestLogRingBufferAddAndLines(t *testing.T) {
	b := NewLogRingBuffer(5)
	b.Add("a", "b")
	b.Add("c")
	got := b.Lines()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lines() = %v, want %v", got, want)
		}
	}
}

func TestLogRingBufferDropsOldestPastCapacity(t *testing.T) {
	b := NewLogRingBuffer(3)
	b.Add("1", "2", "3", "4", "5")
	got := b.Lines()
	want := []string{"3", "4", "5"}
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Lines() = %v, want %v", got, want)
		}
	}
}

func TestLogRingBufferLinesReturnsIndependentCopy(t *testing.T) {
	b := NewLogRingBuffer(5)
	b.Add("a")
	got := b.Lines()
	got[0] = "mutated"
	if b.Lines()[0] != "a" {
		t.Fatalf("mutating the returned slice must not affect the buffer")
	}
}
