// Package logging configures the process-wide slog logger. Book text is
// never logged; only paths (with basenames), IDs, counts, and errors.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a logger honoring level (debug|info|warn|error).
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
