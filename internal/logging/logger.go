package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const defaultMaxBytes int64 = 10 * 1024 * 1024 // 10 MB

const (
	privateDirPerm    os.FileMode = 0o700
	sensitiveFilePerm os.FileMode = 0o600
)

// LogEntry is a single structured log record serialized as one JSON line.
type LogEntry struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"`
	Component string `json:"component"`
	Message   string `json:"msg"`
	Data      any    `json:"data,omitempty"`
}

// Logger writes LogEntry values as NDJSON to a file, rotating when the file
// exceeds a configured size. All methods are safe for concurrent use.
type Logger struct {
	file     *os.File
	mu       sync.Mutex
	maxBytes int64
	path     string
	written  int64
}

// NewLogger opens (or creates) the log file at path for append writing.
// Parent directories are created if they do not exist. If maxBytes is 0,
// the default of 10 MB is used.
func NewLogger(path string, maxBytes int64) (*Logger, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("logging: create parent dirs: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, sensitiveFilePerm)
	if err != nil {
		return nil, fmt.Errorf("logging: open file: %w", err)
	}
	if err := chmodSensitiveFile(path); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("logging: repair file permissions: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("logging: stat file: %w", err)
	}

	return &Logger{
		file:     f,
		maxBytes: maxBytes,
		path:     path,
		written:  info.Size(),
	}, nil
}

// Log marshals entry as a single JSON line and appends it to the log file.
// If entry.Timestamp is empty, it is set to the current time in RFC 3339 format.
// After writing, if the file exceeds maxBytes, rotation is triggered.
func (l *Logger) Log(entry LogEntry) error {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	return l.logValue(entry)
}

// Close flushes and closes the underlying file. Subsequent Log calls will
// return an error.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// rotate performs size-based log rotation. It keeps at most 3 backup files:
//
//	.3 is deleted
//	.2 -> .3
//	.1 -> .2
//	current -> .1
//	new current is created
//
// Must be called with l.mu held.
func (l *Logger) rotate() error {
	// Close current file before renaming.
	if err := l.file.Close(); err != nil {
		l.file = nil
		return fmt.Errorf("close current: %w", err)
	}

	// Shift existing backups. Walk from oldest to newest so renames don't collide.
	backup3 := l.path + ".3"
	backup2 := l.path + ".2"
	backup1 := l.path + ".1"

	_ = os.Remove(backup3)          // delete .3 (may not exist)
	_ = os.Rename(backup2, backup3) // .2 -> .3 (may not exist)
	_ = os.Rename(backup1, backup2) // .1 -> .2 (may not exist)
	if err := os.Rename(l.path, backup1); err != nil {
		// If renaming current file fails, try to reopen it anyway so the
		// logger remains usable.
		f, openErr := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, sensitiveFilePerm)
		if openErr != nil {
			l.file = nil
			return fmt.Errorf("rename current and reopen: %w (rename: %w)", openErr, err)
		}
		_ = chmodSensitiveFile(l.path)
		l.file = f
		return fmt.Errorf("rename current to .1: %w", err)
	}
	_ = chmodSensitiveFile(backup1)
	_ = chmodSensitiveFile(backup2)
	_ = chmodSensitiveFile(backup3)

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, sensitiveFilePerm)
	if err != nil {
		l.file = nil
		return fmt.Errorf("create new log file: %w", err)
	}
	if err := chmodSensitiveFile(l.path); err != nil {
		_ = f.Close()
		l.file = nil
		return fmt.Errorf("repair new log file permissions: %w", err)
	}
	l.file = f
	l.written = 0
	return nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, privateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, privateDirPerm)
}

func chmodSensitiveFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil
	}
	return os.Chmod(path, sensitiveFilePerm)
}

func (l *Logger) logValue(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("logging: marshal entry: %w", err)
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return fmt.Errorf("logging: logger is closed")
	}

	n, err := l.file.Write(data)
	if err != nil {
		return fmt.Errorf("logging: write: %w", err)
	}
	l.written += int64(n)

	if l.written >= l.maxBytes {
		if err := l.rotate(); err != nil {
			return fmt.Errorf("logging: rotate: %w", err)
		}
	}
	return nil
}
