package trayapp

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
)

// maxLogSizeBytes bounds the tray log file so a long-running tray can never grow it unboundedly.
// Once a write would push the file past this size, the file is truncated and logging continues
// from the start. This is a small, dependency-free survivability log, not an audit trail.
const maxLogSizeBytes = 5 << 20 // 5 MiB

var (
	logMu   sync.Mutex
	logger  = log.New(io.Discard, "", log.LstdFlags|log.Lmicroseconds)
	logPath string
)

// InitLogger opens (or creates) the tray's log file and points package-level logging at it.
// It prefers <repoRoot>/logs/tray.log, matching the directory the tray already uses for
// tray-last-session.json and the startup config. If repoRoot is empty or that location cannot
// be opened (permissions, missing drive, etc.), it falls back to
// %LOCALAPPDATA%\LocalHub\tray.log. If both fail, logging becomes a harmless no-op so callers
// never need to check an error. Returns the resolved log path, or "" if no file could be opened.
func InitLogger(repoRoot string) string {
	logMu.Lock()
	defer logMu.Unlock()

	var candidates []string
	if repoRoot != "" {
		candidates = append(candidates, filepath.Join(repoRoot, "logs", "tray.log"))
	}
	if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
		candidates = append(candidates, filepath.Join(appData, "LocalHub", "tray.log"))
	}

	for _, candidate := range candidates {
		if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
			continue
		}
		file, err := os.OpenFile(candidate, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		size := int64(0)
		if info, statErr := file.Stat(); statErr == nil {
			size = info.Size()
		}
		logger = log.New(&rotatingWriter{file: file, size: size, max: maxLogSizeBytes}, "", log.LstdFlags|log.Lmicroseconds)
		logPath = candidate
		return logPath
	}

	// Nothing writable: keep the discard logger so Logf/SafeGo remain safe no-ops.
	logPath = ""
	return ""
}

// Logf writes one line to the tray log. Safe to call before InitLogger (writes are discarded)
// and safe for concurrent use from multiple goroutines.
func Logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	logger.Printf(format, args...)
}

// LogPath returns the currently active log file path, or "" if logging has not been
// initialized (or initialization failed to find a writable location).
func LogPath() string {
	logMu.Lock()
	defer logMu.Unlock()
	return logPath
}

// SafeGo starts fn in a new goroutine guarded by a recover(). If fn panics, the panic and its
// stack trace are logged instead of taking down the whole (hidden, GUI-less) tray process. name
// identifies the goroutine in the log; it does not change fn's behavior in any way.
func SafeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				Logf("PANIC recovered in goroutine %q: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// rotatingWriter truncates the underlying file back to empty before a write that would push it
// past max, so the tray log cannot grow without bound across a long-running session.
type rotatingWriter struct {
	mu   sync.Mutex
	file *os.File
	size int64
	max  int64
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.max {
		if err := w.file.Truncate(0); err == nil {
			if _, seekErr := w.file.Seek(0, io.SeekStart); seekErr == nil {
				w.size = 0
			}
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}
