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

// unanswered is each way a computer gives a redemption no verdict, with how
// the failure reads before its remedy.
var unanswered = []struct {
	name   string
	answer http.HandlerFunc
	reads  string
}{
	{"limited", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
	}, "this computer is limiting pairing attempts; wait "},
	{"unable to save", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"reason":"temporarily_unavailable"}`))
	}, "deviceclient: backend temporarily unavailable (HTTP 503); "},
	{"unreached", func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}, "deviceclient: reach https://"},
}

// A copied link the computer gave no verdict on was not spent, so nothing
// tells the person to open a new window.
func TestACopiedLinkWhoseRedemptionIsUnansweredNamesNoNewWindow(t *testing.T) {
	for _, tc := range unanswered {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(tc.answer)
			t.Cleanup(server.Close)
			manager, _ := newManager(t)
			_, err := manager.Add(context.Background(), deviceclient.LinkFragmentPrefix+base64.RawURLEncoding.EncodeToString(encodedLink(t, server)))
			message := transport.CallerError(err).Message
			if !strings.HasPrefix(message, tc.reads) || strings.Contains(message, pairbootstrap.RestartHint) {
				t.Fatalf("the person reads %q (%v), want %q and no new window", message, err, tc.reads)
			}
		})
	}
}

// An address pairing whose exchange succeeded and whose redemption the
// computer then gave no verdict on has discarded the window's invitation,
// and the window admits no other requester (pairbootstrap's
// TestAWindowHoldingAnExchangeAdmitsAnotherOnlyAsANewWindow): the person is
// told to pair through a new window, not to try the same connect.
func TestAnAddressPairingWhoseRedemptionIsUnansweredSaysToOpenANewWindow(t *testing.T) {
	for _, tc := range unanswered {
		t.Run(tc.name, func(t *testing.T) {
			message := transport.CallerError(addressPairingRedeemedBy(t, tc.answer)).Message
			if !strings.HasPrefix(message, tc.reads) || !strings.HasSuffix(message, "; "+pairbootstrap.RestartHint) && !strings.HasSuffix(message, ", then "+pairbootstrap.RestartHint) {
				t.Fatalf("the person reads %q, want %q ending with %q", message, tc.reads, pairbootstrap.RestartHint)
			}
		})
	}
}

// The wait a limited redemption of an address pairing names covers the
// whole connect through the new window (addressConnectRequests).
func TestAnAddressPairingWhoseRedemptionIsLimitedWaitsForAWholeConnect(t *testing.T) {
	err := addressPairingRedeemedBy(t, unanswered[0].answer)
	want := "this computer is limiting pairing attempts; wait 2 minutes, then " + pairbootstrap.RestartHint
	if answer := transport.CallerError(err); answer.Message != want {
		t.Fatalf("the person reads %q (%v), want %q", answer.Message, err, want)
	}
}

// addressPairingRedeemedBy connects by address to a computer whose pairing
// window answers the exchange and whose redemption pair answers, and
// returns what the connect answered.
func addressPairingRedeemedBy(t *testing.T, pair http.HandlerFunc) error {
	t.Helper()
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
			pair(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	link := server.URL + "/#" + deviceclient.LinkFragmentPrefix + base64.RawURLEncoding.EncodeToString(encodedLink(t, server))
	window, err := book.Open(func(string) (pairbootstrap.Invitation, error) {
		return pairbootstrap.Invitation{LinkID: "link", URL: link}, nil
	}, func(pairbootstrap.Snapshot) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = book.Close(window.WindowID, pairbootstrap.EndedClosed) })
	manager, _ := newManager(t)
	_, err = manager.Add(context.Background(), strings.TrimPrefix(server.URL, "https://"))
	if err == nil {
		t.Fatal("the connect succeeded")
	}
	return err
}

// encodedLink is a pairing link to server, as a link's fragment encodes it.
func encodedLink(t *testing.T, server *httptest.Server) []byte {
	t.Helper()
	route, err := computerroute.Normalize(computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(deviceclient.Link{Version: deviceclient.LinkVersion, BackendID: introducedBackendID,
		Endpoint: route.Endpoint, CertFingerprint: route.CertFingerprint, Token: "pairing"})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
