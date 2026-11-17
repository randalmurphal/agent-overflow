package trackerapi

import (
	"bytes"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

func TestStreamLimit(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("b", 100)
	tr := newTracker(t, route{
		"GET /sized": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.Header().Set("Content-Type", "image/png")
			writeString(w, payload)
		},
		"GET /chunked": func(w http.ResponseWriter, r *http.Request) {
			writeString(w, payload[:50])
			_ = http.NewResponseController(w).Flush()
			writeString(w, payload[50:])
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	for _, path := range []string{"sized", "chunked"} {
		var dst bytes.Buffer
		resp, err := c.Stream(t.Context(), Request{Path: path}, &dst, 100)
		if err != nil || dst.String() != payload || resp.Body != nil || resp.Status != http.StatusOK {
			t.Fatalf("%s at the limit: %v, %d bytes", path, err, dst.Len())
		}
		if req := tr.last(); req.Header.Get("Accept") != "" {
			t.Fatalf("%s: Stream sent Accept %q", path, req.Header.Get("Accept"))
		}
		dst.Reset()
		_, err = c.Stream(t.Context(), Request{Path: path}, &dst, 60)
		if tooLarge, ok := errors.AsType[*BodyTooLargeError](err); !ok || tooLarge.Limit != 60 {
			t.Fatalf("%s over the limit: %v", path, err)
		}
		want := 60
		if path == "sized" {
			want = 0 // refused on its length, before a byte is written
		}
		if dst.Len() != want {
			t.Fatalf("%s over the limit wrote %d bytes, want %d", path, dst.Len(), want)
		}
	}
}

// A download is retried until its first byte reaches the destination,
// and never after.
func TestStreamRetriesOnlyBeforeItWrites(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /flaky": failing(1, reply(http.StatusServiceUnavailable), func(w http.ResponseWriter, r *http.Request) { writeString(w, "data") }),
		"GET /cut": func(w http.ResponseWriter, r *http.Request) {
			hijackClose(t, w, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
		},
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	var dst bytes.Buffer
	if _, err := c.Stream(t.Context(), Request{Path: "flaky"}, &dst, 1<<10); err != nil || dst.String() != "data" || tr.count("GET /flaky") != 2 {
		t.Fatalf("a 503 then data: %v, %q, %d requests", err, dst.String(), tr.count("GET /flaky"))
	}
	dst.Reset()
	_, err := c.Stream(t.Context(), Request{Path: "cut"}, &dst, 1<<10)
	if _, ok := errors.AsType[*TransientError](err); !ok || tr.count("GET /cut") != 1 || dst.String() != "partial" {
		t.Fatalf("cut short: %v, %d requests, %q written", err, tr.count("GET /cut"), dst.String())
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// A destination that fails ends the call with its own error, not a
// transient one, and the call is not sent again.
func TestStreamDestinationFailure(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /file": func(w http.ResponseWriter, r *http.Request) { writeString(w, "data") }})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	full := errors.New("disk full")
	_, err := c.Stream(t.Context(), Request{Path: "file"}, failingWriter{full}, 1<<10)
	if !errors.Is(err, full) || tr.count("GET /file") != 1 {
		t.Fatalf("Stream = %v after %d requests", err, tr.count("GET /file"))
	}
	if _, ok := errors.AsType[*TransientError](err); ok {
		t.Fatalf("a failing destination is not transient: %v", err)
	}
}

func TestLocation(t *testing.T) {
	t.Parallel()
	media := newTracker(t, route{})
	tr := newTracker(t, route{
		"GET /attachment/content/1": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", media.srv.URL+"/file/1?token=signed")
			w.WriteHeader(http.StatusSeeOther)
		},
		"GET /attachment/thumbnail/1": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "../../file/thumb")
			w.WriteHeader(http.StatusFound)
		},
		"GET /attachment/content/2":   reply(http.StatusOK),
		"GET /attachment/content/404": reply(http.StatusNotFound),
		"GET /attachment/content/limited": failing(1, reply(http.StatusTooManyRequests, "Retry-After", "1"), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/file/3", http.StatusFound)
		}),
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	got, err := c.Location(t.Context(), Request{Path: "attachment/content/1"})
	if err != nil || got != media.srv.URL+"/file/1?token=signed" {
		t.Fatalf("Location = %q, %v", got, err)
	}
	if got, err := c.Location(t.Context(), Request{Path: "attachment/thumbnail/1"}); err != nil || got != tr.srv.URL+"/file/thumb" {
		t.Fatalf("relative Location = %q, %v", got, err)
	}
	if got, err := c.Location(t.Context(), Request{Path: "attachment/content/2"}); err == nil {
		t.Fatalf("a 200: %q, want an error", got)
	}
	if _, err := c.Location(t.Context(), Request{Path: "attachment/content/404"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a 404: %v", err)
	}
	if got, err := c.Location(WithInteractive(t.Context()), Request{Path: "attachment/content/limited"}); err != nil || got != tr.srv.URL+"/file/3" {
		t.Fatalf("after a 429: %q, %v", got, err)
	}
}

// A redirect target that repeats the credential the request carried is
// refused, as written and under one or two layers of percent-encoding,
// in the path and in the query; so is any target once the credential is
// gone. Nothing of it reaches the error.
func TestLocationRefusesATargetWithTheCredential(t *testing.T) {
	t.Parallel()
	// "%66" is "f", the first letter of testSecret; "%2566" decodes to it.
	once := "%66" + testSecret[1:]
	twice := "%2566" + testSecret[1:]
	targets := map[string]string{
		"plain":         "https://media.example.com/file/" + testSecret + "/binary?token=signed",
		"encoded":       "https://media.example.com/file/" + once + "/binary",
		"encoded twice": "https://media.example.com/file/" + twice + "/binary",
		"query":         "https://media.example.com/file/1/binary?token=" + once,
	}
	routes := route{"GET /clean": func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://media.example.com/file/1/binary?token=signed")
		w.WriteHeader(http.StatusSeeOther)
	}}
	for name, target := range targets {
		routes["GET /"+strings.ReplaceAll(name, " ", "-")] = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusSeeOther)
		}
	}
	tr := newTracker(t, routes)
	c, _ := loopbackClient(t, testOptions(tr.url()))
	for name := range targets {
		got, err := c.Location(t.Context(), Request{Path: strings.ReplaceAll(name, " ", "-")})
		if err == nil || got != "" {
			t.Errorf("%s: Location = %q, %v, want an error and no target", name, got, err)
			continue
		}
		if text := err.Error(); strings.Contains(text, testSecret) || strings.Contains(text, testSecret[1:]) {
			t.Errorf("%s: the error repeats the target: %v", name, err)
		}
	}
	if got, err := c.Location(t.Context(), Request{Path: "clean"}); err != nil || !strings.HasSuffix(got, "?token=signed") {
		t.Fatalf("a clean target: %q, %v", got, err)
	}
}

// A target cannot be checked once the client dropped the credential its
// request carried, so it is refused.
func TestLocationRefusedOnceTheCredentialIsGone(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	tr := newTracker(t, route{"GET /clean": func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Location", "https://media.example.com/file/1/binary")
		w.WriteHeader(http.StatusSeeOther)
	}})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	done := make(chan error, 1)
	go func() {
		_, err := c.Location(t.Context(), Request{Path: "clean"})
		done <- err
	}()
	for tr.count("GET /clean") == 0 {
		runtime.Gosched()
	}
	c.ForgetCredential()
	close(release)
	if err := <-done; err == nil {
		t.Fatal("Location returned a target it could not check")
	}
}
