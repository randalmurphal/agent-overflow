package transferclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transferwire"
)

func testOffer() Offer {
	return Offer{Version: 1, BackendID: entityid.New(), OperationID: entityid.New(), Endpoint: "https://computer.example", Grant: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa5}, 32))}
}

func TestTransferClientRefusesUnsafeOffersBeforeConnecting(t *testing.T) {
	for _, endpoint := range []string{"http://192.168.1.8:3437", "http://localhost.evil.example", "ftp://host", "https://user:password@host", "https://host/path", "https://host?secret=one", "https://host?", "https://host/#secret", "https://host:0", "https://host:99999", "https:opaque", "/relative"} {
		t.Run(endpoint, func(t *testing.T) {
			offer := testOffer()
			offer.Endpoint = endpoint
			if c, err := New(offer); err == nil {
				c.Close()
				t.Fatal("accepted unsafe endpoint")
			}
		})
	}
	for _, field := range []string{"version", "backend", "operation", "grant", "certificate"} {
		t.Run(field, func(t *testing.T) {
			offer := testOffer()
			switch field {
			case "version":
				offer.Version = 2
			case "backend":
				offer.BackendID = "bad"
			case "operation":
				offer.OperationID = "bad"
			case "grant":
				offer.Grant = "bad"
			case "certificate":
				offer.CertFingerprint = "bad"
			}
			if c, err := New(offer); err == nil {
				c.Close()
				t.Fatal("accepted malformed offer")
			}
		})
	}
}

func TestTransferClientPinsTLSAndBindsEachReply(t *testing.T) {
	for _, change := range []string{"none", "certificate", "backend", "operation", "version", "phase", "progress", "incomplete preparation", "untrusted error", "oversized reply"} {
		t.Run(change, func(t *testing.T) {
			offer := testOffer()
			reply := transferwire.Reply{Version: 1, BackendID: offer.BackendID, OperationID: offer.OperationID, State: &transferwire.State{Phase: "preparing"}}
			switch change {
			case "backend":
				reply.BackendID = entityid.New()
			case "operation":
				reply.OperationID = entityid.New()
			case "version":
				reply.Version = 2
			case "phase":
				reply.State.Phase = "future"
			case "progress":
				reply.State.Received = 1
			case "incomplete preparation":
				reply.State.Phase = "prepared"
			case "untrusted error":
				reply.Error = "secret peer stacktrace"
			}
			host := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+offer.Grant || r.Header.Get(transferwire.BackendHeader) != offer.BackendID {
					t.Error("incorrect authority carrier")
				}
				if change == "oversized reply" {
					io.WriteString(w, strings.Repeat("x", 32<<10))
					return
				}
				json.NewEncoder(w).Encode(reply)
			}))
			host.Config.ErrorLog = log.New(io.Discard, "", 0)
			host.StartTLS()
			defer host.Close()
			offer.Endpoint = host.URL
			offer.CertFingerprint = servercert.Fingerprint(host.Certificate().Raw)
			if change == "certificate" {
				offer.CertFingerprint = "sha256:" + strings.Repeat("a", 64)
			}
			client, err := New(offer)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			state, err := client.Status(context.Background())
			if change == "none" {
				if err != nil || state.Phase != "preparing" {
					t.Fatalf("valid reply refused: %+v %v", state, err)
				}
				return
			}
			var refusal *Error
			if !errors.As(err, &refusal) || strings.Contains(err.Error(), "stacktrace") {
				t.Fatalf("unsafe/missing refusal: %v", err)
			}
			if change == "certificate" && refusal.Code != "certificate_changed" {
				t.Fatalf("certificate failure lost: %v", err)
			}
			if (change == "backend" || change == "operation") && refusal.Code != "destination_changed" {
				t.Fatalf("identity failure lost: %v", err)
			}
		})
	}
}

func TestTransferClientNeverFollowsRedirectWithAuthority(t *testing.T) {
	var reached atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	offer := testOffer()
	offer.Endpoint = redirect.URL
	client, err := New(offer)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Activate(context.Background(), base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xb6}, 32))); err == nil {
		t.Fatal("followed redirect")
	}
	if reached.Load() != 0 {
		t.Fatal("redirect target received authority")
	}
}

func TestTransferClientLoopbackIgnoresProxyConfiguration(t *testing.T) {
	// Even a configured default transport proxy cannot carry a plaintext
	// loopback grant off this machine. DNS likewise cannot redirect the dial.
	base := http.DefaultTransport
	clone := http.DefaultTransport.(*http.Transport).Clone()
	proxy, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	clone.Proxy = http.ProxyURL(proxy)
	http.DefaultTransport = clone
	t.Cleanup(func() { http.DefaultTransport = base })
	offer := testOffer()
	offer.Endpoint = "http://localhost:3437"
	client, err := New(offer)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if client.transport.Proxy != nil || client.transport.DialContext == nil {
		t.Fatal("loopback grant may leave the machine")
	}
}

type transferRoundTripFunc func(*http.Request) (*http.Response, error)

func (f transferRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransferChunkReturnFencesLateHTTPBodyReaders(t *testing.T) {
	offer := testOffer()
	client, err := New(offer)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var late io.ReadCloser
	client.http.Transport = transferRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		late = r.Body
		return nil, context.DeadlineExceeded
	})
	buffer := []byte("reusable chunk buffer")
	if _, err := client.Chunk(context.Background(), 0, strings.Repeat("a", 64), buffer); err == nil {
		t.Fatal("expected timeout")
	}
	copy(buffer, "another request data")
	var data [32]byte
	if n, err := late.Read(data[:]); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("late writer read a reused buffer: %d %v", n, err)
	}
}
