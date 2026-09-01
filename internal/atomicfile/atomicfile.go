package atomicfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	fileMode = 0o600
	dirMode  = 0o700
)

// WriteJSON marshals v (indented) and writes it to path atomically: it creates
// the parent directory if missing, writes to a temp file in the same directory,
// fsyncs it, and renames it into place. A crash or a concurrent reader can only
// ever see the old file or the new one, never a partial write.
//
// The temp file lives in the destination directory (not os.TempDir) so the
// rename stays on one filesystem and is therefore atomic.
func WriteJSON(path string, v any) error {
	if path == "" {
		return errors.New("atomicfile: empty path")
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("atomicfile: marshal: %w", err)
	}
	return Write(path, data)
}

// Write atomically replaces path with data using private file and directory
// permissions. It is the byte-oriented counterpart to WriteJSON for sensitive
// provider-owned files whose schema Agent Overflow must preserve verbatim.
//
// Both halves of durability are done: the file's bytes are fsynced before the
// rename, and the DIRECTORY is fsynced after it. Without the second one the
// rename itself is only in the page cache, so a machine that loses power right
// after a "successful" write can come back to the old name — or to no name at
// all. The file fsync alone protects the contents of an entry that may not be
// there.
func Write(path string, data []byte) error {
	if path == "" {
		return errors.New("atomicfile: empty path")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("atomicfile: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfile: create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicfile: chmod %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicfile: write %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicfile: sync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomicfile: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("atomicfile: rename %s -> %s: %w", tmpPath, path, err)
	}
	return SyncDir(dir)
}

// SyncDir fsyncs a directory so entries created, renamed or removed inside it
// survive a power loss. Exported because the callers that write more than one
// file into a directory — a staged version tree, a database snapshot — need to
// sync it once at the end rather than once per file.
//
// A no-op on Windows, where a directory handle cannot be opened for the flush
// and the filesystem does not offer the guarantee to ask for.
func SyncDir(dir string) error {
	if dir == "" {
		return errors.New("atomicfile: empty directory")
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("atomicfile: sync dir %s: %w", dir, err)
	}
	return nil
}

// ReadJSON reads path and unmarshals it into v. found is false (with a nil
// error) when the file does not exist — the common "no state saved yet" case —
// so callers can distinguish "absent" from "corrupt" cleanly.
func ReadJSON(path string, v any) (found bool, err error) {
	if path == "" {
		return false, errors.New("atomicfile: empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("atomicfile: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("atomicfile: decode %s: %w", path, err)
	}
	return true, nil
}
