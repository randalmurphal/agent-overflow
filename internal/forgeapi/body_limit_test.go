package forgeapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// bodyServer answers every GET with a JSON string body of exactly the
// size its "size" query names, with an X-Next-Page while "page" is below
// "pages", and counts the requests.
func bodyServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pages, _ := strconv.Atoi(r.URL.Query().Get("pages"))
		if max(page, 1) < pages {
			w.Header().Set("X-Next-Page", strconv.Itoa(max(page, 1)+1))
		}
		_, _ = w.Write([]byte(`"` + strings.Repeat("x", size-2) + `"`))
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func sized(size, pages int) url.Values {
	return url.Values{"size": {strconv.Itoa(size)}, "pages": {strconv.Itoa(pages)}}
}

// A request's MaxBodyBytes bounds its body in place of the default, up or
// down, and an unset one is the default; a body past the bound still
// fails with ErrBodyTooLarge naming the bound that applied.
func TestRequestBodyLimit(t *testing.T) {
	t.Parallel()
	srv, _ := bodyServer(t)
	gl := isolatedService(t, srv).GitLab("gitlab.com")
	cases := []struct {
		name  string
		limit int64
		size  int
		fails bool
	}{
		{"unset admits the default", 0, defaultMaxBodyBytes, false},
		{"unset refuses past the default", 0, defaultMaxBodyBytes + 1, true},
		{"a larger bound admits past the default", 2 << 20, 3 << 19, false},
		{"a larger bound admits its own size", 2 << 20, 2 << 20, false},
		{"a larger bound refuses past itself", 2 << 20, 2<<20 + 1, true},
		{"a smaller bound refuses under the default", 10, 11, true},
		{"the ceiling admits its own size", maxRequestBodyBytes, maxRequestBodyBytes, false},
	}
	for _, tc := range cases {
		var out string
		_, err := gl.JSON(t.Context(), Request{Path: "projects/1/items", Query: sized(tc.size, 1), MaxBodyBytes: tc.limit}, &out)
		if !tc.fails {
			if err != nil || len(out) != tc.size-2 {
				t.Errorf("%s: %d bytes = %d decoded, %v", tc.name, tc.size, len(out), err)
			}
			continue
		}
		applied := tc.limit
		if applied == 0 {
			applied = defaultMaxBodyBytes
		}
		if !errors.Is(err, ErrBodyTooLarge) || !strings.Contains(err.Error(), fmt.Sprintf("(limit %d)", applied)) {
			t.Errorf("%s: %d bytes = %v, want ErrBodyTooLarge at limit %d", tc.name, tc.size, err, applied)
		}
	}
}

// Every page Pages reads takes the request's bound.
func TestRequestBodyLimitCarriesAcrossPages(t *testing.T) {
	t.Parallel()
	srv, _ := bodyServer(t)
	gl := isolatedService(t, srv).GitLab("gitlab.com")
	pages := 0
	err := gl.Pages(t.Context(), Request{Path: "projects/1/items", Query: sized(3<<19, 3), MaxBodyBytes: 2 << 20}, func(resp *Response) (bool, error) {
		pages++
		if len(resp.Body) != 3<<19 {
			return false, fmt.Errorf("page %d is %d bytes", pages, len(resp.Body))
		}
		return true, nil
	})
	if err != nil || pages != 3 {
		t.Fatalf("pages = %d, %v", pages, err)
	}
}

// A bound below zero or above the ceiling, or one on a Stream (which
// takes its limit as an argument), is refused before anything is sent.
func TestRequestBodyLimitIsValidated(t *testing.T) {
	t.Parallel()
	srv, requests := bodyServer(t)
	gl := isolatedService(t, srv).GitLab("gitlab.com")
	for _, limit := range []int64{-1, maxRequestBodyBytes + 1} {
		if _, err := gl.Do(t.Context(), Request{Path: "projects/1/items", Query: sized(10, 1), MaxBodyBytes: limit}); err == nil || !strings.Contains(err.Error(), "MaxBodyBytes") {
			t.Errorf("MaxBodyBytes %d = %v, want it refused", limit, err)
		}
	}
	var dst bytes.Buffer
	if _, err := gl.Stream(t.Context(), Request{Path: "projects/1/items", Query: sized(10, 1), MaxBodyBytes: 100}, &dst, 100); err == nil || !strings.Contains(err.Error(), "MaxBodyBytes") {
		t.Errorf("Stream with MaxBodyBytes = %v, want it refused", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("refused requests sent %d", n)
	}
}

// The ceiling keeps one body the ETag store holds to a quarter of the
// store's byte bound, so a large page cannot evict every other entry.
func TestRequestBodyCeilingFitsTheETagStore(t *testing.T) {
	t.Parallel()
	if maxRequestBodyBytes*4 > etagMaxBytes {
		t.Fatalf("ceiling %d is over a quarter of the ETag store's %d bytes", maxRequestBodyBytes, etagMaxBytes)
	}
}
