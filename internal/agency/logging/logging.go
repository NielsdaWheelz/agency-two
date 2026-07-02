// Package logging is the supervisor's operational telemetry surface: structured,
// leveled JSON logs written to the state log directory with size-based rotation
// and bounded retention. It is deliberately separate from the domain event log
// (which is audit, not telemetry). Log records carry correlation and causation
// ids when the operation supplies them so a request can be traced across lines.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Logger is a leveled JSON logger over a rotating file writer.
type Logger struct {
	*slog.Logger
	writer *rotatingWriter
}

// Options configure the rotating log file.
type Options struct {
	MaxBytes int64 // rotate when a write would exceed this size (0 = 8 MiB)
	Keep     int   // number of rotated files to retain (0 = 5)
	Level    slog.Level
}

// New creates a logger writing JSON records to <dir>/supervisor.log with
// rotation. The directory is created 0700 and files 0600 because operational
// logs can echo paths and diagnostics.
func New(dir string, opts Options) (*Logger, error) {
	if dir == "" {
		return nil, fmt.Errorf("log directory is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 8 << 20
	}
	if opts.Keep <= 0 {
		opts.Keep = 5
	}
	w, err := newRotatingWriter(filepath.Join(dir, "supervisor.log"), opts.MaxBytes, opts.Keep)
	if err != nil {
		return nil, err
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: opts.Level})
	return &Logger{Logger: slog.New(handler), writer: w}, nil
}

// NewToWriter builds a logger over an arbitrary writer (used by tests).
func NewToWriter(w io.Writer, level slog.Level) *Logger {
	return &Logger{Logger: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))}
}

// Close flushes and closes the underlying file.
func (l *Logger) Close() error {
	if l == nil || l.writer == nil {
		return nil
	}
	return l.writer.Close()
}

// rotatingWriter is a size-rotating, retention-bounded append writer. Rotation
// renames supervisor.log -> supervisor.log.1 -> .2 ... up to Keep, dropping the
// oldest, so total on-disk size stays bounded.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	file     *os.File
	size     int64
}

func newRotatingWriter(path string, maxBytes int64, keep int) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	// Drop the file that would exceed the retention window, then shift the rest.
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep))
	for i := w.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return err
	}
	return w.open()
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
