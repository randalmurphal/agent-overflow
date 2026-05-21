package uitrace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Layout, batch, and rotation limits. The frontend depends on the path
// shape (DirName/FileName) and the JSONL format, so changing either is a
// breaking change for any tool that tails the file.
const (
	DirName        = "ui-trace"
	BookmarkSubdir = "bookmarks"
	FileName       = "ui-render.jsonl"
	MaxBatchLines  = 1000
	MaxBatchBytes  = 2 * 1024 * 1024
	MaxLineBytes   = 64 * 1024
	MaxFileBytes   = 10 * 1024 * 1024

	privateDirPerm    os.FileMode = 0o700
	sensitiveFilePerm os.FileMode = 0o600
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

	if err := ensurePrivateDir(filepath.Dir(t.path)); err != nil {
		return "", fmt.Errorf("create ui trace directory: %w", err)
	}
	if err := rotateIfNeeded(t.path, int64(byteCount)); err != nil {
		return "", err
	}

	file, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, sensitiveFilePerm)
	if err != nil {
		return "", fmt.Errorf("open ui trace file: %w", err)
	}
	if err := chmodSensitiveFile(t.path); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("repair ui trace file permissions: %w", err)
	}
	defer file.Close()

	for _, line := range cleaned {
		if _, err := file.WriteString(line + "\n"); err != nil {
			return "", fmt.Errorf("write ui trace line: %w", err)
		}
	}
	return t.path, nil
}

// Bookmark copies the current trace file and any rotated `.1` predecessor
// into a non-rotating bookmark file. The frontend invokes this from the
// Ctrl+Shift+B handler so the bug-moment context survives the next
// rotation triggered by ongoing render activity. Returns the bookmark
// path on success; if no trace file exists yet, returns the empty path
// without error so the caller can treat it as a no-op.
//
// The bookmark name is timestamped at second precision so concurrent
// repro attempts (each pressing Ctrl+Shift+B within the same second)
// collide intentionally — losing a duplicate bookmark to the second
// presser is preferable to growing a fan-out of near-identical 20 MB
// copies on the user's disk.
func (t *Tracer) Bookmark(now time.Time) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	bookmarkDir := filepath.Join(filepath.Dir(t.path), BookmarkSubdir)
	if err := ensurePrivateDir(bookmarkDir); err != nil {
		return "", fmt.Errorf("create ui trace bookmark directory: %w", err)
	}
	name := fmt.Sprintf("bug-report-%s.jsonl", now.UTC().Format("20060102T150405Z"))
	dest := filepath.Join(bookmarkDir, name)

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, sensitiveFilePerm)
	if err != nil {
		return "", fmt.Errorf("create ui trace bookmark file: %w", err)
	}
	if err := chmodSensitiveFile(dest); err != nil {
		_ = out.Close()
		return "", fmt.Errorf("repair ui trace bookmark permissions: %w", err)
	}
	defer out.Close()

	// Concatenate `.1` (older history) before the current file so a
	// linear read of the bookmark goes earliest-first, matching the
	// JSONL append order any tooling already expects.
	wroteAny := false
	for _, src := range []string{t.path + ".1", t.path} {
		appended, err := appendFile(out, src)
		if err != nil {
			return "", err
		}
		wroteAny = wroteAny || appended
	}
	if !wroteAny {
		// No trace data exists yet — clean up the empty file so the
		// directory listing isn't polluted by zero-byte bookmarks.
		_ = os.Remove(dest)
		return "", nil
	}
	return dest, nil
}

func appendFile(dst io.Writer, srcPath string) (bool, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("open ui trace source %s: %w", srcPath, err)
	}
	defer src.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return false, fmt.Errorf("copy ui trace source %s: %w", srcPath, err)
	}
	return true, nil
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
	if err := chmodSensitiveFile(rotatedPath); err != nil {
		return fmt.Errorf("repair rotated ui trace permissions: %w", err)
	}
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
