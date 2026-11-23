package trackerapi

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A body reaches the caller in a slice of exactly its length, whether the
// answer declared it, sent it in chunks, or sent it compressed for
// net/http to decode, and the compressed one is still held to the bound.
func TestAnswerBodyIsExactSize(t *testing.T) {
	t.Parallel()
	payload := `{"text":"` + strings.Repeat("a", 70<<10) + `"}`
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write([]byte(payload))
	_ = zw.Close()
	tr := newTracker(t, route{
		"GET /sized": func(w http.ResponseWriter, r *http.Request) { writeString(w, payload) },
		"GET /chunked": func(w http.ResponseWriter, r *http.Request) {
			writeString(w, payload[:1000])
			_ = http.NewResponseController(w).Flush()
			writeString(w, payload[1000:])
		},
		"GET /gzip": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(zipped.Bytes())
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	for _, path := range []string{"sized", "chunked", "gzip"} {
		resp, err := c.Do(t.Context(), Request{Path: path})
		if err != nil || string(resp.Body) != payload {
			t.Fatalf("%s: %v", path, err)
		}
		if cap(resp.Body) != len(resp.Body) {
			t.Errorf("%s: a body of %d bytes holds %d", path, len(resp.Body), cap(resp.Body))
		}
	}
	_, err := c.Do(t.Context(), Request{Path: "gzip", MaxBodyBytes: int64(len(payload) - 1)})
	if _, ok := errors.AsType[*BodyTooLargeError](err); !ok {
		t.Fatalf("a compressed body over the bound: %v", err)
	}
}

// drawPooled takes up to n buffers from the pool and reports each to
// see. The race detector drops a share of what is pooled, and a
// collection empties the pool, so a test reads what it finds.
func drawPooled(n int, see func([]byte)) (seen int) {
	for range n {
		held, _ := answerBuffers.Get().(*[]byte)
		if held == nil {
			continue
		}
		seen++
		see((*held)[:cap(*held)])
	}
	return seen
}

// A buffer an answer was read into is cleared before it is pooled again,
// since the answer may hold a credential the scrub replaced in the copy
// the caller got; one grown past answerBufferKeep is not pooled at all.
func TestPooledAnswerBuffers(t *testing.T) {
	secretText := strings.Repeat("s", 100<<10)
	seen := 0
	for range 20 {
		got, err := readUpTo(unsized{strings.NewReader(secretText)}, -1, defaultMaxBodyBytes)
		if err != nil || string(got) != secretText {
			t.Fatal(err)
		}
		seen += drawPooled(1, func(b []byte) {
			if bytes.ContainsFunc(b, func(r rune) bool { return r != 0 }) {
				t.Fatal("a pooled buffer still holds the answer read into it")
			}
		})
	}
	if seen == 0 {
		t.Fatal("no buffer was pooled")
	}
	// An answer of exactly the cap is pooled: its buffer doubles to the
	// cap and not past it, and is not doubled again to learn the answer
	// ended.
	page := strings.Repeat("p", answerBufferKeep)
	pooledPage := 0
	for range 20 {
		if _, err := readUpTo(unsized{strings.NewReader(page)}, -1, defaultMaxBodyBytes); err != nil {
			t.Fatal(err)
		}
		drawPooled(1, func(b []byte) {
			if len(b) >= len(page) {
				pooledPage++
			}
		})
	}
	if pooledPage == 0 {
		t.Fatalf("no buffer of a %d byte page was pooled", len(page))
	}
	large := strings.Repeat("l", answerBufferKeep+1)
	for range 5 {
		if _, err := readUpTo(unsized{strings.NewReader(large)}, -1, defaultMaxBodyBytes); err != nil {
			t.Fatal(err)
		}
		drawPooled(1, func(b []byte) {
			if len(b) > answerBufferKeep {
				t.Fatalf("a buffer of %d bytes was pooled", len(b))
			}
		})
	}
}

// readUpTo reads what the reader holds whatever length was declared, and
// never past the limit.
func TestReadUpToDeclaredLength(t *testing.T) {
	t.Parallel()
	failure := errors.New("reset")
	for _, tc := range []struct {
		name     string
		r        io.Reader
		size     int64
		limit    int64
		want     string
		wantErr  error
		wantSize int
	}{
		{"as declared", strings.NewReader("0123456789"), 10, 100, "0123456789", nil, 10},
		{"longer than declared", strings.NewReader("0123456789"), 4, 100, "0123456789", nil, 10},
		{"shorter than declared", strings.NewReader("012"), 10, 100, "012", nil, 3},
		{"declared past the limit", strings.NewReader("0123456789"), 10, 8, "01234567", nil, 8},
		{"longer than declared, to the limit", strings.NewReader("0123456789"), 4, 8, "01234567", nil, 8},
		{"undeclared, to the limit", strings.NewReader("0123456789"), -1, 8, "01234567", nil, 8},
		{"declared empty", strings.NewReader(""), 0, 100, "", nil, 0},
		{"cut short", io.MultiReader(strings.NewReader("012"), iotestErr{failure}), 10, 100, "012", failure, 3},
		{"undeclared, cut short", io.MultiReader(strings.NewReader("012"), iotestErr{failure}), -1, 100, "012", failure, 3},
	} {
		got, err := readUpTo(tc.r, tc.size, tc.limit)
		if string(got) != tc.want || !errors.Is(err, tc.wantErr) || len(got) != tc.wantSize || cap(got) != len(got) {
			t.Errorf("%s: %q (cap %d) %v, want %q %v", tc.name, got, cap(got), err, tc.want, tc.wantErr)
		}
	}
	if n := declaredSize(&http.Response{Body: http.NoBody, ContentLength: 1 << 30}); n != 0 {
		t.Errorf("a body that is none, as a HEAD's, declares %d bytes", n)
	}
}

// iotestErr fails every read with err.
type iotestErr struct{ err error }

func (r iotestErr) Read([]byte) (int, error) { return 0, r.err }
