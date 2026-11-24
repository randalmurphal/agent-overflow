//go:build !noremote

package attachedbackends

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/pairbootstrap"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transport"
)

// A computer that limits pairing attempts answers the redemption before
// judging the link. The person adding it on the desktop is told to wait for
// the time it named, and nothing is saved.
func TestAddingAComputerThatLimitsPairingSaysToWait(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/pair" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}))
	t.Cleanup(server.Close)
	route, err := computerroute.Normalize(computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{Version: deviceclient.LinkVersion, BackendID: introducedBackendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "pairing"})
	if err != nil {
		t.Fatal(err)
	}
	manager, dir := newManager(t)
	_, err = manager.Add(context.Background(), deviceclient.LinkFragmentPrefix+base64.RawURLEncoding.EncodeToString(encoded))
	if err == nil {
		t.Fatal("adding a computer that limits pairing succeeded")
	}
	const want = "this computer is limiting pairing attempts; wait 30 seconds, then try again"
	if answer := transport.CallerError(err); answer.Message != want {
		t.Fatalf("the person reads %q (%v), want %q", answer.Message, err, want)
	}
	if _, loadErr := deviceclient.LoadSession(dir, introducedBackendID); loadErr == nil {
		t.Fatal("a refused pairing saved a session")
	}
}

// An address pairing whose exchange succeeded and whose redemption the
// limit then refused has discarded the window's invitation, and the window
// admits no other requester (pairbootstrap's
// TestAWindowHoldingAnExchangeAdmitsAnotherOnlyAsANewWindow): the person is
// told to wait and pair through a new window, not to try the same connect.
func TestAnAddressPairingWhoseRedemptionIsLimitedSaysToOpenANewWindow(t *testing.T) {
	book := pairbootstrap.NewBook(func(string) error { return nil }, time.Hour)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pairbootstrap.Path:
			var req pairbootstrap.Request
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			var v any
			var err error
			if req.Op == "begin" {
				v, err = book.Begin(req.Commitment)
			} else {
				v, err = book.Reveal(req.ID, req.Reveal, "lan")
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			_ = json.NewEncoder(w).Encode(v)
		case "/auth/pair":
			w.Header().Set("Retry-After", "30")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	route, err := computerroute.Normalize(computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{Version: deviceclient.LinkVersion, BackendID: introducedBackendID,
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
	manager, _ := newManager(t)
	_, err = manager.Add(context.Background(), strings.TrimPrefix(server.URL, "https://"))
	want := "this computer is limiting pairing attempts; wait 30 seconds, then " + pairbootstrap.RestartHint
	if answer := transport.CallerError(err); answer.Message != want {
		t.Fatalf("the person reads %q (%v), want %q", answer.Message, err, want)
	}
}
