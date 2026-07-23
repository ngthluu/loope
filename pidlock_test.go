package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
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

// Probing is a read, and two readers must not invent a holder for each other.
// An exclusive probe did: whoever lost the race reported the pid lying in the
// file — a crashed run's, since nothing deletes those — as a live owner. Stop
// then left the labeling to a process that did not exist, and settleStopped left
// a marker behind on a ticket it had just stopped.
func TestConcurrentProbesDoNotInventAHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim")
	// The residue of a crashed run: a file naming a pid nobody holds.
	if err := os.WriteFile(path, []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Probes are taken through probeLock rather than pidLockHeld deliberately:
	// the process-wide guard would serialise them, so they would never reach the
	// syscall together and the lock mode — the thing under test — would not
	// matter. Two processes have no such guard between them.
	const probes = 16
	var wg sync.WaitGroup
	held := make(chan int, probes)
	for i := 0; i < probes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if pid, ok := probeLock(path); ok {
					held <- pid
					return
				}
			}
		}()
	}
	wg.Wait()
	close(held)
	if pid, ok := <-held; ok {
		t.Fatalf("a probe reported pid %d as holding a claim nobody holds", pid)
	}
}

// A probe must not turn away a claimant either: the claim is the real work, and
// a reader that blocks it hands back "#N is already running" for a claim nobody
// holds.
func TestAProbeDoesNotRefuseAClaimant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim")
	if err := os.WriteFile(path, []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var probing sync.WaitGroup
	probing.Add(1)
	go func() {
		defer probing.Done()
		for {
			select {
			case <-stop:
				return
			default:
				probeLock(path)
			}
		}
	}()

	for i := 0; i < 200; i++ {
		l, err := claimPIDLock(path)
		if err != nil {
			close(stop)
			probing.Wait()
			t.Fatalf("a reader refused a claim nobody holds: %v", err)
		}
		l.release()
	}
	close(stop)
	probing.Wait()
}

// The other half of the unlink/claim race: a claimant that wins the lock on an
// inode which has since left the directory holds a claim no other process can
// see, so it must notice and retry.
func TestSameFileAtPathRejectsAReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !sameFileAtPath(f, path) {
		t.Fatal("a file still at its path must read as the same file")
	}

	// A departing holder unlinks it; a new claimant creates a fresh one.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if sameFileAtPath(f, path) {
		t.Fatal("an unlinked inode must not read as the file at the path")
	}
	g, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if sameFileAtPath(f, path) {
		t.Fatal("a replaced file must not read as the one we hold")
	}
	if !sameFileAtPath(g, path) {
		t.Fatal("the replacement must read as the file at the path")
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
