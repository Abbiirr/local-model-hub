package instances

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRingBuffer_KeepsLastNLines(t *testing.T) {
	r := NewRingBuffer(3)
	r.Write([]byte("a\nb\nc\nd\ne\n"))
	lines := r.Lines()
	if len(lines) != 3 || lines[0] != "c" || lines[2] != "e" {
		t.Fatalf("expected [c d e], got %v", lines)
	}
}

func TestRingBuffer_PartialLine(t *testing.T) {
	r := NewRingBuffer(5)
	r.Write([]byte("hello "))
	r.Write([]byte("world\ndone"))
	lines := r.Lines()
	// "hello world" is a complete line; "done" is partial.
	if len(lines) != 2 || lines[0] != "hello world" || lines[1] != "done" {
		t.Fatalf("got %v", lines)
	}
}

func TestRingBuffer_TailStripsCR(t *testing.T) {
	r := NewRingBuffer(10)
	r.Write([]byte("line1\r\nline2\r\n"))
	if tail := r.Tail(2); tail != "line1\nline2" {
		t.Fatalf("Tail = %q", tail)
	}
}

func TestCappedLogFile_CapsWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cap.log")
	f, err := NewCappedLogFile(p, 10) // 10-byte cap
	if err != nil {
		t.Fatalf("NewCappedLogFile: %v", err)
	}
	f.Write([]byte("0123456789ABCDEF")) // 16 bytes > cap
	f.Close()
	// The file should contain the first 10 bytes plus the truncation notice.
	// We only assert it did not grow unbounded (contains the notice).
	if f.written > 10 {
		t.Errorf("written=%d exceeds cap 10", f.written)
	}
}

func TestCappedLogFile_NilIsNoop(t *testing.T) {
	var f *CappedLogFile
	if _, err := f.Write([]byte("x")); err != nil {
		t.Errorf("nil write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("nil close: %v", err)
	}
}

func TestMultiWriter_NilLogFile(t *testing.T) {
	r := NewRingBuffer(5)
	w := MultiWriter(r, nil)
	w.Write([]byte("hi\n"))
	if got := r.Lines(); len(got) != 1 || got[0] != "hi" {
		t.Fatalf("got %v", got)
	}
}

func TestFilepathDir(t *testing.T) {
	if got := filepathDir(`K:\a\b\c.log`); !strings.HasSuffix(got, `a\b`) {
		t.Errorf("filepathDir = %q", got)
	}
	if got := filepathDir("/a/b/c.log"); got != "/a/b" {
		t.Errorf("filepathDir = %q", got)
	}
	if got := filepathDir("nofile"); got != "" {
		t.Errorf("filepathDir(nofile) = %q, want empty", got)
	}
}
