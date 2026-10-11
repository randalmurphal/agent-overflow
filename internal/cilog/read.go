package cilog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Index describes the published version of a job's log: its size, its
// rows for the job as given, and how much of it the previous version
// shares. A reader holding lines of version Version-1 keeps the first
// StableLines of them; anything else it holds is from before that and is
// read again.
type Index struct {
	Version     uint64    `json:"version"`
	TotalLines  int       `json:"totalLines"`
	TotalBytes  int64     `json:"totalBytes"`
	StableLines int       `json:"stableLines"`
	Sections    []Section `json:"sections"`
}

// open takes a reference on key's version, loads its index and opens its
// file. The caller closes the file and releases the version.
func (s *Store) open(key Key) (*entry, *version, *os.File, error) {
	e, v, err := s.acquire(key)
	if err != nil {
		return nil, nil, nil, err
	}
	file, err := os.Open(v.path)
	if err == nil {
		if err = s.ensureLoaded(key, v); err != nil {
			_ = file.Close()
		}
	}
	if err != nil {
		s.release(v)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrLogGone) {
			s.lost(e, v)
			return nil, nil, nil, ErrLogGone
		}
		return nil, nil, nil, err
	}
	return e, v, file, nil
}

func (s *Store) closeRead(v *version, file *os.File) {
	if err := file.Close(); err != nil {
		s.reports.report("close CI log", err)
	}
	s.release(v)
}

// Index answers key's published version with its rows for job. The rows
// are kept for the version and the job state they were computed for, so
// a poll that changed neither costs no read.
func (s *Store) Index(key Key, job Job) (Index, error) {
	e, v, file, err := s.open(key)
	if err != nil {
		return Index{}, err
	}
	defer s.closeRead(v, file)
	jobKey := jobStateKey(job)
	s.mu.Lock()
	sections, hit := e.sections, e.sectionsFor == jobKey && e.sectionsFrom == v.number && e.cur == v
	s.mu.Unlock()
	if !hit {
		if key.github() {
			sections, err = githubSections(lineReader{file: file, idx: v.idx}, v.marks, job)
			if err != nil {
				return Index{}, fmt.Errorf("read CI log sections: %w", err)
			}
		} else {
			sections = gitlabSections(v.spans, job, v.idx)
		}
		s.mu.Lock()
		if e.cur == v {
			e.sections, e.sectionsFor, e.sectionsFrom = sections, jobKey, v.number
		}
		s.mu.Unlock()
	}
	return Index{Version: v.number, TotalLines: v.idx.lines, TotalBytes: v.idx.bytes, StableLines: v.stable, Sections: sections}, nil
}

func jobStateKey(job Job) string {
	raw, _ := json.Marshal(job)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Lines reads up to count lines from line from of key's published log,
// within budget bytes of text (see lineReader.lines).
func (s *Store) Lines(key Key, from, count, budget int) (Lines, error) {
	_, v, file, err := s.open(key)
	if err != nil {
		return Lines{}, err
	}
	defer s.closeRead(v, file)
	lines, clipped, err := lineReader{file: file, idx: v.idx}.lines(from, count, budget)
	if err != nil {
		return Lines{}, fmt.Errorf("read CI log: %w", err)
	}
	if lines == nil {
		lines = []string{}
	}
	return Lines{From: from, Lines: lines, Clipped: clipped, TotalLines: v.idx.lines, Version: v.number}, nil
}

// Section answers the row named sectionKey as the last Index computed it.
func (s *Store) Section(key Key, sectionKey string) (Section, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[key.id()]
	if e == nil || e.cur == nil {
		return Section{}, ErrNoLog
	}
	if e.sectionsFrom == e.cur.number {
		for _, section := range e.sections {
			if section.Key == sectionKey {
				return section, nil
			}
		}
	}
	return Section{}, fmt.Errorf("the log has no section %q", sectionKey)
}

// Copy writes lines [from, to) of key's published log to w, the whole log
// for 0 and its line count. It returns the bytes written.
func (s *Store) Copy(key Key, from, to int, w io.Writer) (int64, error) {
	_, v, file, err := s.open(key)
	if err != nil {
		return 0, err
	}
	defer s.closeRead(v, file)
	r := lineReader{file: file, idx: v.idx}
	if to < 0 || to > v.idx.lines {
		to = v.idx.lines
	}
	if from < 0 || from > to {
		return 0, fmt.Errorf("CI log lines %d to %d are outside the log's %d", from, to, v.idx.lines)
	}
	start, err := r.offset(from)
	if err != nil {
		return 0, err
	}
	end, err := r.offset(to)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(w, io.NewSectionReader(file, start, end-start))
	if err != nil {
		return n, fmt.Errorf("copy CI log: %w", err)
	}
	return n, nil
}

// Total is key's published size, for a caller deciding how to copy it.
func (s *Store) Total(key Key) (lines int, bytes int64, err error) {
	_, v, err := s.acquire(key)
	if err != nil {
		return 0, 0, err
	}
	defer s.release(v)
	return v.idx.lines, v.idx.bytes, nil
}
