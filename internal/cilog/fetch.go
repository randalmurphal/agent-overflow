package cilog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
)

// Request is what a Source asks the forge for. From > 0 resumes a
// download: ask for the bytes from there (Range: bytes=From-). ETag, set
// only when From is 0, is the validator of the version the store holds
// (If-None-Match).
type Request struct {
	From int64
	ETag string
}

// Start describes an answer whose body follows: its status (200 or 206),
// its ETag and, for a 206, the offset its body starts at.
type Start struct {
	Status int
	ETag   string
	Offset int64
}

// Source downloads one job's log. It calls begin once the answer's
// headers are in, and writes the body to the writer begin returns. It
// reports a 304 as notModified with begin never called. A 404 or any
// other failure is its error, passed through.
type Source func(ctx context.Context, req Request, begin func(Start) (io.Writer, error)) (notModified bool, err error)

// FetchResult is what a Fetch did. Changed: a new version was published
// whose lines differ from the one before. Partial: the download was cut
// off after it made progress and is kept, so the next Fetch resumes it.
type FetchResult struct {
	Changed bool
	Partial bool
}

var errRestart = errors.New("cilog: the forge answered a resume with another log")

// download is a download in progress: the part file, the raw bytes taken
// from the forge (where a resume starts) and the cleaner and builder the
// bytes go through. Only the goroutine holding the entry's fetch lock
// touches it.
type download struct {
	file  *os.File
	etag  string
	raw   int64
	feed  io.Writer
	close func() error
	b     *builder
	// prev is the version compared against, read through prevFile.
	prev     *version
	prevFile *os.File
	writeErr error
}

func (d *download) Write(p []byte) (int, error) {
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	if _, err := d.feed.Write(p); err != nil {
		d.writeErr = fmt.Errorf("write CI log: %w", err)
		return 0, d.writeErr
	}
	d.raw += int64(len(p))
	return len(p), nil
}

// Fetch brings key's log up to date from src: revalidates the version it
// holds, downloads a changed log whole into a new version, or resumes a
// download a previous Fetch was cut off in. A download the forge answers
// with another log than the one being resumed (a 200, or a 206 under
// another ETag) starts over. Fetches of one key run one at a time.
func (s *Store) Fetch(ctx context.Context, key Key, src Source) (FetchResult, error) {
	s.mu.Lock()
	e := s.entryLocked(key)
	s.mu.Unlock()
	e.fetch.Lock()
	defer e.fetch.Unlock()
	defer func() {
		s.mu.Lock()
		s.forgetLocked(e)
		s.mu.Unlock()
	}()
	for attempt := 0; ; attempt++ {
		result, err := s.fetchOnce(ctx, e, src)
		if errors.Is(err, errRestart) && attempt == 0 {
			continue
		}
		return result, err
	}
}

func (s *Store) fetchOnce(ctx context.Context, e *entry, src Source) (FetchResult, error) {
	s.mu.Lock()
	resume := e.partial
	var req Request
	if resume != nil {
		req.From = resume.raw
	} else if e.cur != nil {
		req.ETag = e.cur.etag
	}
	s.mu.Unlock()
	before := req.From

	var active *download
	resumed := false
	begin := func(st Start) (io.Writer, error) {
		switch {
		case resume != nil && st.Status == http.StatusPartialContent && st.Offset == resume.raw && st.ETag != "" && st.ETag == resume.etag:
			active, resumed = resume, true
			return active, nil
		case st.Status == http.StatusPartialContent && (resume != nil || st.Offset != 0):
			s.discard(e, resume)
			resume = nil
			return nil, errRestart
		}
		// A whole log, from its first byte.
		s.discard(e, resume)
		resume = nil
		d, err := s.newDownload(e, st.ETag)
		if err != nil {
			return nil, err
		}
		active = d
		return d, nil
	}
	notModified, err := src(ctx, req, begin)
	if active != nil && active.writeErr != nil {
		err = active.writeErr
	}
	switch {
	case err != nil && active == nil:
		return FetchResult{}, err
	case err != nil:
		progressed := active.raw > before
		if active.etag == "" || active.writeErr != nil {
			// Without a validator a resume cannot tell the same log from
			// another; a write failure leaves a file that cannot be trusted.
			s.discard(e, active)
			return FetchResult{}, err
		}
		s.mu.Lock()
		e.partial = active
		s.mu.Unlock()
		if progressed && (resumed || before == 0) {
			return FetchResult{Partial: true}, nil
		}
		return FetchResult{}, err
	case active == nil && notModified:
		return FetchResult{}, nil
	case active == nil:
		// A 200 with no body never called begin through a writer that
		// saw bytes; begin ran, so active is set. A source that reports
		// neither is broken.
		return FetchResult{}, errors.New("cilog: the source answered neither a log nor 304")
	}
	return s.finish(e, active)
}

// newDownload opens the part file of a whole new log, compared as it is
// written against the version it will replace.
func (s *Store) newDownload(e *entry, etag string) (*download, error) {
	file, err := s.createPart(e.id + logExt)
	if err != nil {
		return nil, fmt.Errorf("create CI log file: %w", err)
	}
	d := &download{file: file, etag: etag}
	var prev io.Reader
	if _, v, err := s.acquire(e.key); err == nil {
		if f, openErr := os.Open(v.path); openErr == nil {
			d.prev, d.prevFile, prev = v, f, f
		} else {
			s.release(v)
		}
	}
	d.b = newBuilder(file, e.key.github(), prev)
	if e.key.github() {
		split := &lineSplitter{sink: d.b}
		d.feed, d.close = split, split.close
	} else {
		clean := newTraceCleaner(d.b)
		d.feed, d.close = clean, clean.close
	}
	return d, nil
}

// discard drops a download and its part file.
func (s *Store) discard(e *entry, d *download) {
	if d == nil {
		return
	}
	s.mu.Lock()
	if e.partial == d {
		e.partial = nil
	}
	s.mu.Unlock()
	d.releasePrev(s)
	if err := d.file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		s.reports.report("close unfinished CI log", err)
	}
	s.remove("remove unfinished CI log", d.file.Name())
}

func (d *download) releasePrev(s *Store) {
	if d.prev == nil {
		return
	}
	if err := d.prevFile.Close(); err != nil {
		s.reports.report("close CI log", err)
	}
	s.release(d.prev)
	d.prev, d.prevFile = nil, nil
}

// finish ends a download and publishes it as the entry's version.
func (s *Store) finish(e *entry, d *download) (FetchResult, error) {
	fail := func(err error) (FetchResult, error) {
		s.discard(e, d)
		return FetchResult{}, err
	}
	if err := d.close(); err != nil {
		return fail(fmt.Errorf("write CI log: %w", err))
	}
	idx, stable, err := d.b.finish()
	if err != nil {
		return fail(fmt.Errorf("write CI log: %w", err))
	}
	if err := d.file.Close(); err != nil {
		return fail(fmt.Errorf("write CI log: %w", err))
	}
	s.mu.Lock()
	s.gen++
	gen := s.gen
	e.number++
	number := e.number
	s.mu.Unlock()
	v := &version{
		path: s.logPath(e.id, gen), gen: gen, number: number, etag: d.etag, raw: d.raw, stable: stable,
		charge: chargeFor(idx.bytes), loaded: true, idx: idx, marks: d.b.seconds,
	}
	if !e.key.github() {
		v.spans = d.b.spans.result(idx.lines, idx.bytes)
	}
	prev := d.prev
	changed := prev == nil || stable != idx.lines || idx.lines != prev.idx.lines || idx.bytes != prev.idx.bytes
	d.releasePrev(s)
	if err := os.Rename(d.file.Name(), v.path); err != nil {
		return fail(fmt.Errorf("publish CI log: %w", err))
	}
	if err := s.writeMeta(e, v); err != nil {
		s.remove("remove CI log", v.path)
		return fail(err)
	}
	s.mu.Lock()
	if e.partial == d {
		e.partial = nil
	}
	s.publishLocked(e, v)
	s.mu.Unlock()
	return FetchResult{Changed: changed}, nil
}
