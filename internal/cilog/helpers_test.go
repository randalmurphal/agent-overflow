package cilog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var errCut = errors.New("connection reset by peer")

// fakeForge serves one job's log to a Store as a forge would: a 304 for
// the current ETag, a 206 for a resume unless it ignores Range (a running
// GitLab trace), and, once, a body cut off after cut bytes.
type fakeForge struct {
	mu      sync.Mutex
	body    []byte
	etag    string
	running bool
	cut     int64
	// chunk is the size of the writes the body arrives in.
	chunk    int
	requests []Request
	notFound bool
}

func (f *fakeForge) set(body, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.body, f.etag = []byte(body), etag
}

func (f *fakeForge) source() Source {
	return func(ctx context.Context, req Request, begin func(Start) (io.Writer, error)) (bool, error) {
		f.mu.Lock()
		f.requests = append(f.requests, req)
		body, etag, running, cut, chunk, notFound := f.body, f.etag, f.running, f.cut, f.chunk, f.notFound
		f.cut = 0
		f.mu.Unlock()
		if notFound {
			return false, errors.New("404 not found")
		}
		if req.From == 0 && req.ETag != "" && req.ETag == etag {
			return true, nil
		}
		status, offset := http.StatusOK, int64(0)
		if req.From > 0 && !running {
			if req.From >= int64(len(body)) {
				return false, fmt.Errorf("%w: 416", ErrRangeUnsatisfiable)
			}
			status, offset = http.StatusPartialContent, req.From
		}
		w, err := begin(Start{Status: status, ETag: etag, Offset: offset})
		if err != nil {
			return false, err
		}
		body = body[offset:]
		failed := false
		// A cut at the body's length delivers every byte and then fails,
		// before the end was seen.
		if cut > 0 && int64(len(body)) >= cut {
			body, failed = body[:cut], true
		}
		if chunk <= 0 {
			chunk = 7
		}
		for len(body) > 0 {
			n := min(chunk, len(body))
			if _, err := w.Write(body[:n]); err != nil {
				return false, err
			}
			body = body[n:]
		}
		if failed {
			return false, errCut
		}
		return false, nil
	}
}

func (f *fakeForge) lastRequest() Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logSink) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func newTestStore(t *testing.T, maxBytes int64) (*Store, string, *logSink) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ci-logs", "cache")
	logs := &logSink{}
	return Open(dir, maxBytes, logs.logf), dir, logs
}

func githubKey(job string) Key {
	return Key{Forge: "github", Host: "github.com", Project: "o/r", JobID: job}
}

func gitlabKey(job string) Key {
	return Key{Forge: "gitlab", Host: "gitlab.com", Project: "g/r", JobID: job}
}

// fetchText downloads raw for key, whole, and fails the test otherwise.
func fetchText(t *testing.T, s *Store, key Key, raw, etag string) FetchResult {
	t.Helper()
	forge := &fakeForge{}
	forge.set(raw, etag)
	result, err := s.Fetch(t.Context(), key, forge.source())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return result
}

// allLines reads every line of key's published log, a window at a time.
func allLines(t *testing.T, s *Store, key Key) []string {
	t.Helper()
	var out []string
	for {
		window, err := s.Lines(key, len(out), 1000, 1<<20, 1<<30)
		if err != nil {
			t.Fatalf("Lines(%d): %v", len(out), err)
		}
		out = append(out, window.Lines...)
		if len(window.Lines) == 0 || len(out) >= window.TotalLines {
			return out
		}
	}
}

func sectionText(t *testing.T, s *Store, key Key, section Section) []string {
	t.Helper()
	if section.End <= section.Start {
		return nil
	}
	window, err := s.Lines(key, section.Start, section.End-section.Start, 1<<30, 1<<30)
	if err != nil {
		t.Fatalf("Lines: %v", err)
	}
	return window.Lines
}

func findSection(t *testing.T, index Index, key string) Section {
	t.Helper()
	for _, section := range index.Sections {
		if section.Key == key {
			return section
		}
	}
	keys := make([]string, 0, len(index.Sections))
	for _, section := range index.Sections {
		keys = append(keys, section.Key)
	}
	t.Fatalf("no section %q in %s", key, strings.Join(keys, ", "))
	return Section{}
}
