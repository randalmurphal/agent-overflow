package cilog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/appdirs"
)

// version is one published download of a job's log: its display file and
// what indexes it. The index and the per-second marks are rebuilt from the
// file on first use after a restart (loaded false); a GitLab trace's
// spans come from its metadata, since the display file has no markers.
type version struct {
	path   string
	gen    uint64
	number uint64
	etag   string
	raw    int64
	stable int
	spans  []span
	charge int64

	load   sync.Mutex
	loaded bool
	idx    lineIndex
	marks  []secondMark

	// Guarded by Store.mu.
	refs    int
	retired bool
}

// metaFormat is the version of the metadata layout; a file of another one
// is dropped at the scan.
const metaFormat = 1

type meta struct {
	Format  int    `json:"format"`
	Forge   string `json:"forge"`
	Host    string `json:"host"`
	Project string `json:"project"`
	JobID   string `json:"jobId"`
	Gen     uint64 `json:"gen"`
	Number  uint64 `json:"number"`
	ETag    string `json:"etag"`
	Raw     int64  `json:"raw"`
	Lines   int    `json:"lines"`
	Bytes   int64  `json:"bytes"`
	Stable  int    `json:"stable"`
	Spans   []span `json:"spans,omitempty"`
}

func (s *Store) logPath(id string, gen uint64) string {
	return filepath.Join(s.dir, id+"-"+strconv.FormatUint(gen, 10)+logExt)
}

// scan makes the directory and indexes the versions in it. A version
// whose file or metadata is missing, unreadable or inconsistent is
// removed; so is a download a previous process did not finish.
func (s *Store) scan() error {
	if s.dir == "" || !filepath.IsAbs(s.dir) {
		return errors.New("CI log cache directory is not absolute")
	}
	if err := os.MkdirAll(s.dir, appdirs.PrivateDirPerm); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, appdirs.PrivateDirPerm); err != nil {
		return err
	}
	dirents, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	type found struct {
		e       *entry
		modTime time.Time
	}
	var kept []found
	logs := make(map[string]bool)
	for _, d := range dirents {
		name := d.Name()
		switch {
		case strings.HasPrefix(name, partPrefix):
			s.remove("remove unfinished CI log", filepath.Join(s.dir, name))
		case strings.HasSuffix(name, logExt):
			logs[name] = true
		}
	}
	for _, d := range dirents {
		name := d.Name()
		if !strings.HasSuffix(name, metaExt) {
			continue
		}
		id := strings.TrimSuffix(name, metaExt)
		m, modTime, err := s.readMeta(filepath.Join(s.dir, name))
		key := Key{Forge: m.Forge, Host: m.Host, Project: m.Project, JobID: m.JobID}
		logName := filepath.Base(s.logPath(id, m.Gen))
		if err == nil && (m.Format != metaFormat || key.id() != id) {
			err = errors.New("metadata does not describe its file")
		}
		if err == nil && !logs[logName] {
			err = fs.ErrNotExist
		}
		if err == nil {
			info, statErr := os.Stat(filepath.Join(s.dir, logName))
			if statErr != nil || info.Size() != m.Bytes {
				err = errors.New("log file does not match its metadata")
			}
		}
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				s.reports.report("read CI log metadata", err)
			}
			s.remove("remove CI log metadata", filepath.Join(s.dir, name))
			continue
		}
		delete(logs, logName)
		e := &entry{key: key, id: id, number: m.Number}
		e.cur = &version{
			path: filepath.Join(s.dir, logName), gen: m.Gen, number: m.Number, etag: m.ETag,
			raw: m.Raw, stable: m.Stable, spans: m.Spans, charge: chargeFor(m.Bytes),
			idx: lineIndex{lines: m.Lines, bytes: m.Bytes},
		}
		s.gen = max(s.gen, m.Gen)
		s.entries[id] = e
		kept = append(kept, found{e: e, modTime: modTime})
	}
	for name := range logs {
		s.remove("remove CI log without metadata", filepath.Join(s.dir, name))
	}
	// The store is not shared until Open returns.
	slices.SortFunc(kept, func(a, b found) int { return a.modTime.Compare(b.modTime) })
	for _, f := range kept {
		f.e.elem = s.lru.PushFront(f.e)
		s.total += f.e.cur.charge
	}
	s.evictLocked(nil)
	return nil
}

func (s *Store) readMeta(path string) (meta, time.Time, error) {
	var m meta
	info, err := os.Stat(path)
	if err != nil {
		return m, time.Time{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, time.Time{}, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, time.Time{}, err
	}
	return m, info.ModTime(), nil
}

// writeMeta writes a version's metadata beside its file, through a temp
// file renamed into place, so a crash leaves the old metadata or the new.
func (s *Store) writeMeta(e *entry, v *version) error {
	m := meta{
		Format: metaFormat, Forge: e.key.Forge, Host: e.key.Host, Project: e.key.Project, JobID: e.key.JobID,
		Gen: v.gen, Number: v.number, ETag: v.etag, Raw: v.raw, Lines: v.idx.lines, Bytes: v.idx.bytes,
		Stable: v.stable, Spans: v.spans,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	temp, err := s.createPart(e.id + metaExt)
	if err != nil {
		return err
	}
	_, err = temp.Write(raw)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temp.Name(), filepath.Join(s.dir, e.id+metaExt))
	}
	if err != nil {
		s.remove("remove unfinished CI log metadata", temp.Name())
		return fmt.Errorf("write CI log metadata: %w", err)
	}
	return nil
}

// createPart opens a file a download or metadata is written to before its
// rename. A directory that is gone is made again: a user clears the cache
// by deleting it, which must not fail every later download for the rest
// of the process.
func (s *Store) createPart(name string) (*os.File, error) {
	file, err := os.CreateTemp(s.dir, partPrefix+name+"-*")
	if !errors.Is(err, fs.ErrNotExist) {
		return file, err
	}
	if err := s.recreate(); err != nil {
		return nil, err
	}
	return os.CreateTemp(s.dir, partPrefix+name+"-*")
}

// recreate makes the directory when it no longer exists and empties the
// index with it: nothing the index named survived the deletion. Under the
// lock so a publish that got there first is not reset after its rename.
func (s *Store) recreate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(s.dir, appdirs.PrivateDirPerm); err != nil {
		return err
	}
	for _, e := range s.entries {
		if e.cur != nil {
			// Its file went with the directory; a read holding it fails
			// on its own.
			e.cur.retired = true
			e.cur = nil
		}
		e.elem = nil
		e.sections, e.sectionsFor = nil, ""
	}
	s.lru.Init()
	s.total = 0
	s.openErr = nil
	return nil
}

// ensureLoaded rebuilds a version's index from its file the first time it
// is read after a restart.
func (s *Store) ensureLoaded(key Key, v *version) error {
	v.load.Lock()
	defer v.load.Unlock()
	if v.loaded {
		return nil
	}
	file, err := os.Open(v.path)
	if err != nil {
		return err
	}
	defer file.Close()
	b := newBuilder(io.Discard, key.github(), nil)
	split := &lineSplitter{sink: b, started: true}
	if _, err := io.Copy(split, file); err != nil {
		return err
	}
	if err := split.close(); err != nil {
		return err
	}
	idx, _, err := b.finish()
	if err != nil {
		return err
	}
	if idx.lines != v.idx.lines || idx.bytes != v.idx.bytes {
		return fmt.Errorf("%w: it no longer matches its metadata", ErrLogGone)
	}
	v.idx, v.marks, v.loaded = idx, b.seconds, true
	return nil
}
