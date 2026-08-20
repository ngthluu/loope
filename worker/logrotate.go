package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rotatingFileMaxBytes is the size threshold at which the daemon log
// rotates.
const rotatingFileMaxBytes = 10 * 1024 * 1024 // 10MB

// RotatingFile is an io.Writer over a size-capped log file: once the current
// file would exceed maxBytes, it is renamed to a ".1" sibling (overwriting
// any previous one) and a fresh file is opened — so the daemon log never
// grows unbounded while keeping one previous generation around. Safe for
// concurrent use.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	maxBytes int64
}

// NewRotatingFile opens (creating if needed) path for appending and returns
// a RotatingFile that rotates it at maxBytes.
func NewRotatingFile(path string, maxBytes int64) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &RotatingFile{path: path, f: f, size: info.Size(), maxBytes: maxBytes}, nil
}

// Write appends p to the current file, rotating first if p would push the
// file past maxBytes. A single Write call is never split across the
// rotation boundary, so a reader tailing the file never sees a line cut in
// half by rotation.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		// A failed rotation leaves r.f live (see rotate), so rather than drop
		// the line, write it to the current file anyway: an over-size log is
		// far better than a dead one. The next Write retries the rotation.
		_ = r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames the current file to its ".1" sibling (overwriting a prior
// one) and opens a fresh file at path. Caller must hold r.mu.
//
// The order matters for resilience: the daemon log must never be left without
// a live handle, or every later Write fails and the log dies for the rest of
// the process. So the rename happens while the old file is still open (fine
// on POSIX — the handle follows the inode), the new file is opened next, and
// only then is the old handle closed. If the rename fails, nothing has
// changed: keep writing to the existing handle. If the open fails after a
// successful rename, r.f still points at the (now renamed) backup, which is
// a live handle — keep using it and report the error, so logging continues
// (into the .1 file) until a later rotation attempt succeeds.
func (r *RotatingFile) rotate() error {
	backup := r.path + ".1"
	if err := os.Rename(r.path, backup); err != nil {
		return fmt.Errorf("rotate %s: %w", r.path, err)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// Back out: rename the backup into place again so the next rotation
		// attempt starts from the same state and the file keeps its name. The
		// open handle is unaffected either way.
		_ = os.Rename(backup, r.path)
		return fmt.Errorf("rotate %s: %w", r.path, err)
	}
	old := r.f
	r.f = f
	r.size = 0
	_ = old.Close()
	return nil
}

// Close closes the underlying file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
