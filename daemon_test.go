package main

import (
	"os"
	"strconv"
	"testing"
)

func TestAcquireLockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(lockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := strconv.Atoi(string(b)); got != os.Getpid() {
		t.Errorf("lock pid = %q, want our pid", b)
	}

	// A second acquire against a live pid must refuse.
	if _, err := acquireLock(dir); err == nil {
		t.Fatal("second acquire against a live pid must fail")
	}

	release()
	if _, err := os.Stat(lockPath(dir)); !os.IsNotExist(err) {
		t.Error("release must remove the lock file")
	}
	if release2, err := acquireLock(dir); err != nil {
		t.Errorf("acquire after release must succeed: %v", err)
	} else {
		release2()
	}
}

func TestAcquireLockTakesOverStaleLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/logs", 0o755); err != nil {
		t.Fatal(err)
	}
	// A pid far above any real pid space: kill(2) reports ESRCH.
	if err := os.WriteFile(lockPath(dir), []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	release, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("stale lock must be taken over: %v", err)
	}
	defer release()

	// Garbage content is also stale.
	release()
	if err := os.WriteFile(lockPath(dir), []byte("not-a-pid"), 0o644); err != nil {
		t.Fatal(err)
	}
	release2, err := acquireLock(dir)
	if err != nil {
		t.Fatalf("garbage lock must be taken over: %v", err)
	}
	release2()
}
