// Package cilog keeps the CI job logs the review pane shows as files on
// disk, so a log of any size is read a window at a time and never held
// whole in memory.
//
// A Store owns one cache directory. A job's log is downloaded from its
// forge by a caller-supplied Source, streamed through the trace cleaner
// (GitLab) or passed through (GitHub) into a version file, and indexed as
// it passes: sparse line offsets, a GitHub log's per-second marks and a
// GitLab trace's top-level sections. A completed download is published as
// the entry's current version; readers take a reference to the version
// they read, so a newer version or an eviction never removes a file a
// read still has open. Metadata (ETag, sizes, line count, sections)
// lives beside each version so the cache is reused across restarts.
//
// The cache is bounded by CacheBytes of published files, least recently
// used out; an entry a caller pins (a followed log) is never evicted.
// Deleting the directory, even while the app runs, is the whole cleanup:
// the next download makes it again. Cleanup failures are logged once per
// cause.
package cilog

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// CacheBytes bounds the published log files the store keeps on disk.
// Logs measured on this project's own forges run 1 to 6 MB (a 3.5 MB
// Vitest job, a 5.7 MB GitHub job), so this keeps about a hundred of them
// across restarts, or twenty-five 20 MB logs, while staying a small
// fraction of a data directory that already holds 256 MiB of image
// derivatives. Pinned logs are never evicted, so the logs being followed
// can exceed it by their own size.
const CacheBytes int64 = 512 << 20

const (
	// cacheBlock rounds a file's charge to what a filesystem allocates.
	cacheBlock = 4 << 10
	// partPrefix names a download in progress; the startup scan removes
	// what a previous process left.
	partPrefix = ".part-"
	logExt     = ".log"
	metaExt    = ".json"
)

var (
	// ErrNoLog is a read of a job whose log the store does not hold.
	ErrNoLog = errors.New("cilog: no log downloaded for this job")
	// ErrLogGone is a read of a version whose file was removed from under
	// the store, a user clearing the cache.
	ErrLogGone = errors.New("the downloaded log is gone; refresh to download it again")
)

// Key names one job's log.
type Key struct {
	Forge   string
	Host    string
	Project string
	JobID   string
}

// id is the file stem of the key's log.
func (k Key) id() string {
	sum := sha256.Sum256([]byte(k.Forge + "\x00" + strings.ToLower(k.Host) + "\x00" + k.Project + "\x00" + k.JobID))
	return hex.EncodeToString(sum[:16])
}

func (k Key) github() bool { return k.Forge == "github" }

// Store is the CI log cache. Safe for concurrent use.
type Store struct {
	dir      string
	maxBytes int64
	reports  *reports

	mu      sync.Mutex
	entries map[string]*entry
	// lru holds the entries with a published version, front most recent.
	lru   *list.List
	total int64
	gen   uint64
	// openErr is why the directory could not be opened at start; the next
	// download tries to make it again.
	openErr error
}

// entry is one job's log: its published version and a download in
// progress. fetch serializes the entry's downloads; the fields below it
// are guarded by Store.mu.
type entry struct {
	key  Key
	id   string
	elem *list.Element
	pins int

	fetch sync.Mutex

	cur     *version
	partial *download
	// number is the last version number published, so numbers keep
	// rising across a lost file.
	number uint64
	// sections memoizes the rows of cur for one job state.
	sections     []Section
	sectionsFor  string
	sectionsFrom uint64
}

// Open opens dir, creating it owner-only, and indexes the logs a previous
// process left. A failure is reported and kept: the store still runs, and
// the first download makes the directory again.
func Open(dir string, maxBytes int64, logf func(format string, args ...any)) *Store {
	s := &Store{
		dir:      dir,
		maxBytes: maxBytes,
		reports:  newReports(logf),
		entries:  make(map[string]*entry),
		lru:      list.New(),
	}
	if err := s.scan(); err != nil {
		s.openErr = fmt.Errorf("open CI log cache %q: %w", dir, err)
		s.reports.report("open CI log cache", err)
	}
	return s
}

func (s *Store) entryLocked(key Key) *entry {
	id := key.id()
	e := s.entries[id]
	if e == nil {
		e = &entry{key: key, id: id}
		s.entries[id] = e
	}
	return e
}

// Pin keeps key's log from eviction until a matching Unpin.
func (s *Store) Pin(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entryLocked(key).pins++
}

// Unpin releases one Pin; the cache bound applies to the log again.
func (s *Store) Unpin(key Key) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key.id()]
	if e == nil || e.pins == 0 {
		return
	}
	e.pins--
	s.evictLocked(nil)
	s.forgetLocked(e)
}

// Has reports whether the store holds a published log for key.
func (s *Store) Has(key Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key.id()]
	return e != nil && e.cur != nil
}

// Bytes is the charged size of the published files. Tests only.
func (s *Store) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// publishLocked makes v the entry's current version, retiring the one it
// replaces, and applies the bound.
func (s *Store) publishLocked(e *entry, v *version) {
	if old := e.cur; old != nil {
		s.total -= old.charge
		s.retireLocked(old)
	}
	e.cur = v
	s.total += v.charge
	if e.elem == nil {
		e.elem = s.lru.PushFront(e)
	} else {
		s.lru.MoveToFront(e.elem)
	}
	e.sections, e.sectionsFor = nil, ""
	s.evictLocked(e)
}

// evictLocked drops the least recently used unpinned logs until the
// published files fit the bound. keep is never dropped: the log just
// published stays for the read that asked for it, however large.
func (s *Store) evictLocked(keep *entry) {
	for el := s.lru.Back(); el != nil && s.total > s.maxBytes; {
		prev := el.Prev()
		e := el.Value.(*entry)
		if e.pins == 0 && e != keep {
			s.dropLocked(e, true)
		}
		el = prev
	}
}

// dropLocked removes an entry's published version from the index and,
// when removeFiles, from the disk once no read holds it.
func (s *Store) dropLocked(e *entry, removeFiles bool) {
	if e.cur != nil {
		s.total -= e.cur.charge
		if removeFiles {
			s.retireLocked(e.cur)
			s.remove("remove CI log metadata", filepath.Join(s.dir, e.id+metaExt))
		}
		e.cur = nil
	}
	if e.elem != nil {
		s.lru.Remove(e.elem)
		e.elem = nil
	}
	e.sections, e.sectionsFor = nil, ""
	s.forgetLocked(e)
}

// forgetLocked drops an entry nothing refers to any more.
func (s *Store) forgetLocked(e *entry) {
	if e.cur == nil && e.partial == nil && e.pins == 0 && s.entries[e.id] == e {
		delete(s.entries, e.id)
	}
}

// retireLocked marks a version superseded; its file goes when the last
// read releases it.
func (s *Store) retireLocked(v *version) {
	v.retired = true
	if v.refs == 0 {
		s.remove("remove CI log", v.path)
	}
}

// acquire takes a read reference on key's current version.
func (s *Store) acquire(key Key) (*entry, *version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key.id()]
	if e == nil || e.cur == nil {
		return nil, nil, ErrNoLog
	}
	e.cur.refs++
	if e.elem != nil {
		s.lru.MoveToFront(e.elem)
	}
	return e, e.cur, nil
}

func (s *Store) release(v *version) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.refs--
	if v.refs == 0 && v.retired {
		s.remove("remove CI log", v.path)
	}
}

// lost drops a version whose file is gone, so the next fetch downloads the
// log again instead of answering 304 for a file that no longer exists.
func (s *Store) lost(e *entry, v *version) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.cur == v {
		s.dropLocked(e, false)
	}
}

func (s *Store) remove(op, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.reports.report(op, err)
	}
}

func chargeFor(size int64) int64 {
	if size <= 0 {
		return cacheBlock
	}
	return (size + cacheBlock - 1) / cacheBlock * cacheBlock
}

// reports logs each distinct failure cause once: the operation and the
// innermost wrapped error, without the path a *fs.PathError carries, so
// one failure hitting every file logs one line.
type reports struct {
	logf func(format string, args ...any)
	mu   sync.Mutex
	seen map[string]bool
}

func newReports(logf func(format string, args ...any)) *reports {
	return &reports{logf: logf, seen: make(map[string]bool)}
}

func (r *reports) report(op string, err error) {
	cause := op + ": " + innermost(err).Error()
	r.mu.Lock()
	logged := r.seen[cause]
	r.seen[cause] = true
	r.mu.Unlock()
	if !logged {
		r.logf("cilog: %s: %v", op, err)
	}
}

func innermost(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}
