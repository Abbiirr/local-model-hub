package instances

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"sync"
)

// RingBuffer is a fixed-capacity, line-oriented, in-memory tail of a child process's combined
// output. It keeps the last N lines so a health-timeout FAILED reason can carry "the last
// stderr lines" (P4.2 gate) and the logs SSE tail can show recent output without holding the
// whole log in memory. It is an io.Writer: the adapter tees the child's stdout/stderr into it
// (and, in parallel, into a size-capped log file).
//
// Safe for concurrent Write/Lines.
type RingBuffer struct {
	mu       sync.Mutex
	maxLines int
	lines    []string
	partial  []byte // bytes since the last newline (a line still being written)
}

// NewRingBuffer builds a ring holding the last maxLines lines (min 1).
func NewRingBuffer(maxLines int) *RingBuffer {
	if maxLines < 1 {
		maxLines = 1
	}
	return &RingBuffer{maxLines: maxLines}
}

// Write appends p, splitting on newlines and retaining only the last maxLines complete lines
// (plus any trailing partial line). Never errors; always reports len(p) consumed.
func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data := append(r.partial, p...)
	r.partial = nil
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			r.partial = append(r.partial[:0], data...)
			break
		}
		line := string(bytes.TrimRight(data[:i], "\r"))
		r.appendLine(line)
		data = data[i+1:]
	}
	return len(p), nil
}

func (r *RingBuffer) appendLine(line string) {
	r.lines = append(r.lines, line)
	if len(r.lines) > r.maxLines {
		r.lines = r.lines[len(r.lines)-r.maxLines:]
	}
}

// Lines returns a copy of the buffered lines, oldest first (including the current partial line
// if any content is pending).
func (r *RingBuffer) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	if len(r.partial) > 0 {
		out = append(out, string(bytes.TrimRight(r.partial, "\r")))
	}
	return out
}

// Tail returns the last n lines joined by newlines (fewer if not that many). Used for the
// FAILED reason ("last stderr lines").
func (r *RingBuffer) Tail(n int) string {
	lines := r.Lines()
	if n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	var b bytes.Buffer
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	return b.String()
}

// CappedLogFile is an io.WriteCloser that writes to a log file but stops writing once a byte
// cap is reached (the file is size-capped per P4.2: "log file under scratch/data dir,
// size-capped"). Further writes are dropped after a one-line truncation notice. Safe for
// concurrent writes. A nil *CappedLogFile is a valid no-op writer/closer (so callers need no
// nil checks when file logging is disabled).
type CappedLogFile struct {
	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	written int64
	cap     int64
	capped  bool
}

// NewCappedLogFile opens (creating/truncating) path for writing, capped at capBytes. A
// zero/negative cap defaults to 8 MiB. Returns nil (and no error) if path is empty — logging to
// a file is optional; the ring buffer always captures the tail. It creates the parent dir.
func NewCappedLogFile(path string, capBytes int64) (*CappedLogFile, error) {
	if path == "" {
		return nil, nil
	}
	if capBytes <= 0 {
		capBytes = 8 << 20
	}
	if dir := filepathDir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &CappedLogFile{f: f, w: bufio.NewWriter(f), cap: capBytes}, nil
}

func (c *CappedLogFile) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capped {
		return len(p), nil // silently drop past the cap
	}
	remaining := c.cap - c.written
	if int64(len(p)) > remaining {
		if remaining > 0 {
			_, _ = c.w.Write(p[:remaining])
			c.written += remaining
		}
		_, _ = c.w.WriteString("\n[log truncated: size cap reached]\n")
		c.capped = true
		_ = c.w.Flush()
		return len(p), nil
	}
	n, _ := c.w.Write(p)
	c.written += int64(n)
	return len(p), nil
}

func (c *CappedLogFile) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w != nil {
		_ = c.w.Flush()
	}
	if c.f != nil {
		return c.f.Close()
	}
	return nil
}

// MultiWriter returns an io.Writer that fans a single Write to every non-nil writer. It is a
// thin, nil-tolerant wrapper (io.MultiWriter panics on a nil *CappedLogFile passed as an
// io.Writer because the typed-nil is non-nil as an interface). Used to tee child output to the
// ring buffer + the (optional) log file.
func MultiWriter(ring *RingBuffer, logFile *CappedLogFile) io.Writer {
	ws := []io.Writer{ring}
	if logFile != nil {
		ws = append(ws, logFile)
	}
	return io.MultiWriter(ws...)
}

// filepathDir returns the directory portion of a path without importing path/filepath at the
// top (kept local so the file's imports stay minimal). It handles both separators.
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return ""
}
