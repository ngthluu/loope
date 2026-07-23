package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockPath is the daemon liveness lock, under logs/ next to the artifacts it
// guards.
func lockPath(workDir string) string {
	return filepath.Join(workDir, "logs", "daemon.lock")
}

// acquireLock claims workDir for this process by writing our pid to
// logs/daemon.lock. If the file names a live process, another loop instance
// owns the workDir and an error is returned. A stale lock (dead pid, garbage
// content) is taken over. Holding the lock is what makes the startup orphan
// sweep safe: any ai-wip issue found can only belong to a dead run. The
// returned release removes the lock; call it on clean shutdown.
func acquireLock(workDir string) (release func(), err error) {
	path := lockPath(workDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// O_EXCL makes the claim atomic: at most one of two racing processes can
	// create the file, so there is no read-check-write window for both to
	// observe "no live owner" and both win.
	if err := tryClaimLock(path); err == nil {
		return func() { _ = os.Remove(path) }, nil
	} else if !os.IsExist(err) {
		return nil, err
	}

	// File already exists: decide whether it's a live owner or a stale lock.
	if b, rerr := os.ReadFile(path); rerr == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid > 0 && pidAlive(pid) {
			return nil, fmt.Errorf("another loop instance (pid %d) owns %s — stop it first or use a different workDir", pid, workDir)
		}
	}

	// Stale (dead pid or garbage content): remove and retry the atomic claim
	// exactly once. If a racing process wins that retry, refuse rather than
	// loop — it now legitimately owns the lock.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := tryClaimLock(path); err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("lock %s was claimed by another process during takeover — retry", path)
		}
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

// tryClaimLock atomically creates the lock file and writes our pid, failing
// with an IsExist error if the file already exists.
func tryClaimLock(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strconv.Itoa(os.Getpid()))
	return err
}

// pidAlive reports whether a process with the given pid exists. Signal 0 checks
// existence without delivering anything; EPERM means it exists but isn't ours.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// lockOwnerAlive reports whether a live process currently holds workDir's daemon
// lock. Stop uses it to tell "a daemon is running this issue and will react to
// the marker" from "nothing is running, so I must do the labeling myself".
func lockOwnerAlive(workDir string) bool {
	b, err := os.ReadFile(lockPath(workDir))
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	return pidAlive(pid)
}
