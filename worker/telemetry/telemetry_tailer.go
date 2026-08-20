package telemetry

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// LogTailer incrementally reads new, complete lines appended to a file since
// its last call. It copes with the file being rotated out from under it (see
// RotatingFile.rotate: the file is renamed to a ".1" sibling and a fresh one
// opened at the same path) by first draining whatever remained unread in the
// rotated-out ".1" file, then starting over from the beginning of the new
// file. A trailing partial line (the writer is mid-write) is left unread
// until the next call, so tailing never ships a half-written line.
type LogTailer struct {
	path   string
	offset int64
	// last identifies the file offset refers to, so a rotation is detected
	// even when the new file has already grown past the old offset. nil
	// until the first successful open.
	last os.FileInfo
}

// NewLogTailer returns a tailer starting from the current end of path, so a
// freshly started exporter does not resend the daemon's entire log history
// on its first push. A missing file starts at offset 0, so lines written
// after this call are picked up once the file exists.
func NewLogTailer(path string) *LogTailer {
	t := &LogTailer{path: path}
	if info, err := os.Stat(path); err == nil {
		t.offset = info.Size()
		t.last = info
	}
	return t
}

// Next returns up to maxLines complete lines appended since the last call
// (or since NewLogTailer, on the first call), advancing the tailer's offset
// past them. It detects rotation by the file identity changing (os.SameFile)
// or, as a fallback, by the file being shorter than the last known offset;
// on rotation it drains the rest of the rotated-out ".1" sibling first, then
// restarts the new file from the beginning.
func (t *LogTailer) Next(maxLines int) ([]string, error) {
	f, err := os.Open(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var lines []string
	rotated := (t.last != nil && !os.SameFile(t.last, info)) || info.Size() < t.offset
	if rotated {
		// Best effort: the previous file now lives at <path>.1 (if rotate's
		// rename succeeded and it has not been rotated away again since).
		// Ship what we had not yet read from it before moving on.
		lines = t.drainRotated(maxLines)
		t.offset = 0
	}
	t.last = info
	if len(lines) >= maxLines {
		return lines, nil
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return lines, err
	}
	more, n := readLines(f, maxLines-len(lines))
	t.offset += n
	return append(lines, more...), nil
}

// drainRotated reads the unread remainder of <path>.1 from the old offset,
// but only when that file is the one the offset refers to (os.SameFile), so
// a stale or foreign ".1" is never misread. Any error is swallowed — this is
// a best-effort salvage on top of the normal restart-from-zero behaviour.
func (t *LogTailer) drainRotated(maxLines int) []string {
	old, err := os.Open(t.path + ".1")
	if err != nil {
		return nil
	}
	defer old.Close()
	info, err := old.Stat()
	if err != nil || !os.SameFile(t.last, info) || info.Size() < t.offset {
		return nil
	}
	if _, err := old.Seek(t.offset, io.SeekStart); err != nil {
		return nil
	}
	lines, _ := readLines(old, maxLines)
	return lines
}

// readLines reads up to maxLines complete newline-terminated lines from r,
// returning them (without the newline) and the number of bytes consumed.
func readLines(rd io.Reader, maxLines int) ([]string, int64) {
	var lines []string
	var n int64
	r := bufio.NewReader(rd)
	for len(lines) < maxLines {
		line, err := r.ReadString('\n')
		if err != nil {
			break // no complete line yet (EOF mid-line): leave it for next call
		}
		lines = append(lines, strings.TrimSuffix(line, "\n"))
		n += int64(len(line))
	}
	return lines, n
}
