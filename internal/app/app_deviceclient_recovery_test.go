package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/backendproxy"
	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/servercert"
	"agent-overflow/internal/transport"
)

// The relay loses exactly one successful token response AFTER the real server
// commits. Both legs use verified TLS; no auth or database behavior is mocked.
func TestDeviceClientRecoversLostCommittedRenewalOverHTTP(t *testing.T) {
	for _, alternate := range []bool{false, true} {
		name := "same listener"
		if alternate {
			name = "different verified listener after restart"
		}
		t.Run(name, func(t *testing.T) { testLostCommittedRenewal(t, alternate) })
	}
}

func testLostCommittedRenewal(t *testing.T, switchRoute bool) {
	var advertised atomic.Value
	advertised.Store([]computerroute.Route(nil))
	backend := newPairedBackend(t, func(cfg *transport.Config) {
		cfg.ComputerRoutes = func() []computerroute.Route { return advertised.Load().([]computerroute.Route) }
	})
	if switchRoute {
		advertised.Store([]computerroute.Route{alternatePairedListener(t, backend)})
	}
	invite, link := backend.mintLink(t, string(identity.PairingAccessFull))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	dir := t.TempDir()
	client, _, err := deviceclient.Pair(ctx, dir, link, "recovery test", "linux")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := client.AwaitActivation(ctx); err != nil {
		t.Fatal(err)
	}
	upstream := &http.Client{Transport: client.RoundTripper(), Timeout: 5 * time.Second}
	var lose atomic.Bool
	lose.Store(true)
	var renewals atomic.Int32
	relay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, client.Endpoint()+r.URL.RequestURI(), r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "request failed", 502)
			return
		}
		req.Header = r.Header.Clone()
		response, err := upstream.Do(req)
		if err != nil {
			t.Error(err)
			http.Error(w, "upstream failed", 502)
			return
		}
		defer response.Body.Close()
		if r.URL.Path == "/auth/token/recover" {
			renewals.Add(1)
			if lose.CompareAndSwap(true, false) {
				if response.StatusCode != 200 {
					t.Errorf("renewal status before lost reply: %d", response.StatusCode)
				}
				io.Copy(io.Discard, response.Body)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				return
			}
		}
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	}))
	defer relay.Close()
	held := client.Session()
	held.Endpoint = relay.URL
	held.CertFingerprint = servercert.Fingerprint(relay.Certificate().Raw)
	held.ExpiresAtMs = 1
	if err := deviceclient.SaveSession(dir, held); err != nil {
		t.Fatal(err)
	}
	remote, err := deviceclient.Open(dir, held)
	if err != nil {
		t.Fatal(err)
	}
	if switchRoute {
		wsURL, err := remote.WebSocketURL()
		if err != nil {
			t.Fatal(err)
		}
		carrier, err := backendproxy.New(backendproxy.Config{WSURL: wsURL, Paired: remote})
		if err != nil {
			t.Fatal(err)
		}
		if status, _, err := carrier.FetchBootstrap(ctx); err != nil || status != 200 {
			t.Fatalf("learn recovery route: HTTP %d, %v", status, err)
		}
	}
	if _, err := remote.Ticket(ctx); err == nil {
		t.Fatal("lost response reported success")
	}
	pending, err := deviceclient.LoadSession(dir, link.BackendID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.PendingNextSecret == "" {
		t.Fatal("recovery state not persisted")
	}
	if switchRoute {
		relay.Close()
	}
	restarted, err := deviceclient.Open(dir, pending)
	if err != nil {
		t.Fatal(err)
	}
	if switchRoute {
		if _, err := restarted.Ticket(ctx); err == nil {
			t.Fatal("closed original listener unexpectedly answered")
		}
	}
	if _, err := restarted.Ticket(ctx); err != nil {
		t.Fatal(err)
	}
	saved, err := deviceclient.LoadSession(dir, link.BackendID)
	if err != nil {
		t.Fatal(err)
	}
	wantRelayRenewals := int32(2)
	if switchRoute {
		wantRelayRenewals = 1
	}
	if saved.RefreshSecret != pending.PendingNextSecret || saved.PendingNextSecret != "" || renewals.Load() != wantRelayRenewals {
		t.Fatal("did not recover the original rotation")
	}
	chain, err := backend.app.store.ListRefreshSecretsForSession(held.SessionID)
	if err != nil || len(chain) != 2 {
		t.Fatalf("refresh generations: %d, %v", len(chain), err)
	}
}
