//go:build !noremote

package transport_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/authlimit"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transport"
)

// A connect by address that the other computer's auth budget refused tells
// the person a wait; a connect made after that wait goes through every
// request it makes on that budget (the preflight, Begin, Reveal, and the
// redemption) without being limited again.
func TestAConnectByAddressAfterTheToldWaitIsNotLimitedAgain(t *testing.T) {
	const backendID = "6f0c8a2e-3b4d-4c5e-9f60-7a8b9c0d1e2f"
	var mu sync.Mutex
	clock := time.Unix(1_000_000, 0)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	book := pairbootstrap.NewBook(func(string) error { return nil }, time.Hour)
	var redemptions atomic.Int32
	limited := transport.AuthRateLimitedForTest(now, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pairbootstrap.Path:
			var req pairbootstrap.Request
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			var v any
			var err error
			switch req.Op {
			case "info":
				v = map[string]any{"backendId": backendID, "name": "Workstation", "open": true}
			case "begin":
				v, err = book.Begin(req.Commitment)
			default:
				v, err = book.Reveal(req.ID, req.Reveal, "lan")
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			_ = json.NewEncoder(w).Encode(v)
		case "/auth/pair":
			redemptions.Add(1)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"reason":"invalid_link"}`))
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewTLSServer(limited)
	t.Cleanup(server.Close)
	route, err := computerroute.Normalize(computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{Version: deviceclient.LinkVersion, BackendID: backendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "pairing"})
	if err != nil {
		t.Fatal(err)
	}
	link := server.URL + "/#" + deviceclient.LinkFragmentPrefix + base64.RawURLEncoding.EncodeToString(encoded)
	window, err := book.Open(func(string) (pairbootstrap.Invitation, error) {
		return pairbootstrap.Invitation{LinkID: "link", URL: link}, nil
	}, func(pairbootstrap.Snapshot) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close(window.WindowID, pairbootstrap.EndedClosed) })

	// Spend the budget from the address the connect comes from.
	for spent := 0; ; spent++ {
		if spent > 100 {
			t.Fatal("the budget never ran out")
		}
		req := httptest.NewRequest(http.MethodPost, pairbootstrap.Path, strings.NewReader(`{"op":"info"}`))
		req.RemoteAddr = "127.0.0.1:1"
		answer := httptest.NewRecorder()
		limited(answer, req)
		if answer.Code == http.StatusTooManyRequests {
			break
		}
	}

	dir := t.TempDir()
	if _, err := deviceclient.EnrollDeviceKey(dir); err != nil {
		t.Fatal(err)
	}
	manager, err := attachedbackends.New(dir, "this-machine", "linux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	address := strings.TrimPrefix(server.URL, "https://")
	_, err = manager.Add(context.Background(), address)
	var refused *authlimit.RateLimited
	if !errors.As(err, &refused) {
		t.Fatalf("a connect on a spent budget = %v, want it limited", err)
	}
	told := transport.CallerError(err).Message
	mu.Lock()
	clock = clock.Add(refused.RetryAfter)
	mu.Unlock()
	_, err = manager.Add(context.Background(), address)
	if errors.As(err, &refused) {
		t.Fatalf("the connect after the told wait was limited again: %v", err)
	}
	if got := redemptions.Load(); got != 1 {
		t.Fatalf("the connect after the told wait made %d redemptions (%v), want 1", got, err)
	}
	if want := "this computer is limiting pairing attempts; wait 4 seconds, then try again"; told != want {
		t.Fatalf("the person reads %q, want %q", told, want)
	}
}
