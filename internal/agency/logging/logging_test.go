package logging

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerWritesJSONWithAttributes(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(dir, Options{Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("reconcile completed", "correlationId", "corr-1", "causationId", "cause-1", "adopted", 2)
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "supervisor.log"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &record); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, raw)
	}
	if record["msg"] != "reconcile completed" || record["correlationId"] != "corr-1" || record["causationId"] != "cause-1" {
		t.Fatalf("record = %+v", record)
	}
	if record["level"] != "INFO" {
		t.Fatalf("level = %v", record["level"])
	}
}

func TestLoggerRotatesAndBoundsRetention(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(dir, Options{MaxBytes: 256, Keep: 2, Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		logger.Info("filler line to force rotation", "i", i, "pad", strings.Repeat("x", 64))
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	logs := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "supervisor.log") {
			logs++
		}
	}
	// current + at most Keep rotated files.
	if logs == 0 || logs > 3 {
		t.Fatalf("rotated log file count = %d, want 1..3", logs)
	}
}
