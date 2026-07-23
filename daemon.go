package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// lockPath is the daemon liveness lock, under logs/ next to the artifacts it
// guards.
func lockPath(workDir string) string {
	return filepath.Join(workDir, "logs", "daemon.lock")
}

// acquireLock claims workDir for this process by taking the pidLock on
// logs/daemon.lock. If a live process holds it, another loop instance owns the
// workDir and an error is returned. A lock left behind by a dead instance is
// free, whatever pid it names. Holding the lock is what makes the startup orphan
// sweep safe: any ai-wip issue found can only belong to a dead run. The returned
// release drops the lock; call it on clean shutdown.
//
// A lock that cannot be taken for any other reason is fatal, unlike the
// per-issue claim, which is advisory and grants itself on doubt. Nothing here
// can be proven without it: an unlocked daemon may race a second daemon for live
// work, and the startup orphan sweep would reclaim worktrees out from under it.
// A filesystem that cannot flock (some network mounts) therefore stops the
// daemon rather than being worked around, and says so.
func acquireLock(workDir string) (release func(), err error) {
	path := lockPath(workDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	l, err := claimPIDLock(path)
	if err != nil {
		var held lockHeldError
		if errors.As(err, &held) {
			return nil, fmt.Errorf("another loop instance (pid %d) owns %s — stop it first or use a different workDir", held.pid, workDir)
		}
		return nil, fmt.Errorf("could not lock %s (%w) — workDir must be on a filesystem that supports flock(2)", workDir, err)
	}
	return l.release, nil
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

// claimRunOwner claims issue ownership for this process, returning the held
// claim and whether it was won. It is the cross-process half of
// runRegistry.register, and is the same pidLock as acquireLock — one
// implementation of the protocol, so a fix to it reaches both claims.
//
// The returned lock must be held for the life of the run and released with it
// (runRegistry.release does both). A file left by a crashed run holds nothing,
// so it is taken over rather than obeyed.
//
// A claim that cannot be taken for any reason OTHER than a live holder (an
// unwritable log dir, a filesystem without flock) is granted with a nil lock:
// the file is an advisory record, and refusing every run because of it would
// trade a rare double-run for a daemon that ships nothing.
func claimRunOwner(logDir string) (*pidLock, bool) {
	if logDir == "" {
		return nil, true
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, true
	}
	l, err := claimPIDLock(runOwnerPath(logDir))
	if err != nil {
		var held lockHeldError
		if errors.As(err, &held) {
			return nil, false
		}
		return nil, true
	}
	return l, true
}

// runOwnerAlive reports whether a live process is driving this issue's pipeline,
// and whether that process is this one. A file whose holder is gone — however it
// went — means no run.
func runOwnerAlive(logDir string) (alive, isSelf bool) {
	if logDir == "" {
		return false, false
	}
	pid, held := pidLockHeld(runOwnerPath(logDir))
	if !held {
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
