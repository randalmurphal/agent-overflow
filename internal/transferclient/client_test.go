package transferclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/loopback/loopbacktest"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transferwire"
)

func testOffer() Offer {
	return Offer{Version: 1, BackendID: entityid.New(), OperationID: entityid.New(), Endpoint: "https://computer.example", Grant: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xa5}, 32))}
}

func TestTransferClientRefusesUnsafeOffersBeforeConnecting(t *testing.T) {
	for _, endpoint := range []string{"http://192.168.1.8:3437", "http://localhost.untrusted.example", "ftp://host", "https://user:password@host", "https://host/path", "https://host?secret=one", "https://host?", "https://host/#secret", "https://host:0", "https://host:99999", "https:opaque", "/relative"} {
		t.Run(endpoint, func(t *testing.T) {
			offer := testOffer()
			offer.Endpoint = endpoint
			if c, err := New(offer, nil); err == nil {
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
			if c, err := New(offer, nil); err == nil {
				c.Close()
				t.Fatal("accepted malformed offer")
			}
		})
	}
}

func TestTransferClientPinsTLSAndBindsEachReply(t *testing.T) {
	remotetest.Require(t)
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
			client, err := New(offer, nil)
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
	destination := loopbacktest.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer destination.Close()
	redirect := loopbacktest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	offer := testOffer()
	offer.Endpoint = redirect.URL
	client, err := New(offer, nil)
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
	client, err := New(offer, nil)
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
	client, err := New(offer, nil)
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

// A literal loopback endpoint receives the grant; another process holding
// the same port on the other family does not.
func TestTransferClientDialsTheLoopbackAddressItWasOffered(t *testing.T) {
	remotetest.Require(t)
	ln6, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	port := strconv.Itoa(ln6.Addr().(*net.TCPAddr).Port)
	ln4, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		_ = ln6.Close()
		t.Skipf("127.0.0.1:%s is taken by another process: %v", port, err)
	}
	var hits [2]atomic.Int32
	for i, ln := range []net.Listener{ln4, ln6} {
		srv := &http.Server{ReadHeaderTimeout: time.Second, ErrorLog: log.New(io.Discard, "", 0),
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits[i].Add(1) })}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
	}
	for i, host := range []string{"127.0.0.1", "[::1]"} {
		offer := testOffer()
		offer.Endpoint = "http://" + host + ":" + port
		client, err := New(offer, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = client.Status(context.Background())
		client.Close()
		if hits[i].Load() != 1 || hits[1-i].Load() != 0 {
			t.Fatalf("%s: hits = [%d %d], want only the offered address", host, hits[0].Load(), hits[1-i].Load())
		}
		hits[i].Store(0)
	}
}

// An https offer travels over the dialer it was given, the application's
// route to a peer reachable only through its built-in tailnet node. The
// endpoint's own address has nothing listening, so the OS dialer would
// fail without leaving the machine.
func TestTransferClientCarriesHTTPSOverTheSuppliedDialer(t *testing.T) {
	remotetest.Require(t)
	offer := testOffer()
	host := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(transferwire.Reply{Version: 1, BackendID: offer.BackendID, OperationID: offer.OperationID, State: &transferwire.State{Phase: "preparing"}})
	}))
	host.Config.ErrorLog = log.New(io.Discard, "", 0)
	host.StartTLS()
	defer host.Close()
	offer.Endpoint = "https://127.0.0.1:1"
	offer.CertFingerprint = servercert.Fingerprint(host.Certificate().Raw)
	var dialed atomic.Value
	client, err := New(offer, func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed.Store(address)
		return (&net.Dialer{}).DialContext(ctx, network, host.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if state, err := client.Status(context.Background()); err != nil || state.Phase != "preparing" {
		t.Fatalf("Status = %+v, %v; want the reply carried by the supplied dialer", state, err)
	}
	if got, _ := dialed.Load().(string); got != "127.0.0.1:1" {
		t.Fatalf("supplied dialer saw %q, want the offer's endpoint", got)
	}
}
