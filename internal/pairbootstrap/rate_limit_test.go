package pairbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/authlimit"
)

// limitedPeer serves b's exchange over TLS, answering the next request of
// op with the auth rate limit's 429 when limit names it.
func limitedPeer(t *testing.T, b *Book) (url string, limit func(op string)) {
	t.Helper()
	var mu sync.Mutex
	limited := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		mu.Lock()
		hit := limited == req.Op
		if hit {
			limited = ""
		}
		mu.Unlock()
		if hit {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		var v any
		var err error
		switch req.Op {
		case "begin":
			v, err = b.Begin(req.Commitment)
		case "reveal":
			v, err = b.Reveal(req.ID, req.Reveal, "lan")
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(server.Close)
	return server.URL, func(op string) { mu.Lock(); limited = op; mu.Unlock() }
}

func exchangeWith(t *testing.T, url string) error {
	t.Helper()
	hc, err := NewHTTPClient(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.CloseIdleConnections()
	_, _, err = Exchange(context.Background(), hc, url, "Laptop", "darwin")
	return err
}

func requireLimited(t *testing.T, err error, then string) {
	t.Helper()
	var limited *authlimit.RateLimited
	if !errors.As(err, &limited) || limited.RetryAfter != 30*time.Second || limited.Then != then {
		t.Fatalf("the exchange = %v, want the rate limit's 30 second wait, then %q", err, then)
	}
}

// A limited begin leaves the window free, so the same connect after the
// wait pairs.
func TestALimitedBeginSaysToWaitAndTryAgain(t *testing.T) {
	b, _ := startBook(t)
	url, limit := limitedPeer(t, b)
	limit("begin")
	requireLimited(t, exchangeWith(t, url), "try again")
	if err := exchangeWith(t, url); err != nil {
		t.Fatalf("the connect after the wait: %v", err)
	}
}

// A window admits one requester until it ends. Once a requester's
// commitment is held, by a limited reveal or by a whole exchange whose
// redemption then did not finish, a new connect is refused as busy, and
// only a new window admits it: what RestartHint tells a person to do.
func TestAWindowHoldingAnExchangeAdmitsAnotherOnlyAsANewWindow(t *testing.T) {
	for _, held := range []string{"a limited reveal", "a whole exchange"} {
		t.Run(held, func(t *testing.T) {
			b := settledBook()
			window := openWindow(t, b, invitation)
			url, limit := limitedPeer(t, b)
			if held == "a limited reveal" {
				limit("reveal")
				requireLimited(t, exchangeWith(t, url), RestartHint)
			} else if err := exchangeWith(t, url); err != nil {
				t.Fatal(err)
			}
			if err := exchangeWith(t, url); err == nil {
				t.Fatal("a second connect to the window holding an exchange paired")
			}
			if err := b.Close(window.WindowID, EndedClosed); err != nil {
				t.Fatal(err)
			}
			openWindow(t, b, invitation)
			if err := exchangeWith(t, url); err != nil {
				t.Fatalf("the connect to a new window: %v", err)
			}
		})
	}
}
