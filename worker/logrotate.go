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
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames the current file to its ".1" sibling (overwriting a prior
// one) and opens a fresh file at path. Caller must hold r.mu.
func (r *RotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	backup := r.path + ".1"
	if err := os.Rename(r.path, backup); err != nil {
		return fmt.Errorf("rotate %s: %w", r.path, err)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	r.f = f
	r.size = 0
	return nil
}

// Close closes the underlying file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
