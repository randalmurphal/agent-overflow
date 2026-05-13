package uitrace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Layout, batch, and rotation limits. The frontend depends on the path
// shape (DirName/FileName) and the JSONL format, so changing either is a
// breaking change for any tool that tails the file.
const (
	DirName       = "ui-trace"
	FileName      = "ui-render.jsonl"
	MaxBatchLines = 1000
	MaxBatchBytes = 2 * 1024 * 1024
	MaxLineBytes  = 64 * 1024
	MaxFileBytes  = 10 * 1024 * 1024
)

// Tracer appends compact dev-only UI render trace records to a JSONL
// file. The Append path is safe for concurrent use; rotation runs
// inline when the on-disk size would exceed MaxFileBytes.
type Tracer struct {
	mu   sync.Mutex
	path string
}

// New returns a Tracer that writes to <configDir>/<DirName>/<FileName>.
// Returns an error if configDir is empty so callers fail loudly before
// the app's data directory is initialized.
func New(configDir string) (*Tracer, error) {
	if configDir == "" {
		return nil, errors.New("ui trace path unavailable before app data directory is initialized")
	}
	return &Tracer{
		path: filepath.Join(configDir, DirName, FileName),
	}, nil
}

// Path returns the absolute trace file path. Useful for surfacing the
// location to the frontend console without taking a trace lock.
func (t *Tracer) Path() string { return t.path }

// Append validates each line against the per-line and per-batch caps,
// rotates the file if appending would exceed MaxFileBytes, and writes
// every accepted line followed by a trailing newline. The function
// returns the trace file path on success.
//
// An empty input or a batch consisting entirely of whitespace lines is
// a no-op and returns the file path without touching disk.
func (t *Tracer) Append(lines []string) (string, error) {
	if len(lines) == 0 {
		return t.path, nil
	}
	if len(lines) > MaxBatchLines {
		return "", fmt.Errorf("ui trace batch has %d lines, max %d", len(lines), MaxBatchLines)
	}

	cleaned, byteCount, err := validateLines(lines)
	if err != nil {
		return "", err
	}
	if len(cleaned) == 0 {
		return t.path, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(t.path), 0755); err != nil {
		return "", fmt.Errorf("create ui trace directory: %w", err)
	}
	if err := rotateIfNeeded(t.path, int64(byteCount)); err != nil {
		return "", err
	}

	file, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", fmt.Errorf("open ui trace file: %w", err)
	}
	defer file.Close()

	for _, line := range cleaned {
		if _, err := file.WriteString(line + "\n"); err != nil {
			return "", fmt.Errorf("write ui trace line: %w", err)
		}
	}
	return t.path, nil
}

func validateLines(lines []string) ([]string, int, error) {
	cleaned := make([]string, 0, len(lines))
	byteCount := 0
	for i, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > MaxLineBytes {
			return nil, 0, fmt.Errorf("ui trace line %d is %d bytes, max %d", i, len(line), MaxLineBytes)
		}
		if !json.Valid([]byte(line)) {
			return nil, 0, fmt.Errorf("ui trace line %d is not valid JSON", i)
		}
		cleaned = append(cleaned, line)
		byteCount += len(line) + 1
		if byteCount > MaxBatchBytes {
			return nil, 0, fmt.Errorf("ui trace batch is %d bytes, max %d", byteCount, MaxBatchBytes)
		}
	}
	return cleaned, byteCount, nil
}

func rotateIfNeeded(path string, pendingBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat ui trace file: %w", err)
	}
	if info.Size()+pendingBytes <= MaxFileBytes {
		return nil
	}

	rotatedPath := path + ".1"
	if err := os.Remove(rotatedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove previous ui trace rotation: %w", err)
	}
	if err := os.Rename(path, rotatedPath); err != nil {
		return fmt.Errorf("rotate ui trace file: %w", err)
	}
	return nil
}
