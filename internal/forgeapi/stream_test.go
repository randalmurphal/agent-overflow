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

func TestStreamTail(t *testing.T) {
	t.Parallel()
	log := strings.Repeat("0123456789\n", 1000) // 11000 bytes
	type mode int
	const (
		honor mode = iota
		ignore
		refuse
	)
	newBlob := func(m mode, ranges *seen) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ranges.record(r)
			spec := r.Header.Get("Range")
			if spec == "" || m == ignore {
				http.ServeContent(w, &http.Request{Header: http.Header{}}, "", time.Time{}, strings.NewReader(log))
				return
			}
			if m == refuse {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			http.ServeContent(w, r, "", time.Time{}, strings.NewReader(log))
		}))
	}
	for _, tc := range []struct {
		name string
		mode mode
	}{{"206 range", honor}, {"200 ignores range", ignore}, {"416", refuse}} {
		t.Run(tc.name, func(t *testing.T) {
			var blobSeen, forgeSeen seen
			blob := newBlob(tc.mode, &blobSeen)
			defer blob.Close()
			forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forgeSeen.record(r)
				http.Redirect(w, r, blob.URL+"/blob/job.log?sig=signed", http.StatusFound)
			}))
			defer forge.Close()
			s, _ := liveService(t, &testSource{tokens: []string{testSecret}}, forge)
			tail := NewTailBuffer(1000)
			_, err := s.GitHub("github.com").Stream(t.Context(), Request{Path: "repos/o/r/actions/jobs/1/logs"}, tail, 1<<20)
			if tc.mode == refuse {
				var status *StatusError
				if !errors.As(err, &status) || status.Status != http.StatusRequestedRangeNotSatisfiable || strings.Contains(err.Error(), "signed") {
					t.Fatalf("err = %v, want a redacted 416 *StatusError", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := string(tail.Bytes()); got != log[len(log)-1000:] {
				t.Fatalf("tail = %q", got[:min(len(got), 40)])
			}
			if forgeSeen.count() != 1 {
				t.Fatalf("the logs endpoint was asked %d times; the Range belongs on the blob", forgeSeen.count())
			}
			if blobSeen.count() != 2 {
				t.Fatalf("blob requests = %d, want the probe and the Range", blobSeen.count())
			}
			rangeReq, _ := blobSeen.last()
			if rangeReq.Header.Get("Range") != "bytes=10000-" || rangeReq.URL.Query().Get("sig") != "signed" {
				t.Fatalf("Range request = %s %q", rangeReq.URL, rangeReq.Header.Get("Range"))
			}
			for _, req := range blobSeen.reqs {
				if req.Header.Get("Authorization") != "" {
					t.Fatal("the blob host received the forge credential")
				}
			}
		})
	}

	t.Run("limit", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			// No Content-Length: the limit is enforced while reading.
			w.(http.Flusher).Flush()
			_, _ = fmt.Fprint(w, log)
		}))
		defer srv.Close()
		s := isolatedService(t, srv)
		var dst bytes.Buffer
		if _, err := s.GitLab("gitlab.com").Stream(t.Context(), Request{Path: "projects/1/jobs/2/trace"}, &dst, 100); !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("err = %v, want ErrBodyTooLarge", err)
		}
		if _, err := s.GitLab("gitlab.com").Stream(t.Context(), Request{Path: "projects/1/jobs/2/trace"}, NewTailBuffer(64), int64(len(log))); err != nil {
			t.Fatalf("a tail read through a chunked body: %v", err)
		}
	})
}
