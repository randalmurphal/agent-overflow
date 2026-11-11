package forgeapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// A request that carries a header of its own does not use the ETag
// store, whose key holds no request header: the client adds no
// If-None-Match, the answer is not stored, and a 304 the caller's own
// If-None-Match produced is NotModified with no stored body. A plain
// request keeps its entry through them and revalidates from it.
func TestCallerHeadersBypassTheETagStore(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var sent []string // the If-None-Match of each request, "" for none
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inm := r.Header.Get("If-None-Match")
		mu.Lock()
		sent = append(sent, inm)
		mu.Unlock()
		// Each representation has its own ETag, as a forge's does.
		body, tag := "plain", `W/"plain"`
		switch {
		case r.Header.Get("Range") != "":
			body, tag = "pl", `W/"range"`
		case r.Header.Get("Accept") == "text/plain":
			body, tag = "text", `W/"text"`
		}
		w.Header().Set("ETag", tag)
		if inm == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	gl := isolatedService(t, srv).GitLab("gitlab.com")
	do := func(header http.Header) *Response {
		t.Helper()
		resp, err := gl.Do(t.Context(), Request{Path: "projects/1/items", Header: header})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		return sent[len(sent)-1]
	}

	if resp := do(nil); resp.NotModified || string(resp.Body) != "plain" {
		t.Fatalf("plain = %+v", resp)
	}
	for name, header := range map[string]http.Header{
		"Accept": {"Accept": {"text/plain"}},
		"Range":  {"Range": {"bytes=0-1"}},
	} {
		want := map[string]string{"Accept": "text", "Range": "pl"}[name]
		for range 2 {
			resp := do(header)
			if resp.NotModified || string(resp.Body) != want || last() != "" {
				t.Fatalf("%s = %+v sent If-None-Match %q, want a fresh %q with none", name, resp, last(), want)
			}
		}
	}
	// The caller's own If-None-Match is passed through, and its 304 has no
	// stored body.
	if resp := do(http.Header{"If-None-Match": {`W/"text"`}, "Accept": {"text/plain"}}); !resp.NotModified || resp.Body != nil || last() != `W/"text"` {
		t.Fatalf("caller's revalidation = %+v sent %q", resp, last())
	}
	if resp := do(http.Header{"If-None-Match": {`W/"plain"`}}); !resp.NotModified || resp.Body != nil {
		t.Fatalf("caller's revalidation of the plain URL = %+v, want no stored body", resp)
	}
	if resp := do(nil); !resp.NotModified || string(resp.Body) != "plain" || last() != `W/"plain"` {
		t.Fatalf("plain again = %+v sent %q, want a 304 from its own entry", resp, last())
	}
}
