package deviceclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/computerroute"
	"agent-overflow/internal/servercert"
)

func candidateServer(t *testing.T, id string, serve func(http.ResponseWriter, *http.Request)) (*httptest.Server, computerroute.Route) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			for _, key := range []string{SessionCredentialHeader, DeviceKeyHeader, "Authorization", "Cookie"} {
				if r.Header.Get(key) != "" {
					t.Errorf("health probe carried %s", key)
				}
			}
			json.NewEncoder(w).Encode(map[string]string{"backendId": id})
			return
		}
		serve(w, r)
	}))
	t.Cleanup(server.Close)
	return server, computerroute.Route{Endpoint: server.URL, CertFingerprint: servercert.Fingerprint(server.Certificate().Raw)}
}

func learnRoutes(t *testing.T, client *Client, routes ...computerroute.Route) {
	t.Helper()
	body, err := json.Marshal(struct {
		BackendID string                `json:"backendId"`
		Routes    []computerroute.Route `json:"routes"`
	}{client.Session().BackendID, routes})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ObserveBootstrap(context.Background(), body); err != nil {
		t.Fatal(err)
	}
}

func routeRequest(client *Client, method, path string) error {
	req, err := client.request(context.Background(), method, path, []byte("one operation"))
	if err != nil {
		return err
	}
	if err := client.Authorize(req); err != nil {
		return err
	}
	response, err := client.http.Do(req)
	if err == nil {
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	return err
}

func TestComputerRoutesDistinguishBrokenUpgradeFromAuthentication(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var originalCalls, alternateCalls atomic.Int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					json.NewEncoder(w).Encode(map[string]string{"backendId": "backend-a"})
				} else if r.Header.Get("Upgrade") == "websocket" {
					w.WriteHeader(status)
				} else {
					originalCalls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			t.Cleanup(first.Close)
			client, _ := openAgainst(t, &backend{Server: first}, nil)
			_, alternate := candidateServer(t, client.Session().BackendID, func(w http.ResponseWriter, r *http.Request) {
				alternateCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			})
			learnRoutes(t, client, alternate)
			req, err := http.NewRequest(http.MethodGet, client.Endpoint()+"/ws", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Connection", "Upgrade")
			response, err := client.http.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != status || alternateCalls.Load() != 0 {
				t.Fatal("failed upgrade was hidden or replayed")
			}
			if err := routeRequest(client, http.MethodGet, "/next"); err != nil {
				t.Fatal(err)
			}
			wantAlternate := status != http.StatusUnauthorized && status != http.StatusForbidden
			if (alternateCalls.Load() == 1) != wantAlternate || originalCalls.Load()+alternateCalls.Load() != 1 {
				t.Fatalf("wrong route after HTTP %d: original %d, alternate %d", status, originalCalls.Load(), alternateCalls.Load())
			}
		})
	}
}

func TestComputerRouteRejectsURLCredentialsBeforeDialing(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted request reached the wire") }))
	t.Cleanup(first.Close)
	client, _ := openAgainst(t, &backend{Server: first}, nil)
	req, err := http.NewRequest(http.MethodGet, client.Endpoint()+"/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.URL.User = url.UserPassword("unexpected", "secret")
	if _, err := client.http.Do(req); err == nil {
		t.Fatal("URL credentials were accepted")
	}
}

func TestComputerRoutesSwitchWithoutReplayingACommittedRequest(t *testing.T) {
	var operations atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operations.Add(1)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close() // committed, but its answer was lost.
	}))
	t.Cleanup(first.Close)
	client, _ := openAgainst(t, &backend{Server: first}, nil)
	var alternateCalls atomic.Int32
	_, alternate := candidateServer(t, client.Session().BackendID, func(w http.ResponseWriter, r *http.Request) {
		alternateCalls.Add(1)
		if r.URL.Path != "/next" || r.Method != http.MethodGet {
			t.Errorf("replayed a mutation on the alternate: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get(SessionCredentialHeader) == "" || r.Header.Get(DeviceKeyHeader) == "" {
			t.Error("missing paired credential/proof")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	learnRoutes(t, client, alternate)
	if err := routeRequest(client, http.MethodPost, "/mutate"); err == nil {
		t.Fatal("lost mutation reply was hidden")
	}
	if alternateCalls.Load() != 0 || operations.Load() != 1 {
		t.Fatal("failed mutation was retried")
	}
	first.Close()
	if err := routeRequest(client, http.MethodGet, "/next"); err != nil {
		t.Fatal(err)
	}
	if operations.Load() != 1 || alternateCalls.Load() != 1 {
		t.Fatal("route switch replayed previous work")
	}
	held, err := LoadSession(client.dir, client.Session().BackendID)
	if err != nil {
		t.Fatal(err)
	}
	if held.LastEndpoint != alternate.Endpoint || held.SessionID != client.Session().SessionID || held.Endpoint != first.URL {
		t.Fatal("switch changed pairing or lost the last working route")
	}
	reopened, err := Open(client.dir, held)
	if err != nil {
		t.Fatal(err)
	}
	if err := routeRequest(reopened, http.MethodGet, "/next"); err != nil {
		t.Fatal(err)
	}
}

func TestComputerRoutesRefuseWrongIdentityPinAndRedirectBeforeCredentials(t *testing.T) {
	for _, kind := range []string{"identity", "pin", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			first := newBackend(t)
			client, _ := openAgainst(t, first, nil)
			var credentialCalls atomic.Int32
			id := client.Session().BackendID
			if kind == "identity" {
				id = "different-computer"
			}
			_, candidate := candidateServer(t, id, func(http.ResponseWriter, *http.Request) { credentialCalls.Add(1) })
			if kind == "pin" {
				candidate.CertFingerprint = "sha256:" + strings.Repeat("a", 64)
			}
			if kind == "redirect" {
				redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, candidate.Endpoint+"/healthz", http.StatusTemporaryRedirect)
				}))
				t.Cleanup(redirect.Close)
				candidate = computerroute.Route{Endpoint: redirect.URL, CertFingerprint: servercert.Fingerprint(redirect.Certificate().Raw)}
			}
			learnRoutes(t, client, candidate)
			first.Close()
			if err := routeRequest(client, http.MethodGet, "/next"); err == nil {
				t.Fatal("closed initial route succeeded")
			}
			if err := routeRequest(client, http.MethodGet, "/next"); err == nil {
				t.Fatal("unverified candidate received a request")
			}
			if credentialCalls.Load() != 0 {
				t.Fatal("credentials reached an unverified route")
			}
			if _, err := LoadSession(client.dir, client.Session().BackendID); err != nil {
				t.Fatal("route failure removed pairing", err)
			}
		})
	}
}

func TestComputerRouteSelectionCoalescesAndDoesNotWaitForAStalledRoute(t *testing.T) {
	first := newBackend(t)
	client, _ := openAgainst(t, first, nil)
	var probes, calls atomic.Int32
	arrived, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	healthy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			if probes.Add(1) == 1 {
				close(arrived)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"backendId": client.Session().BackendID})
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(healthy.Close)
	stalled := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(stalled.Close)
	learnRoutes(t, client,
		computerroute.Route{Endpoint: stalled.URL, CertFingerprint: servercert.Fingerprint(stalled.Certificate().Raw)},
		computerroute.Route{Endpoint: healthy.URL, CertFingerprint: servercert.Fingerprint(healthy.Certificate().Raw)})
	first.Close()
	if err := routeRequest(client, http.MethodGet, "/next"); err == nil {
		t.Fatal("closed initial route succeeded")
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if err := routeRequest(client, http.MethodGet, "/next"); err != nil {
				t.Error(err)
			}
		})
	}
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("no health probe")
	}
	start := time.Now()
	unblock()
	wg.Wait()
	if time.Since(start) > time.Second {
		t.Fatal("healthy route waited behind a stalled route")
	}
	if probes.Load() != 1 || calls.Load() != 12 {
		t.Fatalf("probes=%d calls=%d", probes.Load(), calls.Load())
	}
}
