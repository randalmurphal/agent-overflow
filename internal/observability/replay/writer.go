package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// defaultMaxBytes is the threshold at which the current file is rotated.
	// 100 MB balances "enough history for a long-running session" against
	// "doesn't eat a meaningful chunk of disk on a developer laptop."
	defaultMaxBytes int64 = 100 * 1024 * 1024

	// defaultFsyncEvery decides how often we call Sync after a write. We
	// batch so the hot path stays fast; the trade-off is up to N unflushed
	// events can be lost if the app crashes uncleanly.
	defaultFsyncEvery int = 64
)

// Writer is an append-only NDJSON writer for one thread's replay log.
// Writes are protected by an internal mutex, so callers can share a writer
// across goroutines.
type Writer struct {
	mu         sync.Mutex
	file       *os.File
	path       string
	maxBytes   int64
	written    int64
	lastAccess time.Time

	// fsyncEvery is the number of writes between Sync() calls. Exposed on
	// the struct so tests can force every write to flush.
	fsyncEvery  int
	writesSince int
}

// WriterConfig tunes writer behaviour. A zero-value WriterConfig is valid.
type WriterConfig struct {
	// MaxBytes is the rotation threshold for the current file. Zero uses
	// defaultMaxBytes.
	MaxBytes int64
	// FsyncEvery is the number of writes between Sync calls. Zero uses
	// defaultFsyncEvery; 1 flushes every write (useful for tests).
	FsyncEvery int
}

// NewWriter opens (or creates) a per-thread replay file at path. The parent
// directory is created if it doesn't exist. The file is opened with O_APPEND
// so reopening an existing file preserves earlier records.
func NewWriter(path string, cfg WriterConfig) (*Writer, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("replay: create parent dirs %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("replay: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("replay: stat %s: %w", path, err)
	}

	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	fsyncEvery := cfg.FsyncEvery
	if fsyncEvery <= 0 {
		fsyncEvery = defaultFsyncEvery
	}
	return &Writer{
		file:       f,
		path:       path,
		maxBytes:   maxBytes,
		written:    info.Size(),
		lastAccess: time.Now(),
		fsyncEvery: fsyncEvery,
	}, nil
}

// Write appends a record as a single NDJSON line. After each write, if the
// file has grown past MaxBytes, the writer rotates; if the write count
// reaches FsyncEvery, the writer calls Sync on the underlying file.
func (w *Writer) Write(rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("replay: marshal record: %w", err)
	}
	data = append(data, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return fmt.Errorf("replay: writer is closed")
	}
	n, err := w.file.Write(data)
	if err != nil {
		return fmt.Errorf("replay: write: %w", err)
	}
	w.written += int64(n)
	w.lastAccess = time.Now()
	w.writesSince++

	if w.writesSince >= w.fsyncEvery {
		if err := w.file.Sync(); err != nil {
			// A Sync failure doesn't corrupt the stream — the bytes are in
			// the OS buffer already. We surface the error so the caller
			// can notice, but we keep the writer usable.
			w.writesSince = 0
			return fmt.Errorf("replay: sync: %w", err)
		}
		w.writesSince = 0
	}
	if w.written >= w.maxBytes {
		if err := w.rotate(); err != nil {
			return fmt.Errorf("replay: rotate: %w", err)
		}
	}
	return nil
}

// LastAccess reports the wall-clock time of the most recent successful write.
// Used by the manager to evict idle writers.
func (w *Writer) LastAccess() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastAccess
}

// Path returns the on-disk path of the current file.
func (w *Writer) Path() string {
	return w.path
}

// Close flushes and closes the underlying file. Subsequent Write calls return
// an error.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	w.file = nil
	if syncErr != nil {
		return fmt.Errorf("replay: close sync: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("replay: close: %w", closeErr)
	}
	return nil
}

// rotate mirrors logging.Logger.rotate: keep .1/.2/.3 backups, replace the
// current file. Must be called with w.mu held.
func (w *Writer) rotate() error {
	if err := w.file.Close(); err != nil {
		w.file = nil
		return fmt.Errorf("close current: %w", err)
	}

	backup3 := w.path + ".3"
	backup2 := w.path + ".2"
	backup1 := w.path + ".1"

	_ = os.Remove(backup3)
	_ = os.Rename(backup2, backup3)
	_ = os.Rename(backup1, backup2)
	if err := os.Rename(w.path, backup1); err != nil {
		// Best-effort: try to reopen the current file so we stay usable.
		f, openErr := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if openErr != nil {
			w.file = nil
			return fmt.Errorf("rename current and reopen: %w (rename: %w)", openErr, err)
		}
		w.file = f
		return fmt.Errorf("rename current to .1: %w", err)
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.file = nil
		return fmt.Errorf("create new file after rotate: %w", err)
	}
	w.file = f
	w.written = 0
	w.writesSince = 0
	return nil
}
