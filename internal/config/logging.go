package config

import (
	"io"
	"log/slog"
)

// NewLogger builds a slog.Logger from the logging config. Level and format come from the
// config (LoggingConfig); output goes to w (typically os.Stderr).
func (c LoggingConfig) NewLogger(w io.Writer) *slog.Logger {
	var level slog.Level
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if c.Format == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return slog.New(h)
}
