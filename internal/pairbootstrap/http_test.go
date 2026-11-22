package pairbootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The client's bound ends an exchange whose answer never comes, and
// replaces the wait for one answer too. Zero keeps the product's bounds:
// 15 s for the exchange and 10 s for one answer.
func TestTheClientsBoundEndsAHeldExchange(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	const bound = 100 * time.Millisecond
	hc := NewHTTPClient(nil, bound)
	defer hc.CloseIdleConnections()
	started := time.Now()
	_, _, err := Exchange(context.Background(), hc, server.URL, "Laptop", "darwin")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("an exchange whose answer never came succeeded")
	}
	if elapsed >= responseHeaderTimeout {
		t.Fatalf("the exchange ended after %v, at the product's wait for an answer, not at the %v bound it was given: %v", elapsed, bound, err)
	}

	long := NewHTTPClient(nil, 2*time.Minute)
	if answer := long.Transport.(*http.Transport).ResponseHeaderTimeout; long.Timeout != 2*time.Minute || answer != 2*time.Minute {
		t.Fatalf("a 2m bound gives the exchange %v and one answer %v, want 2m for both", long.Timeout, answer)
	}

	product := NewHTTPClient(nil, 0)
	answer := product.Transport.(*http.Transport).ResponseHeaderTimeout
	if product.Timeout != 15*time.Second || answer != 10*time.Second {
		t.Fatalf("the product client waits %v for the exchange and %v for one answer, want 15s and 10s", product.Timeout, answer)
	}
}
