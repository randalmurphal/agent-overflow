package trackerapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/secret"
)

const (
	testSecret  = "fake-token-1"
	testSecret2 = "fake-token-2"
)

// testClock is the client's injected clock: sleep advances it and records
// the wait instead of sleeping.
type testClock struct {
	mu    sync.Mutex
	t     time.Time
	waits []time.Duration
}

func newTestClock() *testClock { return &testClock{t: time.Unix(1_800_000_000, 0)} }

func (k *testClock) now() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.t
}

func (k *testClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.t = k.t.Add(d)
	k.waits = append(k.waits, d)
	return nil
}

func (k *testClock) slept() []time.Duration {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]time.Duration(nil), k.waits...)
}

// testSource hands out Bearer credentials of secrets in turn, the last
// one again once they run out, and keeps every credential it handed out.
type testSource struct {
	mu      sync.Mutex
	secrets []string
	err     error
	reads   int
	// backing holds the bytes of every credential handed out, which the
	// client zeroes in place.
	backing [][]byte
	// gate, when set, holds each read until it is closed.
	gate chan struct{}
	// entered, when set, is told of each read as it starts.
	entered chan struct{}
}

func (s *testSource) Credential(ctx context.Context) (*Credential, error) {
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	value := s.secrets[min(s.reads-1, len(s.secrets)-1)]
	cred := Bearer(secret.New([]byte(value)))
	s.backing = append(s.backing, cred.secret.Reveal())
	return cred, nil
}

func (s *testSource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *testSource) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// zeroed reports whether the n-th credential handed out was zeroed.
func (s *testSource) zeroed(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return isZero(s.backing[n])
}

func isZero(b []byte) bool { return len(b) > 0 && bytes.Count(b, []byte{0}) == len(b) }

// route answers one request, "METHOD /path".
type route = map[string]http.HandlerFunc

// tracker is a loopback test server that answers only the routes a test
// names and fails the test on any other request.
type tracker struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits map[string]int
	reqs []*http.Request
}

func newTracker(t *testing.T, routes route) *tracker {
	t.Helper()
	tr := &tracker{hits: make(map[string]int)}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		tr.mu.Lock()
		tr.hits[key]++
		tr.reqs = append(tr.reqs, r.Clone(context.Background()))
		tr.mu.Unlock()
		handler, ok := routes[key]
		if !ok {
			t.Errorf("unexpected request %s", key)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *tracker) count(key string) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.hits[key]
}

func (tr *tracker) last() *http.Request {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.reqs[len(tr.reqs)-1]
}

func (tr *tracker) url() string { return tr.srv.URL + "/" }

// testOptions is a valid Options on base with a static credential.
func testOptions(base string) Options {
	return Options{
		Name:        "jira",
		BaseURL:     base,
		Version:     "test",
		Credentials: StaticCredentials(Bearer(secret.New([]byte(testSecret)))),
		Dialect:     PlainDialect{},
	}
}

// loopbackClient is a production Client (New, its own transport) on a
// loopback server, with the test clock.
func loopbackClient(t *testing.T, opts Options) (*Client, *testClock) {
	t.Helper()
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	clock := newTestClock()
	c.now, c.sleep = clock.now, clock.sleep
	t.Cleanup(c.Close)
	return c, clock
}

// memoryClient is a Client on an in-memory test server, for a test inside
// a synctest bubble: every host routes to handler, and time is the
// bubble's.
func memoryClient(t *testing.T, handler http.HandlerFunc, configure func(*Options)) *Client {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	opts := testOptions("https://example.atlassian.net/")
	if configure != nil {
		configure(&opts)
	}
	c, err := newClient(opts, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func writeString(w io.Writer, s string) { _, _ = io.WriteString(w, s) }

// hang reads the request body, so the server notices the client leave,
// and waits until it does.
func hang(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}
