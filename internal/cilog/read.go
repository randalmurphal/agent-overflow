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
// within budget bytes of text, each line clipped at lineMax bytes (see
// lineReader.lines).
func (s *Store) Lines(key Key, from, count, budget, lineMax int) (Lines, error) {
	_, v, file, err := s.open(key)
	if err != nil {
		return Lines{}, err
	}
	defer s.closeRead(v, file)
	lines, clipped, err := lineReader{file: file, idx: v.idx}.lines(from, count, budget, lineMax)
	if err != nil {
		return Lines{}, fmt.Errorf("read CI log: %w", err)
	}
	if lines == nil {
		lines = []string{}
	}
	return Lines{From: from, Lines: lines, Clipped: clipped, TotalLines: v.idx.lines, Version: v.number}, nil
}

// SectionsWithoutLog are a job's rows while the store holds no log for
// it: a GitHub job's steps, without lines. A GitLab trace's sections come
// from the trace, so there are none.
func SectionsWithoutLog(key Key, job Job) []Section {
	if !key.github() {
		return nil
	}
	// With no lines the split reads nothing, so it cannot fail.
	sections, err := githubSections(lineReader{}, nil, job)
	if err != nil {
		return nil
	}
	return sections
}

// CopyLog writes key's whole published log to w and returns the bytes
// written.
func (s *Store) CopyLog(key Key, w io.Writer) (int64, error) {
	_, v, file, err := s.open(key)
	if err != nil {
		return 0, err
	}
	defer s.closeRead(v, file)
	return copyLines(lineReader{file: file, idx: v.idx}, file, 0, v.idx.lines, w)
}

// CopySection writes the lines of the row named sectionKey to w, as the
// last Index computed the rows of the version it reads, and returns the
// row.
func (s *Store) CopySection(key Key, sectionKey string, w io.Writer) (Section, error) {
	e, v, file, err := s.open(key)
	if err != nil {
		return Section{}, err
	}
	defer s.closeRead(v, file)
	var section Section
	found := false
	s.mu.Lock()
	if e.sectionsFrom == v.number && e.cur == v {
		for _, row := range e.sections {
			if row.Key == sectionKey {
				section, found = row, true
				break
			}
		}
	}
	s.mu.Unlock()
	if !found {
		return Section{}, fmt.Errorf("the log has no section %q", sectionKey)
	}
	if section.Lines == 0 {
		return Section{}, fmt.Errorf("section %q has no lines", section.Name)
	}
	if _, err := copyLines(lineReader{file: file, idx: v.idx}, file, section.Start, section.End, w); err != nil {
		return Section{}, err
	}
	return section, nil
}

// copyLines writes lines [from, to) of a version to w.
func copyLines(r lineReader, file io.ReaderAt, from, to int, w io.Writer) (int64, error) {
	start, err := r.offset(from)
	if err != nil {
		return 0, fmt.Errorf("read CI log: %w", err)
	}
	end, err := r.offset(to)
	if err != nil {
		return 0, fmt.Errorf("read CI log: %w", err)
	}
	n, err := io.Copy(w, io.NewSectionReader(file, start, end-start))
	if err != nil {
		return n, fmt.Errorf("copy CI log: %w", err)
	}
	return n, nil
}
