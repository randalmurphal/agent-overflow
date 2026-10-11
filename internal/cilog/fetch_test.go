package cilog

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func partFiles(t *testing.T, dir string) []string {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(dir, partPrefix+"*"))
	return names
}

// A followed GitHub log is revalidated by its ETag and downloaded whole
// again only when the ETag changed; the new version reports how many of
// its leading lines the previous one shares.
func TestFetchRevalidatesAndDownloadsAChangedLogWhole(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	forge := &fakeForge{}
	forge.set(utf8BOM+"a\nb\nc\n", `"v1"`)
	result, err := s.Fetch(t.Context(), key, forge.source())
	if err != nil || !result.Changed || result.Partial {
		t.Fatalf("first fetch = %+v, %v", result, err)
	}
	first, _ := s.Index(key, Job{})
	if first.Version != 1 || first.TotalLines != 3 || first.StableLines != 0 {
		t.Fatalf("first index = %+v", first)
	}

	result, err = s.Fetch(t.Context(), key, forge.source())
	if err != nil || result.Changed || forge.lastRequest() != (Request{ETag: `"v1"`}) {
		t.Fatalf("revalidation = %+v, %v, request %+v", result, err, forge.lastRequest())
	}
	if again, _ := s.Index(key, Job{}); again.Version != 1 {
		t.Fatalf("a 304 published version %d", again.Version)
	}

	// The blob grew under a new ETag: downloaded whole, from its start.
	forge.set(utf8BOM+"a\nb\nc\nd\n", `"v2"`)
	result, err = s.Fetch(t.Context(), key, forge.source())
	if err != nil || !result.Changed || forge.lastRequest() != (Request{ETag: `"v1"`}) {
		t.Fatalf("changed fetch = %+v, %v", result, err)
	}
	grown, _ := s.Index(key, Job{})
	if grown.Version != 2 || grown.TotalLines != 4 || grown.StableLines != 3 {
		t.Fatalf("grown index = %+v, want version 2 keeping 3 lines", grown)
	}
	// A line in the middle changed: only the lines before it are stable.
	forge.set(utf8BOM+"a\nB\nc\nd\n", `"v3"`)
	if _, err := s.Fetch(t.Context(), key, forge.source()); err != nil {
		t.Fatal(err)
	}
	edited, _ := s.Index(key, Job{})
	if edited.Version != 3 || edited.StableLines != 1 {
		t.Fatalf("edited index = %+v, want 1 stable line", edited)
	}
	if got := allLines(t, s, key); !slices.Equal(got, []string{"a", "B", "c", "d"}) {
		t.Fatalf("lines = %q", got)
	}
}

// A download cut off after it made progress is kept, and the next fetch
// asks for the rest with a start range, appending to it.
func TestAnInterruptedDownloadResumesWithARange(t *testing.T) {
	t.Parallel()
	s, dir, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	want := numberedLines(500)
	body := utf8BOM + strings.Join(want, "\n") + "\n"
	forge := &fakeForge{cut: 1000}
	forge.set(body, `"blob"`)
	result, err := s.Fetch(t.Context(), key, forge.source())
	if err != nil || !result.Partial || s.Has(key) {
		t.Fatalf("cut fetch = %+v, %v, published %v", result, err, s.Has(key))
	}
	if parts := partFiles(t, dir); len(parts) != 1 {
		t.Fatalf("part files %v, want the kept download", parts)
	}
	result, err = s.Fetch(t.Context(), key, forge.source())
	if err != nil || !result.Changed {
		t.Fatalf("resumed fetch = %+v, %v", result, err)
	}
	if got := forge.lastRequest(); got != (Request{From: 1000}) {
		t.Fatalf("resume asked %+v, want the bytes from 1000", got)
	}
	if got := allLines(t, s, key); !slices.Equal(got, want) {
		t.Fatalf("resumed log has %d lines, want %d", len(got), len(want))
	}
	if parts := partFiles(t, dir); len(parts) != 0 {
		t.Fatalf("part files left %v", parts)
	}
}

// A running GitLab trace ignores Range: the resume is answered whole, and
// the download starts over from the first byte.
func TestAResumeAnsweredWholeStartsOver(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := gitlabKey("1")
	want := numberedLines(300)
	forge := &fakeForge{cut: 700}
	forge.set(strings.Join(want, "\n")+"\n", `"t1"`)
	if result, err := s.Fetch(t.Context(), key, forge.source()); err != nil || !result.Partial {
		t.Fatalf("cut fetch = %+v, %v", result, err)
	}
	forge.running = true
	if _, err := s.Fetch(t.Context(), key, forge.source()); err != nil {
		t.Fatal(err)
	}
	if got := forge.lastRequest(); got.From != 700 {
		t.Fatalf("second request %+v, want a resume", got)
	}
	if got := allLines(t, s, key); !slices.Equal(got, want) {
		t.Fatalf("restarted log has %d lines, want %d", len(got), len(want))
	}
	// Cut off again after a whole answer to a resume: no progress the next
	// resume can build on, so it is a failure, not a partial.
	forge.set(strings.Join(want, "\n")+"\nmore\n", `"t2"`)
	forge.cut = 500
	forge.running = false
	if result, err := s.Fetch(t.Context(), key, forge.source()); err != nil || !result.Partial {
		t.Fatalf("cut fetch = %+v, %v", result, err)
	}
	forge.running, forge.cut = true, 600
	result, err := s.Fetch(t.Context(), key, forge.source())
	if !errors.Is(err, errCut) || result.Partial {
		t.Fatalf("a whole answer cut off again = %+v, %v; want the failure", result, err)
	}
}

// A resume the forge answers for another log (a 206 under another ETag)
// starts over without Range in the same fetch.
func TestAResumeOfAnotherLogStartsOver(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	forge := &fakeForge{cut: 300}
	forge.set(strings.Join(numberedLines(100), "\n")+"\n", `"old"`)
	if result, err := s.Fetch(t.Context(), key, forge.source()); err != nil || !result.Partial {
		t.Fatalf("cut fetch = %+v, %v", result, err)
	}
	want := numberedLines(150)
	forge.set(strings.Join(want, "\n")+"\n", `"new"`)
	if _, err := s.Fetch(t.Context(), key, forge.source()); err != nil {
		t.Fatal(err)
	}
	if got := forge.requests; len(got) != 3 || got[1].From != 300 || got[2] != (Request{}) {
		t.Fatalf("requests %+v, want the resume and then a whole request", got)
	}
	if got := allLines(t, s, key); !slices.Equal(got, want) {
		t.Fatalf("log has %d lines, want %d", len(got), len(want))
	}
}

// Without an ETag a resume cannot tell the same log from another: the cut
// download is dropped and the cut is the fetch's failure.
func TestADownloadWithoutAValidatorIsNotKept(t *testing.T) {
	t.Parallel()
	s, dir, _ := newTestStore(t, CacheBytes)
	forge := &fakeForge{cut: 50}
	forge.set(strings.Join(numberedLines(100), "\n"), "")
	result, err := s.Fetch(t.Context(), githubKey("1"), forge.source())
	if !errors.Is(err, errCut) || result.Partial || len(partFiles(t, dir)) != 0 {
		t.Fatalf("cut fetch without an ETag = %+v, %v, parts %v", result, err, partFiles(t, dir))
	}
}

// A running GitLab trace is answered whole each time and rewritten; the
// last line of a trace that had no newline yet is not stable.
func TestARunningTraceRewriteReportsItsStableLines(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestStore(t, CacheBytes)
	key := gitlabKey("1")
	forge := &fakeForge{running: true}
	forge.set("a\nb\nDownloading 10%", `"1"`)
	if _, err := s.Fetch(t.Context(), key, forge.source()); err != nil {
		t.Fatal(err)
	}
	forge.set("a\nb\nDownloading 10%\rDownloading 60%\nc\n", `"2"`)
	if _, err := s.Fetch(t.Context(), key, forge.source()); err != nil {
		t.Fatal(err)
	}
	index, _ := s.Index(key, Job{Status: statusRunning})
	if index.Version != 2 || index.TotalLines != 4 || index.StableLines != 2 {
		t.Fatalf("rewritten index = %+v, want 4 lines, 2 stable", index)
	}
	if got := allLines(t, s, key); !slices.Equal(got, []string{"a", "b", "Downloading 60%", "c"}) {
		t.Fatalf("lines %q", got)
	}
}

func TestAFetchFailureBeforeABodyLeavesTheVersion(t *testing.T) {
	t.Parallel()
	s, dir, _ := newTestStore(t, CacheBytes)
	key := githubKey("1")
	fetchText(t, s, key, "a\n", `"1"`)
	forge := &fakeForge{notFound: true}
	if _, err := s.Fetch(t.Context(), key, forge.source()); err == nil {
		t.Fatal("a 404 fetch succeeded")
	}
	if got := allLines(t, s, key); !slices.Equal(got, []string{"a"}) || len(partFiles(t, dir)) != 0 {
		t.Fatalf("after a failed fetch: %q, parts %v", got, partFiles(t, dir))
	}
}
