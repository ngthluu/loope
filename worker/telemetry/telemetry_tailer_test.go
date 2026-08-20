package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogTailerMissingFileReturnsNoLines(t *testing.T) {
	tl := NewLogTailer(filepath.Join(t.TempDir(), "nope.log"))
	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("lines = %v, want none", lines)
	}
}

func TestLogTailerReadsNewLinesSinceLastCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("old1\nold2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := NewLogTailer(path) // starts at current EOF: does not resend history

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new1\nnew2\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new1", "new2"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines = %v, want %v", lines, want)
		}
	}
}

func TestLogTailerLeavesPartialLineForNextCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := &LogTailer{path: path} // starts at offset 0

	if err := os.WriteFile(path, []byte("complete\npartial-no-newline-yet"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "complete" {
		t.Fatalf("lines = %v, want [complete] (the partial line must wait)", lines)
	}

	if err := os.WriteFile(path, []byte("complete\npartial-no-newline-yet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err = tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "partial-no-newline-yet" {
		t.Fatalf("lines = %v, want [partial-no-newline-yet] once its newline arrives", lines)
	}
}

func TestLogTailerCapsAtMaxLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	content := strings.Repeat("line\n", 10)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := &LogTailer{path: path}

	lines, err := tl.Next(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	rest, err := tl.Next(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 7 {
		t.Fatalf("rest = %d, want the remaining 7 lines", len(rest))
	}
}

func TestLogTailerHandlesRotationBySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("old\n", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := &LogTailer{path: path}
	if _, err := tl.Next(10); err != nil { // advance offset partway into the "old" file
		t.Fatal(err)
	}

	// Simulate RotatingFile.rotate(): the same path now holds a much
	// smaller, freshly-started file.
	if err := os.WriteFile(path, []byte("rotated1\nrotated2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rotated1", "rotated2"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v (tailer must restart from 0 after rotation)", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines = %v, want %v", lines, want)
		}
	}
}

func TestLogTailerDrainsRotatedFileBeforeNewOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := NewLogTailer(path) // offset at EOF of the old file
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("d\ne\n") // unread when rotation happens
	f.Close()

	// RotatingFile.rotate: rename to .1, fresh file at path. The new file is
	// already longer than the old offset, so size alone would not reveal it.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("new\n", 10)), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, err := tl.Next(500)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{"d", "e"}, strings.Split(strings.TrimSuffix(strings.Repeat("new\n", 10), "\n"), "\n")...)
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
	// Subsequent call continues the new file, without re-draining .1.
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("tail\n")
	f.Close()
	lines, _ = tl.Next(500)
	if len(lines) != 1 || lines[0] != "tail" {
		t.Fatalf("after rotation lines = %v, want [tail]", lines)
	}
}
