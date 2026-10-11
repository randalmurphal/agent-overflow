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

func TestTailBufferKeepsTheTailAcrossChunks(t *testing.T) {
	t.Parallel()
	var all bytes.Buffer
	for i := range 500 {
		fmt.Fprintf(&all, "line %04d\n", i)
	}
	data := all.Bytes()
	for _, capacity := range []int{1, 7, 64, 1000, len(data), len(data) + 5} {
		for _, chunk := range []int{1, 3, 13, 64, 999, len(data)} {
			tail := NewTailBuffer(capacity)
			for i := 0; i < len(data); i += chunk {
				end := min(i+chunk, len(data))
				if n, err := tail.Write(data[i:end]); err != nil || n != end-i {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			want := data[max(0, len(data)-capacity):]
			if got := tail.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("cap %d chunk %d: tail = %q..., want %q...", capacity, chunk, head(got), head(want))
			}
			if tail.Total() != int64(len(data)) || tail.Truncated() != (capacity < len(data)) {
				t.Fatalf("cap %d chunk %d: total %d truncated %v", capacity, chunk, tail.Total(), tail.Truncated())
			}
		}
	}
}

func head(b []byte) []byte { return b[:min(len(b), 24)] }

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
