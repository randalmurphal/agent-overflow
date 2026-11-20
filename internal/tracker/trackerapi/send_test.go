package trackerapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
)

// A write that dies on a reused connection before its answer is not sent
// again by net/http: the idempotency headers that would let it are
// refused, and a write's request has no GetBody to resend.
func TestWriteIsNotReplayedOnAReusedConnection(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		tr := newTracker(t, route{})
		c, _ := loopbackClient(t, testOptions(tr.url()))
		_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "issue", Header: http.Header{header: {"1"}}, Body: map[string]string{}})
		if _, ok := errors.AsType[*NotSentError](err); !ok {
			t.Errorf("%s: %v, want the header refused", header, err)
		}
	}

	tr := newTracker(t, route{
		"GET /warm":   reply(http.StatusOK),
		"POST /issue": func(w http.ResponseWriter, r *http.Request) { hijackClose(t, w, "") },
	})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	if _, err := c.Do(t.Context(), Request{Path: "warm"}); err != nil {
		t.Fatal(err)
	}
	k, err := c.prepare(Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{"summary": "s"}})
	if err != nil {
		t.Fatal(err)
	}
	// Past the header check, as a caller could not: the request alone must
	// not be replayable.
	k.header = http.Header{"Idempotency-Key": {"1"}}
	_, err = c.execute(t.Context(), k, (*Client).readBody)
	if err == nil || WriteNotApplied(err) {
		t.Fatalf("execute = %v, want an unknown outcome", err)
	}
	if got := tr.count("POST /issue"); got != 1 {
		t.Fatalf("the write was sent %d times", got)
	}
}

// A reader body is refused rather than read before the call starts.
func TestReaderBodyIsRefused(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	_, err := c.Do(t.Context(), Request{Method: http.MethodPost, Path: "upload", Body: strings.NewReader("a"), ContentType: "text/plain"})
	if _, ok := errors.AsType[*NotSentError](err); !ok {
		t.Fatalf("Do = %v, want the reader body refused", err)
	}
}

// A call whose context ended before the request was written is not sent,
// with a credential already held.
func TestCancelledBeforeSendingIsNotSent(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /me": reply(http.StatusOK), "POST /issue": reply(http.StatusCreated)})
	c, _ := loopbackClient(t, testOptions(tr.url()))
	if _, err := c.Do(t.Context(), Request{Path: "me"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := c.Do(ctx, Request{Method: http.MethodPost, Path: "issue", Body: map[string]string{}})
	if _, ok := errors.AsType[*NotSentError](err); !ok || !errors.Is(err, context.Canceled) || !WriteNotApplied(err) {
		t.Fatalf("Do = %v, want an unsent cancellation", err)
	}
	if tr.count("POST /issue") != 0 {
		t.Fatal("the cancelled write was sent")
	}
}

// The largest bounds work as bounds, and a rate the pacing cannot keep is
// refused.
func TestExtremeBounds(t *testing.T) {
	t.Parallel()
	tr := newTracker(t, route{"GET /a": func(w http.ResponseWriter, r *http.Request) { writeString(w, "hello") }})
	opts := testOptions(tr.url())
	opts.MaxBodyBytes = math.MaxInt64
	c, _ := loopbackClient(t, opts)
	for _, r := range []Request{{Path: "a"}, {Path: "a", MaxBodyBytes: math.MaxInt64}} {
		if resp, err := c.Do(t.Context(), r); err != nil || string(resp.Body) != "hello" {
			t.Fatalf("Do = %v, %q; want the whole body", err, resp.Body)
		}
	}
	var dst strings.Builder
	if _, err := c.Stream(t.Context(), Request{Path: "a"}, &dst, math.MaxInt64); err != nil || dst.String() != "hello" {
		t.Fatalf("Stream = %v, %q; want the whole body", err, dst.String())
	}
	for _, rate := range []float64{1e-12, 1e10} {
		opts := testOptions(tr.url())
		opts.MaxPerSecond = rate
		if _, err := New(opts); err == nil {
			t.Errorf("MaxPerSecond %v: accepted", rate)
		}
	}
}
