package trackerapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"agent-overflow/internal/secret"
)

// reports records what OnAnswer was told.
type reports struct {
	mu       sync.Mutex
	statuses []int
	headers  []http.Header
}

func (r *reports) record(status int, header http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, status)
	r.headers = append(r.headers, header)
}

func (r *reports) seen() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.statuses)
}

// OnAnswer is told every answer whose headers arrive: a success, an
// error, each retry attempt and each redirect hop, with a copy of its
// headers.
func TestOnAnswerReportsEveryAnswer(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /ok":       reply(http.StatusOK, "X-Budget", "10"),
		"GET /missing":  reply(http.StatusNotFound),
		"GET /flaky":    failing(1, reply(http.StatusServiceUnavailable), reply(http.StatusOK)),
		"GET /moved":    func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) },
		"POST /limited": reply(http.StatusTooManyRequests, "Retry-After", "5"),
	})
	var got reports
	opts := testOptions(tr.url())
	opts.OnAnswer = got.record
	opts.ReportHeaders = []string{"x-budget"}
	c, _ := loopbackClient(t, opts)
	resp, err := c.Do(t.Context(), Request{Path: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if got.headers[0].Get("X-Budget") != "10" {
		t.Fatalf("reported %v, want X-Budget as sent", got.headers[0])
	}
	got.headers[0].Set("X-Budget", "changed")
	if resp.Header.Get("X-Budget") != "10" {
		t.Fatal("the reported header is the Response's own map")
	}
	_, _ = c.Do(t.Context(), Request{Path: "missing"})
	_, _ = c.Do(t.Context(), Request{Path: "flaky"})
	_, _ = c.Do(t.Context(), Request{Path: "moved"})
	_, _ = c.Do(t.Context(), Request{Method: http.MethodPost, Path: "limited", Body: map[string]string{}})
	want := []int{200, 404, 503, 200, 302, 200, 429}
	if !slices.Equal(got.seen(), want) {
		t.Fatalf("reported %v, want %v", got.seen(), want)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	var none reports
	opts = testOptions(closed.URL)
	opts.OnAnswer = none.record
	c, _ = loopbackClient(t, opts)
	if _, err := c.Do(t.Context(), Request{Path: "ok"}); err == nil || len(none.seen()) != 0 {
		t.Fatalf("Do = %v with %v reported, want nothing reported when nothing answered", err, none.seen())
	}
}

// An answer is reported when its headers arrive, before its body does.
func TestOnAnswerBeforeTheBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		reported := make(chan int, 1)
		c := memoryClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			<-release
			writeString(w, `{}`)
		}, func(o *Options) { o.OnAnswer = func(status int, _ http.Header) { reported <- status } })
		start := time.Now()
		done := make(chan error)
		go func() {
			_, err := c.Do(t.Context(), Request{Path: "a"})
			done <- err
		}()
		if status := <-reported; status != http.StatusOK || time.Since(start) != 0 {
			t.Fatalf("reported %d after %s, want the 200 as its headers arrive", status, time.Since(start))
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// Concurrent calls each report their answer.
func TestOnAnswerUnderConcurrentCalls(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /a": reply(http.StatusOK)})
	var got reports
	opts := testOptions(tr.url())
	opts.OnAnswer = got.record
	c, _ := loopbackClient(t, opts)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if _, err := c.Do(t.Context(), Request{Path: "a", Query: map[string][]string{"n": {strconv.Itoa(i)}}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := len(got.seen()); n != 20 {
		t.Fatalf("%d reports, want 20", n)
	}
}

// roundTripFunc is a transport made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// An answer whose headers arrive after Close returned is not reported.
func TestOnAnswerNotAfterClose(t *testing.T) {
	t.Parallel()
	var got reports
	var c *Client
	opts := testOptions("https://example.atlassian.net/")
	opts.OnAnswer = got.record
	c, err := newClient(opts, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		c.Close()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(t.Context(), Request{Path: "a"})
	if !errors.Is(err, errClosed) && !errors.Is(err, context.Canceled) {
		t.Logf("Do after Close: %v", err)
	}
	if s := got.seen(); len(s) != 0 {
		t.Fatalf("reported %v after Close returned", s)
	}
}

// The report has no Scrub of its own. It holds only the headers the
// owner named, so no header name is the tracker's and no URL-bearing
// header arrives unasked, and every value is scrubbed.
func TestOnAnswerHeadersAreSafe(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{
		"GET /content": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://media.example.com/file/1/binary?token=signed")
			w.Header().Set("Link", "<https://media.example.com/file?token=signed>; rel=\"next\"")
			w.Header().Set("X-Name-"+testSecret, "present")
			w.Header().Set("X-Echo", "you sent "+r.Header.Get("Authorization"))
			w.Header().Set("X-Budget", "10")
			w.WriteHeader(http.StatusSeeOther)
		},
	})
	var got reports
	opts := testOptions(tr.url())
	opts.OnAnswer = got.record
	opts.ReportHeaders = []string{"X-Echo", "X-Budget", "X-Absent"}
	c, _ := loopbackClient(t, opts)
	if _, err := c.Location(t.Context(), Request{Path: "content"}); err != nil {
		t.Fatal(err)
	}
	header := got.headers[0]
	if len(header) != 2 {
		t.Errorf("the report holds %v, want only the two named headers that were sent", header)
	}
	for name, values := range header {
		if text := name + strings.Join(values, ""); strings.Contains(text, testSecret) || strings.Contains(text, "signed") {
			t.Errorf("the report holds %s: %v", name, values)
		}
	}
	if echo := header.Get("X-Echo"); !strings.Contains(echo, secret.Redacted) {
		t.Errorf("X-Echo = %q, want the credential redacted", echo)
	}
	if header.Get("X-Budget") != "10" {
		t.Errorf("X-Budget = %q, want it as sent", header.Get("X-Budget"))
	}
}
