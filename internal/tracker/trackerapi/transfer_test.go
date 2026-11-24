package trackerapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"agent-overflow/internal/secret"
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
			body, err := Multipart([]Part{{FieldName: "file", FileName: "a.bin", Content: bytes.NewReader(make([]byte, size)), Size: int64(size)}})
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: path, Body: body, Transfer: true})
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
		body := st.upload(bytes.NewReader(make([]byte, 10)), 10)
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
		body = st.upload(bytes.NewReader(make([]byte, 10)), 10)
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

// An upload's body is mime/multipart's framing around each part's
// content, read as it is sent, with its exact length declared.
func TestMultipart(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"POST /issue/WEB-1/attachments": func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength <= 0 || len(r.TransferEncoding) > 0 {
				t.Errorf("Content-Length %d, Transfer-Encoding %v: want a declared length", r.ContentLength, r.TransferEncoding)
			}
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
			f, err := files[0].Open()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = f.Close() }()
			if content, _ := io.ReadAll(f); string(content) != "first" {
				t.Errorf("content = %q", content)
			}
			writeString(w, `[{"id":"1"},{"id":"2"}]`)
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	parts := []Part{
		{FieldName: "file", FileName: `a "quoted".txt`, Content: bytes.NewReader([]byte("first")), Size: 5},
		{FieldName: "file", FileName: "b.png", Content: bytes.NewReader([]byte{0x89, 'P', 'N', 'G'}), Size: 4},
		{FieldName: "comment", Content: bytes.NewReader([]byte("see attached")), Size: 12},
	}
	body, err := Multipart(parts)
	if err != nil {
		t.Fatal(err)
	}
	// The bytes are mime/multipart's own, as a buffered body has them.
	var want bytes.Buffer
	w := multipart.NewWriter(&want)
	if err := w.SetBoundary(body.contentType[strings.Index(body.contentType, "boundary=")+len("boundary="):]); err != nil {
		t.Fatal(err)
	}
	for _, content := range []struct{ field, file, text string }{{"file", `a "quoted".txt`, "first"}, {"file", "b.png", "\x89PNG"}, {"comment", "", "see attached"}} {
		var dst io.Writer
		if content.file != "" {
			dst, err = w.CreateFormFile(content.field, content.file)
		} else {
			dst, err = w.CreateFormField(content.field)
		}
		if err != nil {
			t.Fatal(err)
		}
		writeString(dst, content.text)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if body.Size() != int64(want.Len()) {
		t.Fatalf("size %d, want %d", body.Size(), want.Len())
	}
	// Seekable contents are read again from their start for each attempt.
	for attempt := range 2 {
		sent, err := body.open()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := io.ReadAll(sent); err != nil || !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("attempt %d sent %q, %v; want %q", attempt, got, err, want.Bytes())
		}
	}
	var out []struct{ ID string }
	if _, err := c.JSON(t.Context(), Request{Method: http.MethodPost, Path: "issue/WEB-1/attachments", Body: body, Transfer: true}, &out); err != nil || len(out) != 2 {
		t.Fatalf("upload: %v, %+v", err, out)
	}
	if _, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue/WEB-1/attachments", Body: body, ContentType: "text/plain", Transfer: true}); err == nil {
		t.Fatal("an Upload with a content type of the caller's: no error")
	}
	for _, bad := range [][]Part{
		{{FileName: "a", Content: strings.NewReader("x"), Size: 1}},
		{{FieldName: "file", FileName: "a", Size: 1}},
		{{FieldName: "file", FileName: "a", Content: strings.NewReader("x"), Size: -1}},
	} {
		if _, err := Multipart(bad); err == nil {
			t.Fatalf("%+v: no error", bad)
		}
	}
}

// A content that fails, ends short of its size or runs past it while it is
// sent stops the request short of its declared length: the tracker never
// had the whole request, so the write was not applied (*SourceError),
// never unknown. Nothing is sent again.
func TestUploadSourceFailureIsNotApplied(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	tr := newTracker(t, route{
		"POST /upload": func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				return
			}
			writeString(w, `{}`)
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	broken := errors.New("the client went away")
	for _, tc := range []struct {
		name    string
		content io.Reader
		size    int64
		want    string
	}{
		{"a failing content", io.MultiReader(strings.NewReader("abc"), errReader{broken}), 10, "the client went away"},
		{"a content shorter than its size", strings.NewReader("abc"), 10, "7 bytes short"},
		{"a content longer than its size", strings.NewReader("abcdef"), 3, "more than its declared size"},
	} {
		body, err := Multipart([]Part{{FieldName: "file", FileName: "a", Content: tc.content, Size: tc.size}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: "upload", Body: body, Transfer: true})
		source, ok := errors.AsType[*SourceError](err)
		if !ok || !WriteNotApplied(err) || !strings.Contains(source.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if n := requests.Load(); n > 3 {
		t.Errorf("%d requests for three uploads: one was sent again", n)
	}
}

// A body read once is never sent again: not after a refused credential
// that was replaced, which sends a seekable body again, and the call
// answers with the refusal.
func TestUploadReadOnceIsSentOnce(t *testing.T) {
	t.Parallel()
	var requests, limits atomic.Int32
	tr := newTracker(t, route{
		"POST /upload": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if requests.Add(1)%2 == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeString(w, `{}`)
		},
		"POST /limited": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			limits.Add(1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		},
	})
	opts := testOptions(tr.url())
	reads := 0
	opts.Credentials = sourceFunc(func(context.Context) (*Credential, error) {
		reads++
		return Bearer(secret.New([]byte(fmt.Sprintf("%s-%d", testSecret, reads)))), nil
	})
	c, _ := loopbackClient(t, opts)
	send := func(content io.Reader) error {
		body, err := Multipart([]Part{{FieldName: "file", FileName: "a", Content: content, Size: 3}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Do(t.Context(), Request{Method: http.MethodPost, Path: "upload", Body: body, Transfer: true})
		return err
	}
	if err := send(bytes.NewReader([]byte("abc"))); err != nil || requests.Load() != 2 {
		t.Fatalf("a seekable body after a replaced credential: %v after %d requests", err, requests.Load())
	}
	// The refusal is the answer, not a failure to send the body again.
	err := send(io.MultiReader(strings.NewReader("abc")))
	if _, refused := errors.AsType[*AuthError](err); !refused || requests.Load() != 3 {
		t.Fatalf("a body read once after a refusal: %v after %d requests", err, requests.Load())
	}
	// An interactive write goes again after a 429, but not one read once
	// (io.MultiReader hides the strings.Reader's Seek).
	body, err := Multipart([]Part{{FieldName: "file", FileName: "a", Content: io.MultiReader(strings.NewReader("abc")), Size: 3}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(WithInteractive(t.Context()), Request{Method: http.MethodPost, Path: "limited", Body: body, Transfer: true})
	if _, limited := errors.AsType[*RateLimitedError](err); !limited || limits.Load() != 1 {
		t.Fatalf("a body read once after a rate limit: %v after %d requests", err, limits.Load())
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
