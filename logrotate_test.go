package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingFileWritesUnderThresholdDoNotRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	rf, err := NewRotatingFile(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	if _, err := rf.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatal("no rotation expected under the threshold")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("content = %q", data)
	}
}

func TestRotatingFileRotatesAtThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	rf, err := NewRotatingFile(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	if _, err := rf.Write([]byte("0123456789")); err != nil { // exactly fills to 10 bytes
		t.Fatal(err)
	}
	if _, err := rf.Write([]byte("next")); err != nil { // would push past 10: must rotate first
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected a rotated backup file: %v", err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "0123456789" {
		t.Fatalf("backup content = %q", backup)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "next" {
		t.Fatalf("current content = %q, want the post-rotation write only", current)
	}
}

func TestRotatingFileReopensExistingFileWithoutLosingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	rf1, err := NewRotatingFile(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rf1.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	rf1.Close()

	rf2, err := NewRotatingFile(path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer rf2.Close()
	if _, err := rf2.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "first\nsecond\n" {
		t.Fatalf("content = %q, want appended content preserved across a reopen", data)
	}
}
