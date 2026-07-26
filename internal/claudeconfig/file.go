package claudeconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrConcurrentWrite reports that the file mtime/size changed between
// our read and our write attempt. The caller may retry once.
var ErrConcurrentWrite = errors.New("claudeconfig: concurrent write detected")

// Store reads and writes ~/.claude.json. Tests inject a temp path via
// the constructor — the package never reads $HOME directly.
type Store struct {
	path string
}

// New returns a Store bound to the given file path. A non-existent
// file is treated as an empty config on Load; Save creates the file
// (and its parent directory) on first write.
func New(path string) *Store {
	return &Store{path: path}
}

// DefaultPath returns ~/.claude.json based on the current user. The
// helper exists so callers can build a Store with one line; tests
// should always inject an explicit path through New() instead.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// snapshot is the in-memory view used for read+mutate+write cycles.
// stat captures the file metadata at read time so save can detect a
// concurrent write before clobbering. raw is nil if the file was
// missing at read time.
type snapshot struct {
	raw  *orderedJSON
	stat os.FileInfo
}

func (s *Store) load() (*snapshot, error) {
	data, statBefore, err := readFileWithStat(s.path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return &snapshot{raw: newOrderedJSON()}, nil
	}
	obj, err := parseOrderedJSON(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return &snapshot{raw: obj, stat: statBefore}, nil
}

// readFileWithStat returns (contents, info, nil) when the file
// exists. A missing file returns (nil, nil, nil). Any other read or
// stat error is surfaced.
//
// The TOCTOU-safe ordering is stat-read-stat: a writer who atomically
// renames a new version over ours between the read and the post-read
// stat would otherwise hand us v1 bytes paired with v2 metadata,
// which would silently pass writeIfUnchanged on save and clobber the
// concurrent write. When the bracketing stats don't match we retry up
// to a small bounded number of times — Claude Code writes
// ~/.claude.json on a sub-second cadence (metrics, lastSessionId,
// fps), so a tight retry loop converges immediately.
func readFileWithStat(path string) ([]byte, os.FileInfo, error) {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		before, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("stat %s: %w", path, statErr)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("read %s: %w", path, readErr)
		}
		after, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("re-stat %s: %w", path, statErr)
		}
		if before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()) {
			return data, before, nil
		}
	}
	return nil, nil, fmt.Errorf("read %s: file changed during read across %d attempts", path, maxAttempts)
}

// modifyAttempts bounds the read-mutate-write retry loop. Claude Code
// rewrites ~/.claude.json on a sub-second cadence (metrics,
// lastSessionId, fps), so losing a race is routine rather than
// exceptional and a single retry is thin cover. Each attempt re-reads
// first, so fn always runs against current content and retrying is
// idempotent. Same reasoning and same bound as readFileWithStat.
const modifyAttempts = 5

// modify runs fn against the parsed snapshot, then atomically writes
// the result back to disk. If the file's mtime or size changed
// between our read and our write attempt, the call retries. After
// modifyAttempts it returns ErrConcurrentWrite.
func (s *Store) modify(fn func(*orderedJSON) error) error {
	_, err := s.modifyReporting(func(root *orderedJSON) (bool, error) {
		return true, fn(root)
	})
	return err
}

// modifyReporting is modify for mutations that may be no-ops. fn
// reports whether it changed anything; a false return skips the write
// entirely (no rename, no mtime bump) and reports changed=false to the
// caller. Claude Code watches ~/.claude.json's mtime, so a no-op write
// is not free.
func (s *Store) modifyReporting(fn func(*orderedJSON) (bool, error)) (bool, error) {
	for attempt := 0; attempt < modifyAttempts; attempt++ {
		snap, err := s.load()
		if err != nil {
			return false, err
		}
		changed, err := fn(snap.raw)
		if err != nil {
			return false, err
		}
		if !changed {
			return false, nil
		}
		out, err := snap.raw.marshalIndented()
		if err != nil {
			return false, fmt.Errorf("marshal: %w", err)
		}
		ok, err := writeIfUnchanged(s.path, out, snap.stat)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, ErrConcurrentWrite
}

// writeIfUnchanged performs the atomic temp+rename write only when
// the file's mtime/size still match the snapshot we took at load
// time. Returns (true, nil) on success, (false, nil) when another
// writer beat us so the caller can retry, or (_, err) on a write
// error that can't be retried.
func writeIfUnchanged(path string, data []byte, before os.FileInfo) (bool, error) {
	if before != nil {
		current, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("recheck %s: %w", path, err)
		}
		if err == nil && (current.Size() != before.Size() || !current.ModTime().Equal(before.ModTime())) {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, fmt.Errorf("create parent: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".claude.json.tmp.*")
	if err != nil {
		return false, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return false, fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return false, fmt.Errorf("rename: %w", err)
	}
	return true, nil
}
