//go:build !noremote

package deviceclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// sentDeadlines records the deadline each credential request was sent with.
type sentDeadlines struct {
	mu   sync.Mutex
	sent []sentDeadline
}

type sentDeadline struct {
	path     string
	deadline time.Time
	ok       bool
	at       time.Time
}

func (d *sentDeadlines) wrap(next http.RoundTripper) http.RoundTripper {
	return roundTripper(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		d.mu.Lock()
		d.sent = append(d.sent, sentDeadline{path: req.URL.Path, deadline: deadline, ok: ok, at: time.Now()})
		d.mu.Unlock()
		return next.RoundTrip(req)
	})
}

// since returns the requests sent after the first n.
func (d *sentDeadlines) since(n int) []sentDeadline {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sentDeadline(nil), d.sent[n:]...)
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// carries reports whether s was sent with a deadline bound after its
// exchange began: no earlier than began plus bound, and no later than the
// moment it was sent plus bound. Both ends are clock reads around the moment
// the deadline was set, so the check needs no allowance for a slow machine.
func (s sentDeadline) carries(bound time.Duration, began time.Time) error {
	if !s.ok {
		return fmt.Errorf("%s was sent without a deadline, want %v", s.path, bound)
	}
	if s.deadline.Before(began.Add(bound)) || s.deadline.After(s.at.Add(bound)) {
		return fmt.Errorf("%s was sent with its deadline %v after its exchange began and %v after it was sent, want %v", s.path, s.deadline.Sub(began), s.deadline.Sub(s.at), bound)
	}
	return nil
}

// The redemption bound reaches the redemption, and nothing after it: the
// client the redemption leaves, and a client Open builds with the same
// option, send every later exchange within pinTimeout. A redemption without
// the option is sent within pinTimeout, which is 10 s.
func TestTheRedemptionBoundIsTheRedemptionsAlone(t *testing.T) {
	t.Parallel()
	if pinTimeout != 10*time.Second {
		t.Fatalf("pinTimeout = %v, want 10s", pinTimeout)
	}
	hour := func() (time.Duration, error) { return time.Hour, nil }
	for _, c := range []struct {
		name  string
		opts  []Option
		bound time.Duration
	}{
		{name: "with the option", opts: []Option{WithRedemptionBound(hour)}, bound: time.Hour},
		{name: "without it", bound: pinTimeout},
	} {
		h := newHeldRedemption(t)
		close(h.release)
		sent := &sentDeadlines{}
		opts := append([]Option{withRoundTrip(sent.wrap)}, c.opts...)
		dir := t.TempDir()
		began := time.Now()
		client, _, err := Pair(context.Background(), dir, h.link, "test", "test", opts...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Cleanup(client.http.CloseIdleConnections)
		redemption := sent.since(0)
		if len(redemption) != 1 || redemption[0].path != authPairPath {
			t.Fatalf("%s: sent %+v, want the one redemption", c.name, redemption)
		}
		if err := redemption[0].carries(c.bound, began); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}

		opened, err := Open(dir, client.Session(), opts...)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		t.Cleanup(opened.http.CloseIdleConnections)
		for _, later := range []*Client{client, opened} {
			n := len(sent.since(0))
			began := time.Now()
			_, _ = later.Ticket(context.Background()) // A pending pairing refuses it once sent.
			after := sent.since(n)
			if len(after) == 0 {
				t.Fatalf("%s: a ticket mint sent nothing", c.name)
			}
			for _, s := range after {
				if err := s.carries(pinTimeout, began); err != nil {
					t.Fatalf("%s: after the redemption: %v", c.name, err)
				}
			}
		}
	}
}

// A redemption whose bound has no time left, or answers a non-positive
// bound, is refused before it is sent.
func TestARedemptionWithNoBoundLeftIsNotSent(t *testing.T) {
	t.Parallel()
	exhausted := errors.New("no time left in this run")
	for _, c := range []struct {
		name  string
		bound func() (time.Duration, error)
		want  string
	}{
		{name: "an exhausted budget", bound: func() (time.Duration, error) { return 0, exhausted }, want: "it has no bound: no time left in this run"},
		{name: "a zero bound", bound: func() (time.Duration, error) { return 0, nil }, want: "its bound 0s is not positive"},
		{name: "a negative bound", bound: func() (time.Duration, error) { return -time.Second, nil }, want: "its bound -1s is not positive"},
	} {
		h := newHeldRedemption(t)
		close(h.release)
		_, err := Redeem(context.Background(), t.TempDir(), h.link, "test", "test", WithRedemptionBound(c.bound))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: Redeem = %v, want %q", c.name, err, c.want)
		}
		if c.name == "an exhausted budget" && !errors.Is(err, exhausted) {
			t.Fatalf("%s: Redeem = %v, want it to wrap the budget's error", c.name, err)
		}
		if n := h.redeemed.Load(); n != 0 {
			t.Fatalf("%s: the backend saw %d redemptions, want none", c.name, n)
		}
	}
}

// withRoundTrip wraps the credential transport, so this package's tests can
// read the deadline each exchange is sent with.
func withRoundTrip(wrap func(http.RoundTripper) http.RoundTripper) Option {
	return func(o *options) { o.roundTrip = wrap }
}
