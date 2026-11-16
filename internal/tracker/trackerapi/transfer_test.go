package trackerapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A transfer has no total timeout: a download that keeps moving outlives
// ReadTimeout, one that stops fails after the stall timeout, and a call
// that is not a transfer keeps its timeout.
func TestTransferStallTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			switch r.URL.Path {
			case "/slow":
				for range 4 {
					writeString(w, "chunk")
					_ = http.NewResponseController(w).Flush()
					time.Sleep(stallTimeout - time.Second)
				}
			case "/stuck":
				writeString(w, "chunk")
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
			}
		}, nil)

		start := time.Now()
		var dst bytes.Buffer
		if _, err := c.Stream(t.Context(), Request{Path: "slow", Transfer: true}, &dst, 1<<20); err != nil || dst.String() != strings.Repeat("chunk", 4) {
			t.Fatalf("a moving download: %v, %q", err, dst.String())
		}
		if elapsed := time.Since(start); elapsed < 4*(stallTimeout-time.Second) {
			t.Fatalf("the download took %s", elapsed)
		}

		requests.Store(0)
		start = time.Now()
		dst.Reset()
		_, err := c.Stream(t.Context(), Request{Path: "stuck", Transfer: true}, &dst, 1<<20)
		if _, ok := errors.AsType[*TransientError](err); !ok || !errors.Is(err, errStalled) || !strings.Contains(err.Error(), "no data moved for 1m30s") {
			t.Fatalf("a stalled download: %v", err)
		}
		if elapsed := time.Since(start); elapsed != stallTimeout {
			t.Fatalf("the stall was noticed after %s, want %s", elapsed, stallTimeout)
		}
		// The first byte reached dst, so the call is not sent again.
		if requests.Load() != 1 {
			t.Fatalf("a stalled download was sent %d times", requests.Load())
		}

		_, err = c.Do(t.Context(), Request{Path: "slow"})
		if _, ok := errors.AsType[*TransientError](err); !ok || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a call that is not a transfer: %v, want its timeout", err)
		}
	})
}

// An upload has the stall timeout while it sends and the header timeout
// once sent: a tracker that takes minutes to answer an upload is not a
// stall, and one that never answers fails after the header timeout. Each
// is sent once.
func TestUploadTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			switch r.URL.Path {
			case "/process":
				_, _ = io.Copy(io.Discard, r.Body)
				time.Sleep(headerTimeout - time.Second)
				writeString(w, `{"id":"1"}`)
			case "/silent":
				_, _ = io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
			}
		}, nil)
		upload := func(path string, size int) (time.Duration, error) {
			body, contentType, err := Multipart(t.Context(), []Part{{FieldName: "file", FileName: "a.bin", Content: bytes.NewReader(make([]byte, size))}}, 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: path, Body: body, ContentType: contentType, Transfer: true})
			return time.Since(start), err
		}

		if elapsed, err := upload("process", 1<<10); err != nil || elapsed != headerTimeout-time.Second {
			t.Fatalf("an upload the tracker took a while to answer: %v after %s", err, elapsed)
		}
		elapsed, err := upload("silent", 1<<10)
		if !errors.Is(err, errStalled) || !strings.Contains(err.Error(), "no answer within 5m0s") || elapsed != headerTimeout || WriteNotApplied(err) {
			t.Fatalf("an upload never answered: %v after %s", err, elapsed)
		}
		if requests.Load() != 2 {
			t.Fatalf("%d uploads sent, want each once", requests.Load())
		}
	})
}

// The stall times the upload body as the transport reads it: progress
// keeps it alive, a pause of the stall timeout ends it, and once the body
// is all read the header timeout applies. The in-memory network buffers
// without bound, so a tracker that stops reading is driven here directly.
func TestStallTimesTheUploadBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := newStall(t.Context(), true)
		defer st.stop()
		body := st.upload(make([]byte, 10))
		for range 3 {
			time.Sleep(stallTimeout - time.Second)
			if _, err := body.Read(make([]byte, 1)); err != nil || st.ctx.Err() != nil {
				t.Fatalf("a moving upload stalled: %v", context.Cause(st.ctx))
			}
		}
		time.Sleep(stallTimeout)
		synctest.Wait() // the timer's callback runs at the same instant
		if cause := context.Cause(st.ctx); !errors.Is(cause, errStalled) || !strings.Contains(cause.Error(), "no data moved") {
			t.Fatalf("a paused upload: %v", cause)
		}

		st = newStall(t.Context(), true)
		defer st.stop()
		body = st.upload(make([]byte, 10))
		if _, err := io.ReadAll(body); err != nil {
			t.Fatal(err)
		}
		time.Sleep(headerTimeout - time.Second)
		if st.ctx.Err() != nil {
			t.Fatalf("a sent upload stalled before the header timeout: %v", context.Cause(st.ctx))
		}
		st.receiving()
		time.Sleep(stallTimeout - time.Second)
		if st.ctx.Err() != nil {
			t.Fatal("the answer's progress timer did not restart")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if cause := context.Cause(st.ctx); !errors.Is(cause, errStalled) {
			t.Fatalf("a stalled answer: %v", cause)
		}
	})
}

// A download whose answer never starts fails after the header timeout.
func TestDownloadHeaderTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			<-r.Context().Done()
		}, nil)
		start := time.Now()
		_, err := c.Stream(t.Context(), Request{Path: "attachment/1", Transfer: true}, io.Discard, 1<<20)
		if !errors.Is(err, errStalled) || !strings.Contains(err.Error(), "no answer within 5m0s") {
			t.Fatalf("Stream = %v", err)
		}
		// A read with nothing written yet is retried: five attempts, each
		// out of time, with the backoff between them.
		if elapsed, attempts := time.Since(start), requests.Load(); attempts != 5 || elapsed != 5*headerTimeout+15*time.Second {
			t.Fatalf("%d attempts in %s", attempts, elapsed)
		}
	})
}

func TestMultipart(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"POST /issue/WEB-1/attachments": func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse: %v", err)
				return
			}
			if got := r.FormValue("comment"); got != "see attached" {
				t.Errorf("comment = %q", got)
			}
			files := r.MultipartForm.File["file"]
			if len(files) != 2 || files[0].Filename != `a "quoted".txt` || files[1].Filename != "b.png" {
				t.Errorf("files = %+v", files)
				return
			}
			f, _ := files[0].Open()
			content, _ := io.ReadAll(f)
			if string(content) != "first" {
				t.Errorf("content = %q", content)
			}
			writeString(w, `[{"id":"1"},{"id":"2"}]`)
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	body, contentType, err := Multipart(t.Context(), []Part{
		{FieldName: "file", FileName: `a "quoted".txt`, Content: strings.NewReader("first")},
		{FieldName: "file", FileName: "b.png", Content: bytes.NewReader([]byte{0x89, 'P', 'N', 'G'})},
		{FieldName: "comment", Content: strings.NewReader("see attached")},
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var out []struct{ ID string }
	if _, err := c.JSON(t.Context(), Request{Method: http.MethodPost, Path: "issue/WEB-1/attachments", Body: body, ContentType: contentType, Transfer: true}, &out); err != nil || len(out) != 2 {
		t.Fatalf("upload: %v, %+v", err, out)
	}

	if _, _, err := Multipart(t.Context(), []Part{{FieldName: "file", FileName: "big", Content: bytes.NewReader(make([]byte, 2000))}}, 1000); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("over the cap: %v", err)
	}
	if _, _, err := Multipart(t.Context(), []Part{{FieldName: "file", FileName: "big", Content: bytes.NewReader(make([]byte, 900))}}, 1000); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("over the cap with boundaries: %v", err)
	}
	broken := errors.New("disk gone")
	if _, _, err := Multipart(t.Context(), []Part{{FieldName: "file", FileName: "a", Content: io.MultiReader(strings.NewReader("x"), errReader{broken})}}, 1000); !errors.Is(err, broken) {
		t.Fatalf("a failing reader: %v", err)
	}
	for _, bad := range [][]Part{
		{{FileName: "a", Content: strings.NewReader("x")}},
		{{FieldName: "file", FileName: "a"}},
	} {
		if _, _, err := Multipart(t.Context(), bad, 1000); err == nil {
			t.Fatalf("%+v: no error", bad)
		}
	}
	if _, _, err := Multipart(t.Context(), nil, 0); err == nil {
		t.Fatal("a zero limit: no error")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := Multipart(ctx, []Part{{FieldName: "file", FileName: "a", Content: strings.NewReader("x")}}, 1000); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled build: %v", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
