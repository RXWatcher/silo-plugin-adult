// Package logging provides a small structured-logging facade shared across the
// plugin's sources and entry point.
//
// The plugin runs as a go-plugin subprocess: the host captures the plugin's
// stderr and folds it into its own logs. We therefore emit structured records
// to stderr via log/slog. A single process-wide logger keeps formatting
// consistent and avoids threading a logger through every constructor.
//
// Callers must never log secrets (API keys, full auth headers). Helpers in this
// package deliberately surface only non-sensitive fields (enabled flags, hosts,
// status codes, attempt counts).
package logging

import (
	"log/slog"
	"os"
	"sync"
)

var (
	mu     sync.RWMutex
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
)

// SetLogger overrides the process-wide logger. Used by tests to capture output;
// production code relies on the default stderr logger.
func SetLogger(l *slog.Logger) {
	if l == nil {
		return
	}
	mu.Lock()
	logger = l
	mu.Unlock()
}

// L returns the current process-wide logger.
func L() *slog.Logger {
	mu.RLock()
	defer mu.RUnlock()
	return logger
}
