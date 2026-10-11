package forgeapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// startRecorder is a Stream destination that records the answer it is
// told before the body.
type startRecorder struct {
	bytes.Buffer
	status int
	header http.Header
	fail   error
}

func (r *startRecorder) StartStream(status int, header http.Header) error {
	r.status, r.header = status, header
	return r.fail
}

// A GitHub job log redirects to a signed blob. The followed hop carries
// the caller's Range and If-None-Match, so a resume and a revalidation
// reach the blob, and never the forge credential; the destination is told
// the blob's answer (206, its Content-Range and ETag) before the body.
func TestStreamFollowsTheLogBlobWithTheCallersHeaders(t *testing.T) {
	t.Parallel()
	log := strings.Repeat("0123456789\n", 1000)
	var blobSeen, forgeSeen seen
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobSeen.record(r)
		w.Header().Set("ETag", `"blob"`)
		if r.Header.Get("If-None-Match") == `"blob"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(log))
	}))
	defer blob.Close()
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forgeSeen.record(r)
		http.Redirect(w, r, blob.URL+"/blob/job.log?sig=signed", http.StatusFound)
	}))
	defer forge.Close()
	s, _ := liveService(t, &testSource{tokens: []string{testSecret}}, forge)
	gh := s.GitHub("github.com")

	dst := &startRecorder{}
	resp, err := gh.Stream(t.Context(), Request{Path: "repos/o/r/actions/jobs/1/logs", Header: http.Header{"Range": {"bytes=10000-"}}}, dst, NoLimit)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusPartialContent || dst.status != http.StatusPartialContent || dst.String() != log[10000:] {
		t.Fatalf("ranged read = %d (started %d) with %d bytes", resp.Status, dst.status, dst.Len())
	}
	if got := dst.header.Get("Content-Range"); got != fmt.Sprintf("bytes 10000-%d/%d", len(log)-1, len(log)) || dst.header.Get("ETag") != `"blob"` {
		t.Fatalf("started with Content-Range %q, ETag %q", got, dst.header.Get("ETag"))
	}
	req, _ := blobSeen.last()
	if req.Header.Get("Range") != "bytes=10000-" || req.URL.Query().Get("sig") != "signed" {
		t.Fatalf("blob hop = %s with Range %q", req.URL, req.Header.Get("Range"))
	}

	notModified, err := gh.Stream(t.Context(), Request{Path: "repos/o/r/actions/jobs/1/logs", Header: http.Header{"If-None-Match": {`"blob"`}}}, &startRecorder{}, NoLimit)
	if err != nil || !notModified.NotModified {
		t.Fatalf("revalidation = %+v, %v; want NotModified", notModified, err)
	}
	for _, req := range blobSeen.reqs {
		if req.Header.Get("Authorization") != "" {
			t.Fatal("the blob host received the forge credential")
		}
	}
	if forgeSeen.count() != 2 || blobSeen.count() != 2 {
		t.Fatalf("requests: forge %d, blob %d", forgeSeen.count(), blobSeen.count())
	}
}

// A destination that refuses the answer ends the call with its error,
// before any of the body is written.
func TestStreamStarterRefusalEndsTheCall(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "body")
	}))
	defer srv.Close()
	s := isolatedService(t, srv)
	refusal := errors.New("not this log")
	dst := &startRecorder{fail: refusal}
	if _, err := s.GitLab("gitlab.com").Stream(t.Context(), Request{Path: "projects/1/jobs/2/trace"}, dst, NoLimit); !errors.Is(err, refusal) || dst.Len() != 0 {
		t.Fatalf("err = %v with %d bytes written, want the refusal and none", err, dst.Len())
	}
}

func TestStreamLimit(t *testing.T) {
	t.Parallel()
	log := strings.Repeat("0123456789\n", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		// No Content-Length: the limit is enforced while reading.
		w.(http.Flusher).Flush()
		_, _ = fmt.Fprint(w, log)
	}))
	defer srv.Close()
	s := isolatedService(t, srv)
	gl := s.GitLab("gitlab.com")
	var dst bytes.Buffer
	if _, err := gl.Stream(t.Context(), Request{Path: "projects/1/jobs/2/trace"}, &dst, 100); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	// NoLimit streams a body past the service's own body bound.
	dst.Reset()
	if _, err := gl.Stream(t.Context(), Request{Path: "projects/1/jobs/2/trace"}, &dst, NoLimit); err != nil || dst.String() != log {
		t.Fatalf("unbounded read = %d bytes, %v", dst.Len(), err)
	}
	for _, limit := range []int64{0, -2} {
		if _, err := gl.Stream(t.Context(), Request{Path: "projects/1/jobs/2/trace"}, &dst, limit); err == nil {
			t.Fatalf("limit %d was accepted", limit)
		}
	}
}
