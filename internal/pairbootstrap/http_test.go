package pairbootstrap

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The client carries its bound for the exchange and for one answer: zero
// keeps the product's 15 s and 10 s, a set bound replaces both, and a
// negative one is refused.
func TestTheClientCarriesTheExchangesBound(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		bound, exchange, answer time.Duration
	}{
		{bound: 0, exchange: 15 * time.Second, answer: 10 * time.Second},
		{bound: 2 * time.Minute, exchange: 2 * time.Minute, answer: 2 * time.Minute},
	} {
		hc, err := NewHTTPClient(nil, c.bound)
		if err != nil {
			t.Fatal(err)
		}
		answer := hc.Transport.(*http.Transport).ResponseHeaderTimeout
		if hc.Timeout != c.exchange || answer != c.answer {
			t.Fatalf("a %v bound gives the exchange %v and one answer %v, want %v and %v", c.bound, hc.Timeout, answer, c.exchange, c.answer)
		}
	}
	if _, err := NewHTTPClient(nil, -time.Second); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("a negative bound = %v, want it refused", err)
	}
}

// Exchange sends its first request with the deadline its client's Timeout
// sets for the whole exchange, or exchangeTimeout for a client without one,
// and refuses a client with a negative Timeout before it sends anything.
func TestExchangeBoundsTheWholeExchangeByItsClient(t *testing.T) {
	t.Parallel()
	stop := errors.New("stopped after reading the deadline")
	for _, c := range []struct {
		timeout, want time.Duration
	}{
		{timeout: time.Hour, want: time.Hour},
		{timeout: 0, want: exchangeTimeout},
	} {
		var deadline, sent time.Time
		var ok bool
		hc := &http.Client{Timeout: c.timeout, Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
			deadline, ok = req.Context().Deadline()
			sent = time.Now()
			return nil, stop
		})}
		began := time.Now()
		if _, _, err := Exchange(context.Background(), hc, "127.0.0.1:1", "Laptop", "darwin"); !errors.Is(err, stop) {
			t.Fatalf("a %v client: Exchange = %v, want it stopped at its first request", c.timeout, err)
		}
		// Both ends are clock reads around the moment the deadline was set.
		if !ok || deadline.Before(began.Add(c.want)) || deadline.After(sent.Add(c.want)) {
			t.Fatalf("a %v client sent its first request with a deadline %v after the exchange began (set %t), want %v", c.timeout, deadline.Sub(began), ok, c.want)
		}
	}
	if exchangeTimeout != 15*time.Second {
		t.Fatalf("exchangeTimeout = %v, want 15s", exchangeTimeout)
	}
	sent := false
	hc := &http.Client{Timeout: -time.Second, Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		sent = true
		return nil, stop
	})}
	if _, _, err := Exchange(context.Background(), hc, "127.0.0.1:1", "Laptop", "darwin"); err == nil || !strings.Contains(err.Error(), "negative") || sent {
		t.Fatalf("a client with a negative Timeout: Exchange = %v (sent %t), want it refused before sending", err, sent)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
