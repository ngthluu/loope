package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPIDLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim")

	l, err := claimPIDLock(path)
	if err != nil {
		t.Fatalf("claiming a free path must succeed: %v", err)
	}
	if pid, ok := readPIDFile(path); !ok || pid != os.Getpid() {
		t.Fatalf("a claim must publish our pid, got %d (ok=%v)", pid, ok)
	}
	if pid, held := pidLockHeld(path); !held || pid != os.Getpid() {
		t.Fatalf("our own claim must read as held by us, got pid=%d held=%v", pid, held)
	}

	l.release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("releasing must remove the claim file")
	}
	if _, held := pidLockHeld(path); held {
		t.Error("a released claim must not read as held")
	}
	// Releasing twice is what a defer plus an explicit release does; it must not
	// panic or resurrect anything.
	l.release()
}

func TestPIDLockRefusesAHeldPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim")
	l, err := claimPIDLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.release()

	_, err = claimPIDLock(path)
	var held lockHeldError
	if !errors.As(err, &held) {
		t.Fatalf("a second claim must be refused with the holder's pid, got %v", err)
	}
	if held.pid != os.Getpid() {
		t.Errorf("refusal names pid %d, want the holder's %d", held.pid, os.Getpid())
	}
}

// The bug this primitive exists to close: a claim file left by a crashed process
// names a pid the OS is free to hand to something else. Checking liveness by pid
// then reads an unrelated live process as the owner, and the claim can never be
// taken over again — the issue becomes permanently unclaimable, unsweepable and
// unstoppable. The kernel's own record of who holds the lock cannot be confused
// this way: it is released when the holder dies, however it dies.
func TestPIDLockTakesOverAFileWhosePidWasReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim")
	// pid 1 is alive and is not us — indistinguishable, by pid alone, from a
	// live owner.
	if err := os.WriteFile(path, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, held := pidLockHeld(path); held {
		t.Fatal("a file nobody holds must not read as held, whatever pid it names")
	}
	l, err := claimPIDLock(path)
	if err != nil {
		t.Fatalf("a file nobody holds must be claimable: %v", err)
	}
	defer l.release()
	if pid, _ := readPIDFile(path); pid != os.Getpid() {
		t.Errorf("taking over must republish our pid, got %d", pid)
	}
}

func TestPIDLockOnGarbageContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim")
	if err := os.WriteFile(path, []byte("not-a-pid\nand more"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := claimPIDLock(path)
	if err != nil {
		t.Fatalf("garbage is not an owner: %v", err)
	}
	defer l.release()
	// The claim truncates: a shorter pid must not leave a tail of the old
	// content behind for readPIDFile to choke on.
	if pid, ok := readPIDFile(path); !ok || pid != os.Getpid() {
		t.Fatalf("claim file = pid %d (ok=%v), want our pid on its own", pid, ok)
	}
}

// A claim is only worth anything if a reader in another process sees it, so the
// file must exist at the claimed path — not merely as an unlinked inode the
// holder happens to keep open, which is what an unlink racing a claim produces.
func TestPIDLockClaimIsVisibleAtThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim")
	l, err := claimPIDLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.release()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the claimed path must exist: %v", err)
	}
	held, err := l.f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi, held) {
		t.Fatal("the holder must own the file that is actually at the path")
	}
}
