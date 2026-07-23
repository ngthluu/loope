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
	if pid, ok := readPIDFile(path); ok && pidAlive(pid) {
		return nil, fmt.Errorf("another loop instance (pid %d) owns %s — stop it first or use a different workDir", pid, workDir)
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

// runOwnerFile records the pid of the process driving one issue's pipeline, in
// that issue's log dir. It is the cross-process half of runRegistry: the
// registry answers "is this issue running in THIS process", which is all a
// daemon needs for itself, but only this file can answer "is it running in ANY
// process" — the question `loope -stop <N>` in a second shell has to get right
// before deciding whether to leave the labeling to the process that owns the
// run or do it itself.
//
// It is deliberately per-issue rather than per-workDir. The workDir lock says
// only that a daemon is up — it proves no other process may claim FRESH work
// here, which is what makes the startup orphan sweep safe, and nothing at all
// about whether a given issue has a pipeline behind it right now. An issue can
// equally be driven by a process holding no lock (-once, -rework, -continue).
const runOwnerFile = "owner"

func runOwnerPath(logDir string) string { return filepath.Join(logDir, runOwnerFile) }

// claimRunOwner claims issue ownership for this process, reporting whether the
// claim was won. It is the cross-process half of runRegistry.register and works
// exactly like acquireLock: O_EXCL makes the claim atomic, so two processes
// racing for the same issue cannot both read "nobody owns this" and both win.
// A file naming a dead pid — or our own, left by a run that has already
// unwound — is stale and is taken over.
//
// A claim that cannot be written (an unwritable log dir) is granted rather than
// refused: the file is an advisory record, and refusing every run because of it
// would trade a rare double-run for a daemon that ships nothing.
func claimRunOwner(logDir string) bool {
	if logDir == "" {
		return true
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return true
	}
	path := runOwnerPath(logDir)
	if err := tryClaimLock(path); err == nil {
		return true
	} else if !os.IsExist(err) {
		return true
	}
	// The file exists: only a live process OTHER than us is a real owner.
	if otherProcessRunning(logDir) {
		return false
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return true
	}
	// Retry the atomic claim exactly once. Losing that retry means a racing
	// process created the file in between and now legitimately owns the issue —
	// the only failure worth refusing over.
	err := tryClaimLock(path)
	return err == nil || !os.IsExist(err)
}

// clearRunOwner releases this process's claim on an issue. A file left behind
// by a crash is self-healing rather than sticky: every reader checks liveness,
// so a dead pid reads as "nobody is running this".
func clearRunOwner(logDir string) {
	if logDir == "" {
		return
	}
	_ = os.Remove(runOwnerPath(logDir))
}

// runOwnerAlive reports whether a live process is driving this issue's pipeline,
// and whether that process is this one. A dead or missing pid means no run.
func runOwnerAlive(logDir string) (alive, isSelf bool) {
	if logDir == "" {
		return false, false
	}
	pid, ok := readPIDFile(runOwnerPath(logDir))
	if !ok || !pidAlive(pid) {
		return false, false
	}
	return true, pid == os.Getpid()
}

// otherProcessRunning reports whether a live process OTHER than this one is
// driving this issue's pipeline. That process has a stop watcher on the marker,
// so it — and only it — can halt the run and do the labeling as it unwinds.
//
// Ownership by this process is excluded because runRegistry already answers
// that exactly: if the run were live here it would be registered, so a file
// naming our own pid without a registry entry is a leftover, not a run.
func otherProcessRunning(logDir string) bool {
	alive, isSelf := runOwnerAlive(logDir)
	return alive && !isSelf
}

// readPIDFile reads a pid from a one-line pid file, reporting whether it held a
// plausible one. Missing files and garbage content both read as absent.
func readPIDFile(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
