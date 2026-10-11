package forgeapi

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestETagStoreBoundsAndHostDrop(t *testing.T) {
	t.Parallel()
	key := func(host, fp string, i int) etagKey {
		return etagKey{host: host, fingerprint: fp, method: "GET", url: fmt.Sprintf("https://%s/r/%d", host, i)}
	}
	t.Run("entry bound evicts the least recently used", func(t *testing.T) {
		s := newETagStore()
		s.maxLen = 3
		for i := range 3 {
			s.store(key("h", "fp", i), "e", []byte("b"), nil)
		}
		s.lookup(key("h", "fp", 0))
		s.store(key("h", "fp", 3), "e", []byte("b"), nil)
		if _, ok := s.lookup(key("h", "fp", 1)); ok {
			t.Fatal("the least recently used entry survived")
		}
		if _, ok := s.lookup(key("h", "fp", 0)); !ok {
			t.Fatal("a recently used entry was evicted")
		}
	})
	t.Run("byte bound", func(t *testing.T) {
		s := newETagStore()
		s.maxSize = 1000
		s.store(key("h", "fp", 0), "e", bytes.Repeat([]byte("x"), 600), nil)
		s.store(key("h", "fp", 1), "e", bytes.Repeat([]byte("x"), 600), nil)
		if _, ok := s.lookup(key("h", "fp", 0)); ok || s.bytes > s.maxSize {
			t.Fatalf("bytes = %d over %d", s.bytes, s.maxSize)
		}
		s.store(key("h", "fp", 2), "e", bytes.Repeat([]byte("x"), 2000), nil)
		if _, ok := s.lookup(key("h", "fp", 2)); ok {
			t.Fatal("a body over the whole bound was held")
		}
	})
	t.Run("dropHost", func(t *testing.T) {
		s := newETagStore()
		s.store(key("a", "fp1", 0), "e", []byte("b"), nil)
		s.store(key("a", "fp2", 1), "e", []byte("b"), nil)
		s.store(key("b", "fp1", 0), "e", []byte("b"), nil)
		s.dropHost("a", "")
		if s.order.Len() != 1 || len(s.byKey) != 1 {
			t.Fatalf("entries after drop = %d", s.order.Len())
		}
		if _, ok := s.lookup(key("b", "fp1", 0)); !ok {
			t.Fatal("another host's entry was dropped")
		}
	})
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	blob, _ := url.Parse("https://user:pass@pipelines.actions.githubusercontent.com/logs/job.txt?sig=SECRET&se=2026#frag")
	got := RedactURL(blob, "api.github.com", "github.com")
	if strings.Contains(got, "SECRET") || strings.Contains(got, "pass") || strings.Contains(got, "frag") || !strings.HasSuffix(got, "/logs/job.txt") {
		t.Fatalf("RedactURL(blob) = %q", got)
	}
	api, _ := url.Parse("https://api.github.com/repos/o/r/actions/runs/1/jobs?per_page=100")
	if got := RedactURL(api, "api.github.com"); got != api.String() {
		t.Fatalf("RedactURL(api) = %q, want the query kept", got)
	}
	if RedactURL(nil) != "" {
		t.Fatal("nil URL")
	}
}

func TestNextLink(t *testing.T) {
	t.Parallel()
	header := http.Header{}
	header.Add("Link", `<https://api.github.com/repositories/1/pulls?page=3>; rel="last", <https://api.github.com/repositories/1/pulls?page=2>; rel="next"`)
	if got := nextLink(header.Values("Link")); got != "https://api.github.com/repositories/1/pulls?page=2" {
		t.Fatalf("nextLink = %q", got)
	}
	if nextLink([]string{`<https://x/?page=1>; rel="prev"`}) != "" {
		t.Fatal("prev read as next")
	}
}
