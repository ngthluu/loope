package telemetry

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// LogTailer incrementally reads new, complete lines appended to a file since
// its last call. It copes with the file being rotated out from under it
// (same path, shorter size — see RotatingFile.rotate) by starting over from
// the beginning of the new file. A trailing partial line (the writer is
// mid-write) is left unread until the next call, so tailing never ships a
// half-written line.
type LogTailer struct {
	path   string
	offset int64
}

// NewLogTailer returns a tailer starting from the current end of path, so a
// freshly started exporter does not resend the daemon's entire log history
// on its first push. A missing file starts at offset 0, so lines written
// after this call are picked up once the file exists.
func NewLogTailer(path string) *LogTailer {
	t := &LogTailer{path: path}
	if info, err := os.Stat(path); err == nil {
		t.offset = info.Size()
	}
	return t
}

// Next returns up to maxLines complete lines appended since the last call
// (or since NewLogTailer, on the first call), advancing the tailer's offset
// past them. It detects rotation by the file being shorter than the last
// known offset and restarts from the beginning in that case.
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
	if info.Size() < t.offset {
		t.offset = 0 // rotated out from under us: start over
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, err
	}

	var lines []string
	r := bufio.NewReader(f)
	for len(lines) < maxLines {
		line, err := r.ReadString('\n')
		if err != nil {
			break // no complete line yet (EOF mid-line): leave it for next call
		}
		lines = append(lines, strings.TrimSuffix(line, "\n"))
		t.offset += int64(len(line))
	}
	return lines, nil
}
