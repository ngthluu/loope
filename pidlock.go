package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
)

// pidLock is a claim on a path, held for as long as the claiming process lives.
// Both of loope's claims are one of these: the workDir lock ("a daemon owns this
// directory") and the per-issue run-owner file ("a process is driving this
// issue's pipeline").
//
// The claim is the kernel's flock, not the pid written in the file. That
// distinction is the whole point. A pid is not a process identity: the OS is
// free to hand a crashed daemon's pid to something unrelated, and a liveness
// check by pid then reads that stranger as the owner — forever, since nothing
// can ever prove the file stale again. Such a file used to wedge an issue
// permanently: register refused it, the orphan sweep skipped it for the same
// reason and never reached its own cleanup, and Stop left the labeling to a
// process that did not exist. An flock cannot be confused this way. It is
// attached to the open file description, so the kernel drops it when the holder
// exits — cleanly, on SIGKILL, or in a panic — and a crashed run's file is
// self-evidently free.
//
// The pid inside the file is therefore advisory: it names the holder in error
// messages and lets a reader tell OUR claim from someone else's.
type pidLock struct {
	f *os.File
}

// pidLockGuard serialises this process's claims and probes. flock conflicts are
// between open file descriptions, not processes, so two goroutines here race
// each other exactly as two processes would — and a probe that lost such a race
// used to report a phantom holder. Held only across an acquisition, never for
// the life of a claim.
var pidLockGuard sync.Mutex

// lockHeldError reports that a live process already holds the path. It is the
// only failure worth refusing over: every other error (an unwritable directory,
// a filesystem that cannot flock) leaves the caller to decide, and both callers
// choose to proceed rather than let an advisory file stop real work.
type lockHeldError struct{ pid int }

func (e lockHeldError) Error() string { return fmt.Sprintf("held by pid %d", e.pid) }

// claimPIDLock takes the claim on path, creating the file if needed and writing
// our pid into it. It returns lockHeldError when a live process holds it.
//
// Creating with O_CREATE rather than O_EXCL is deliberate: the flock, not the
// file's existence, is the claim, so there is no read-check-write window and no
// stale-lock takeover protocol to get right — the two things the previous
// O_EXCL implementations each had their own copy of.
func claimPIDLock(path string) (*pidLock, error) {
	pidLockGuard.Lock()
	defer pidLockGuard.Unlock()
	// A departing holder unlinks the file while still holding the lock, so a
	// claimant that opened it a moment earlier can win an flock on an inode that
	// is no longer at the path — a claim nobody else could see. Detect that by
	// comparing what we hold against what is at the path, and retry: the loser
	// simply creates the file afresh. Two rounds is enough for any real race;
	// bounding it means a pathological one fails loudly instead of spinning.
	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			if err == syscall.EWOULDBLOCK {
				pid, _ := readPIDFile(path)
				return nil, lockHeldError{pid: pid}
			}
			return nil, err
		}
		if !sameFileAtPath(f, path) {
			f.Close()
			continue
		}
		if err := writePID(f); err != nil {
			f.Close()
			return nil, err
		}
		return &pidLock{f: f}, nil
	}
	return nil, fmt.Errorf("claim %s: the file kept being replaced under us", path)
}

// release gives up the claim. The file is unlinked BEFORE the lock is dropped,
// so a claimant holding an fd on this inode fails its sameFileAtPath check and
// retries instead of winning a claim on a file that has left the directory.
// Safe to call twice: a deferred release plus an explicit one is a normal shape.
func (l *pidLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = os.Remove(l.f.Name())
	_ = l.f.Close()
	l.f = nil
}

// pidLockHeld reports whether a live process holds the claim on path, and which
// pid it published. A missing file, or one whose owner is gone, reads as free.
//
// The probe is SHARED, and that is load-bearing. A shared lock still conflicts
// with the exclusive one a real holder took, which is the question being asked —
// but it does not conflict with another probe, so two readers cannot invent a
// holder for each other. An exclusive probe did exactly that: whoever lost the
// race read the pid lying in the file (a crashed run's, since nothing deletes
// those) and reported it as a live owner. Stop then left the labeling to a
// process that did not exist, settleStopped left a marker on a stopped ticket,
// and register refused a claim nobody held.
//
// Probing at all is still an acquisition, so it is serialised with this
// process's claims (see pidLockGuard) and can still, very rarely, make a
// claimant in ANOTHER process see a phantom holder. Every caller of a refused
// claim treats it as "someone else is running this", which is recoverable —
// the cycle retries — where a phantom read as a real owner was not.
func pidLockHeld(path string) (pid int, held bool) {
	pidLockGuard.Lock()
	defer pidLockGuard.Unlock()
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK {
			pid, _ = readPIDFile(path)
			return pid, true
		}
		// The lock cannot be probed (a filesystem without flock, say). Nothing
		// can be proven, so claim nothing: reporting "held" would wedge every
		// issue, which is the failure mode this file exists to prevent.
		return 0, false
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return 0, false
}

// sameFileAtPath reports whether the open file is still the one the path names.
func sameFileAtPath(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	at, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(held, at)
}

// writePID replaces the file's contents with our pid. It truncates first, so
// taking over a file that held a longer pid (or garbage) leaves no tail behind.
func writePID(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	_, err := f.WriteString(strconv.Itoa(os.Getpid()))
	return err
}
