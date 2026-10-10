package forgeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testSource hands out tokens in order (the last repeats) and counts
// reads; set replaces the queue.
type testSource struct {
	mu     sync.Mutex
	tokens []string
	info   SourceInfo
	err    error
	reads  int
}

func (s *testSource) Token(_ context.Context, _, _ string) ([]byte, SourceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return nil, SourceInfo{}, s.err
	}
	token := s.tokens[0]
	if len(s.tokens) > 1 {
		s.tokens = s.tokens[1:]
	}
	return []byte(token), s.info, nil
}

func (s *testSource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *testSource) set(err error, tokens ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
	if len(tokens) > 0 {
		s.tokens = tokens
	}
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// liveService is a production-shaped Service (a token source, no
// isolation) whose bases point at srv.
func liveService(t *testing.T, src TokenSource, srv *httptest.Server) (*Service, *testClock) {
	t.Helper()
	s, err := New(Options{Version: "test", TokenSource: src})
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	s.now = clock.now
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.resolveBase = func(forge, _ string, _ SourceInfo) (*url.URL, *url.URL, error) {
		if forge == ForgeGitLab {
			return base.JoinPath("api", "v4/"), nil, nil
		}
		return base.JoinPath("rest/"), base.JoinPath("graphql"), nil
	}
	t.Cleanup(s.Close)
	return s, clock
}

func isolatedService(t *testing.T, srv *httptest.Server) *Service {
	t.Helper()
	s, err := New(Options{Version: "test", Isolated: &Isolated{BaseURL: srv.URL, Token: testSecret}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (s *seen) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r.Clone(context.Background()))
	s.body = append(s.body, body)
}

func (s *seen) last() (*http.Request, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1], s.body[len(s.body)-1]
}

func (s *seen) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func TestETagRevalidation(t *testing.T) {
	t.Parallel()
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.record(r)
		w.Header().Set("ETag", `W/"abc"`)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(4999-got.count()))
		w.Header().Set("X-RateLimit-Reset", "1700003600")
		if r.Header.Get("If-None-Match") == `W/"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("X-Next-Page", "2")
		fmt.Fprint(w, `[1,2,3]`)
	}))
	defer srv.Close()
	s := isolatedService(t, srv)
	gl := s.GitLab("gitlab.com")
	first, err := gl.Do(t.Context(), Request{Path: "projects/1/merge_requests"})
	if err != nil || first.NotModified {
		t.Fatal(err)
	}
	second, err := gl.Do(t.Context(), Request{Path: "projects/1/merge_requests"})
	if err != nil || !second.NotModified || string(second.Body) != `[1,2,3]` || second.Header.Get("X-Next-Page") != "2" {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if second.Rate == nil || second.Rate.Remaining != 4997 {
		t.Fatalf("the 304's quota was not read: %+v", second.Rate)
	}
	// Stream never stores and never consults the store.
	var dst bytes.Buffer
	streamed, err := gl.Stream(t.Context(), Request{Path: "projects/1/merge_requests"}, &dst, 1<<20)
	if err != nil || streamed.NotModified || dst.String() != `[1,2,3]` || streamed.Header.Get("ETag") != `W/"abc"` {
		t.Fatalf("Stream = %+v, %q, %v", streamed, dst.String(), err)
	}
	dst.Reset()
	revalidated, err := gl.Stream(t.Context(), Request{Path: "projects/1/merge_requests", Header: http.Header{"If-None-Match": {`W/"abc"`}}}, &dst, 1<<20)
	if err != nil || !revalidated.NotModified || dst.Len() != 0 {
		t.Fatalf("Stream with the caller's ETag = %+v, %v", revalidated, err)
	}
}

func TestRateLimitedRequestsCloseTheGate(t *testing.T) {
	t.Parallel()
	var remaining atomic.Int32
	remaining.Store(4000)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(int(remaining.Load())))
		w.Header().Set("X-RateLimit-Reset", "1700003600")
		w.Header().Set("X-RateLimit-Resource", "core")
		if r.URL.Path == "/rest/limited" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	s, clock := liveService(t, &testSource{tokens: []string{testSecret}}, srv)
	gh := s.GitHub("github.com")

	remaining.Store(100)
	if _, err := gh.Do(t.Context(), Request{Path: "user"}); err != nil {
		t.Fatal(err)
	}
	_, err := gh.Do(t.Context(), Request{Path: "user"})
	var limited *RateLimitedError
	if !errors.As(err, &limited) || !limited.Reserve || limited.Until.Unix() != 1700003600 {
		t.Fatalf("background under the reserve = %v", err)
	}
	if _, err := gh.Do(WithInteractive(t.Context()), Request{Path: "user"}); err != nil {
		t.Fatalf("interactive under the reserve refused: %v", err)
	}
	// GraphQL is another pool.
	if _, err := gh.GraphQL(t.Context(), "Viewer", "query Viewer { viewer { login } }", nil, nil); err != nil {
		t.Fatalf("graphql stalled by the core reserve: %v", err)
	}

	remaining.Store(4000)
	clock.advance(time.Hour)
	before := hits.Load()
	if _, err := gh.Do(t.Context(), Request{Path: "limited"}); !errors.As(err, &limited) || limited.Until != clock.now().Add(2*time.Minute) {
		t.Fatalf("429 = %v", err)
	}
	if _, err := gh.Do(WithInteractive(t.Context()), Request{Path: "user"}); !errors.As(err, &limited) || limited.Reserve {
		t.Fatalf("interactive through a closed gate = %v", err)
	}
	if hits.Load() != before+1 {
		t.Fatalf("a closed gate let %d requests through", hits.Load()-before-1)
	}
}

func TestPages(t *testing.T) {
	t.Parallel()
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/rest/"):
			if page < 3 {
				w.Header().Set("Link", fmt.Sprintf(`<%s/rest/repositories/1/items?page=%d>; rel="next"`, srvURL, page+1))
			}
		default:
			if page < 3 {
				w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
			} else {
				w.Header().Set("X-Next-Page", "")
			}
		}
		fmt.Fprintf(w, "[%d]", page)
	}))
	defer srv.Close()
	srvURL = srv.URL
	s, _ := liveService(t, &testSource{tokens: []string{testSecret}}, srv)
	for _, c := range []*Client{s.GitHub("github.com"), s.GitLab("gitlab.com")} {
		var bodies []string
		err := c.Pages(t.Context(), Request{Path: "repos/o/r/items", Query: url.Values{"per_page": {"1"}}}, func(r *Response) (bool, error) {
			bodies = append(bodies, string(r.Body))
			return true, nil
		})
		if err != nil || strings.Join(bodies, "") != "[1][2][3]" {
			t.Fatalf("%s pages = %v, %v", c.forge, bodies, err)
		}
		bodies = nil
		_ = c.Pages(t.Context(), Request{Path: "repos/o/r/items"}, func(r *Response) (bool, error) {
			bodies = append(bodies, string(r.Body))
			return len(bodies) < 2, nil
		})
		if len(bodies) != 2 {
			t.Fatalf("%s: the caller's cap of 2 read %d pages", c.forge, len(bodies))
		}
	}
}

func TestUnreachableForgeIsTransientAndRedacted(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	s, err := New(Options{Version: "test", Isolated: &Isolated{BaseURL: base, Token: testSecret}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.GitHub("github.com").Do(t.Context(), Request{Path: "https://private-user-images.githubusercontent.com/a?jwt=SECRET", Attachment: true})
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("err = %T %v, want *TransientError", err, err)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("error leaks a secret: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.GitHub("github.com").Do(ctx, Request{Path: "user"}); errors.As(err, &transient) || !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled call = %v, want the cancellation, not transient", err)
	}
}

func TestHostConcurrencyIsBounded(t *testing.T) {
	t.Parallel()
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
	}))
	defer srv.Close()
	s := isolatedService(t, srv)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, _ = s.GitHub("github.com").Do(t.Context(), Request{Path: "user"}) })
	}
	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < hostConcurrency && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if peak.Load() != hostConcurrency {
		t.Fatalf("peak in flight = %d, want %d", peak.Load(), hostConcurrency)
	}
}
