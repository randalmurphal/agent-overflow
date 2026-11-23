package trackerapi

import (
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// failing answers the first n requests with fail, then with ok.
func failing(n int32, fail, ok http.HandlerFunc) http.HandlerFunc {
	var seen atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if seen.Add(1) <= n {
			fail(w, r)
			return
		}
		ok(w, r)
	}
}

func reply(status int, header ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		writeString(w, `{}`)
	}
}

func seconds(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, s := range n {
		out[i] = time.Duration(s) * time.Second
	}
	return out
}

func TestReadRetries(t *testing.T) {
	t.Parallel()
	reset := func(w http.ResponseWriter, r *http.Request) { hijackClose(t, w, "") }
	ok := reply(http.StatusOK)
	for _, tc := range []struct {
		name   string
		r      Request
		routes route
		key    string
		calls  int
		waits  []time.Duration
		ok     bool
	}{
		{"connection failure", Request{Path: "a"}, route{"GET /a": failing(2, reset, ok)}, "GET /a", 3, seconds(1, 2), true},
		{"connection failure, bound", Request{Path: "a"}, route{"GET /a": reset}, "GET /a", 5, seconds(1, 2, 4, 8), false},
		{"503 with Retry-After", Request{Path: "a"}, route{"GET /a": failing(2, reply(503, "Retry-After", "3"), ok)}, "GET /a", 3, seconds(3, 3), true},
		{"503 backoff, bound", Request{Path: "a"}, route{"GET /a": reply(503)}, "GET /a", 5, seconds(1, 2, 4, 8), false},
		{"503 wait capped", Request{Path: "a", Timeout: 5 * time.Minute}, route{"GET /a": failing(1, reply(503, "Retry-After", "600"), ok)}, "GET /a", 2, seconds(60), true},
		{"HEAD", Request{Method: http.MethodHead, Path: "a"}, route{"HEAD /a": failing(1, reply(503), ok)}, "HEAD /a", 2, seconds(1), true},
		{"ReadOnly POST", Request{Method: http.MethodPost, Path: "search", ReadOnly: true, Body: map[string]string{"jql": "x"}},
			route{"POST /search": failing(1, reset, ok)}, "POST /search", 2, seconds(1), true},
	} {
		tr := newTracker(t, tc.routes)
		c, clock := loopbackClient(t, testOptions(tr.url()))
		_, err := c.Do(t.Context(), tc.r)
		if (err == nil) != tc.ok {
			t.Errorf("%s: Do = %v", tc.name, err)
		}
		if got := tr.count(tc.key); got != tc.calls {
			t.Errorf("%s: %d requests, want %d", tc.name, got, tc.calls)
		}
		if got := clock.slept(); !slices.Equal(got, tc.waits) {
			t.Errorf("%s: waited %v, want %v", tc.name, got, tc.waits)
		}
	}
}

// A 503 whose Retry-After outlasts the call fails at once instead of
// waiting into the deadline.
func TestRetryWaitPastTheDeadlineFailsAtOnce(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /a": reply(503, "Retry-After", "30")})
	c, clock := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(t.Context(), Request{Path: "a", Timeout: 10 * time.Second})
	if status, ok := errors.AsType[*StatusError](err); !ok || status.Status != 503 {
		t.Fatalf("Do = %v, want the 503", err)
	}
	if tr.count("GET /a") != 1 || len(clock.slept()) != 0 {
		t.Fatalf("%d requests, waits %v", tr.count("GET /a"), clock.slept())
	}
}

// A write is sent once whatever fails after it left: a reset, an answer
// cut short, a 5xx and a 503 alike. None of them proves it not applied.
func TestWritesAreNeverReplayed(t *testing.T) {
	t.Parallel()
	failures := map[string]http.HandlerFunc{
		"reset": func(w http.ResponseWriter, r *http.Request) { hijackClose(t, w, "") },
		"cut 2xx": func(w http.ResponseWriter, r *http.Request) {
			hijackClose(t, w, "HTTP/1.1 200 OK\r\nContent-Length: 50\r\n\r\n{\"id\":")
		},
		"500": reply(http.StatusInternalServerError),
		"502": reply(http.StatusBadGateway),
		"503": reply(http.StatusServiceUnavailable, "Retry-After", "1"),
		"504": reply(http.StatusGatewayTimeout),
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for name, fail := range failures {
			tr := newTracker(t, route{method + " /issue/WEB-1": fail})
			c, clock := loopbackClient(t, testOptions(tr.url()))
			_, err := c.Do(WithInteractive(t.Context()), Request{Method: method, Path: "issue/WEB-1", Body: map[string]string{"summary": "s"}})
			if err == nil || WriteNotApplied(err) {
				t.Errorf("%s %s: %v, want an unconfirmed failure", method, name, err)
			}
			if got := tr.count(method + " /issue/WEB-1"); got != 1 || len(clock.slept()) != 0 {
				t.Errorf("%s %s: sent %d times, waited %v", method, name, got, clock.slept())
			}
		}
	}
}

// A 429 is answered before anything is applied: an interactive call is
// sent again after waiting, a write included; a background call fails at
// once with a refusal that proves a write not applied.
func TestRateLimitRetries(t *testing.T) {
	t.Parallel()
	limited := reply(http.StatusTooManyRequests, "Retry-After", "2")
	created := reply(http.StatusCreated)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := Request{Method: method, Path: "issue"}
		if method == http.MethodPost {
			r.Body = map[string]string{"summary": "s"}
		}
		key := method + " /issue"

		tr := newTracker(t, route{key: failing(1, limited, created)})
		c, clock := loopbackClient(t, testOptions(tr.url()))
		if _, err := c.Do(WithInteractive(t.Context()), r); err != nil {
			t.Fatalf("%s interactive: %v", method, err)
		}
		if tr.count(key) != 2 || !slices.Equal(clock.slept(), seconds(2)) {
			t.Fatalf("%s interactive: %d requests, waits %v", method, tr.count(key), clock.slept())
		}

		tr = newTracker(t, route{key: failing(1, limited, created)})
		c, clock = loopbackClient(t, testOptions(tr.url()))
		start := clock.now()
		_, err := c.Do(t.Context(), r)
		rate, ok := errors.AsType[*RateLimitedError](err)
		if !ok || rate.Status != http.StatusTooManyRequests || !rate.Until.Equal(start.Add(2*time.Second)) || !WriteNotApplied(err) {
			t.Fatalf("%s background: %v", method, err)
		}
		if tr.count(key) != 1 || len(clock.slept()) != 0 {
			t.Fatalf("%s background: %d requests, waits %v", method, tr.count(key), clock.slept())
		}
	}
}

func TestInteractiveRateLimitRetryBound(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /a": reply(http.StatusTooManyRequests, "Retry-After", "1")})
	c, clock := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(WithInteractive(t.Context()), Request{Path: "a"})
	if rate, ok := errors.AsType[*RateLimitedError](err); !ok || rate.Status != http.StatusTooManyRequests {
		t.Fatalf("Do = %v", err)
	}
	if tr.count("GET /a") != 5 || !slices.Equal(clock.slept(), seconds(1, 1, 1, 1)) {
		t.Fatalf("%d requests, waits %v; want 5 and four waits", tr.count("GET /a"), clock.slept())
	}
}

// A 429 without Retry-After closes the gate for the backoff; an HTTP date
// is read against the client's clock; a dialect's limit names its own
// time. A dialect's limit that is not a 429 is not sent again for a write.
func TestRateLimitTimes(t *testing.T) {
	t.Parallel()
	clockStart := newTestClock().now()
	tr := newTracker(t, route{
		"GET /bare": reply(http.StatusTooManyRequests),
		"GET /date": reply(http.StatusTooManyRequests, "Retry-After", clockStart.Add(7*time.Second).UTC().Format(http.TimeFormat)),
		"GET /huge": reply(http.StatusTooManyRequests, "Retry-After", "9223372036854775807"),
		"GET /dialect": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Limit-Until", "1800000030")
			w.WriteHeader(http.StatusOK)
		},
		"POST /comment": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			writeString(w, `{"message":"rate limit exceeded"}`)
		},
	})
	opts := testOptions(tr.url())
	opts.Dialect = testDialect{}
	for path, want := range map[string]time.Duration{"bare": time.Second, "date": 7 * time.Second, "dialect": 30 * time.Second, "huge": 24 * time.Hour} {
		c, clock := loopbackClient(t, opts)
		_, err := c.Do(t.Context(), Request{Path: path})
		if rate, ok := errors.AsType[*RateLimitedError](err); !ok || !rate.Until.Equal(clock.now().Add(want)) {
			t.Errorf("%s: %v, want the gate closed for %s", path, err, want)
		}
	}
	c, _ := loopbackClient(t, opts)
	_, err := c.Do(WithInteractive(t.Context()), Request{Method: http.MethodPost, Path: "comment", Body: map[string]string{}})
	if rate, ok := errors.AsType[*RateLimitedError](err); !ok || rate.Status != http.StatusForbidden || !WriteNotApplied(err) {
		t.Fatalf("write on a dialect limit: %v", err)
	}
	if tr.count("POST /comment") != 1 {
		t.Fatalf("write on a dialect limit sent %d times", tr.count("POST /comment"))
	}
}
