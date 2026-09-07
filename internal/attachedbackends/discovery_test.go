package attachedbackends

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/entityid"
	"agent-overflow/internal/pairbootstrap"
)

func discoveryServer(t *testing.T, handle func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.Method != http.MethodPost || r.URL.Path != pairbootstrap.Path {
			t.Errorf("unexpected discovery request %s %s TLS=%t", r.Method, r.URL.Path, r.TLS != nil)
		}
		for _, header := range []string{"Authorization", "Cookie", "X-Ao-Session", "X-Ao-Device-Key"} {
			if r.Header.Get(header) != "" {
				t.Errorf("discovery carried %s", header)
			}
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"op":"info"}` {
			t.Errorf("discovery body=%q error=%v", body, err)
		}
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestDiscoveryProbesRealTLSWithoutCredentialsAndAggregatesReachableRoutes(t *testing.T) {
	manager, _ := newManager(t)
	id := entityid.New()
	serve := func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(computerInfo{BackendID: id, Name: "Actual device name", Open: true})
	}
	lan := discoveryServer(t, serve)
	tailnet := discoveryServer(t, serve)
	dead := discoveryServer(t, serve)
	dead.Close()
	candidates := []DiscoveredComputer{
		{BackendID: id, Name: "untrusted name", Address: tailnet.URL, Network: "tailnet"},
		{BackendID: id, Address: dead.URL, Network: "lan"},
		{BackendID: id, Name: "wrong name", Address: lan.URL, Network: "lan"},
	}
	got, err := manager.probeCandidates(context.Background(), candidates)
	want := []DiscoveredComputer{{BackendID: id, Name: "Actual device name", Address: lan.URL, Network: "lan"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("discovery=%+v error=%v want=%+v", got, err, want)
	}
	sessions, err := deviceclient.ListSessions(manager.dir)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("probe persisted trust: sessions=%+v error=%v", sessions, err)
	}
	// An unreachable LAN route must not hide the same host's working tailnet route.
	lan.Close()
	got, err = manager.probeCandidates(context.Background(), candidates)
	want[0].Address, want[0].Network = tailnet.URL, "tailnet"
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback=%+v error=%v want=%+v", got, err, want)
	}
}

func TestDiscoveryRejectsMismatchedClosedAndMalformedHosts(t *testing.T) {
	manager, _ := newManager(t)
	id := entityid.New()
	var redirectRequests atomic.Int32
	redirect := discoveryServer(t, func(w http.ResponseWriter, r *http.Request) {
		redirectRequests.Add(1)
		json.NewEncoder(w).Encode(computerInfo{BackendID: id, Name: "Redirect target", Open: true})
	})
	cases := []struct {
		name   string
		handle func(http.ResponseWriter, *http.Request)
	}{
		{"different identity", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(computerInfo{BackendID: entityid.New(), Name: "Other", Open: true})
		}},
		{"pairing closed", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(computerInfo{BackendID: id, Name: "Closed", Open: false})
		}},
		{"not available", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }},
		{"malformed JSON", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"backendId":`) }},
		{"invalid identity", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(computerInfo{BackendID: "not-a-backend-id", Name: "Bad ID", Open: true})
		}},
		{"missing name", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(computerInfo{BackendID: id, Open: true})
		}},
		{"oversized body", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat(" ", 4097)) }},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, redirect.URL+pairbootstrap.Path, http.StatusTemporaryRedirect)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := discoveryServer(t, tc.handle)
			got, err := manager.probeCandidates(context.Background(), []DiscoveredComputer{{BackendID: id, Address: server.URL, Network: "lan"}})
			if err != nil || len(got) != 0 {
				t.Fatalf("accepted %+v error=%v", got, err)
			}
		})
	}
	if redirectRequests.Load() != 0 {
		t.Fatal("followed an untrusted discovery redirect")
	}
}

func TestDiscoveryExcludesSelfAndSavedProfilesBeforeAndAfterProbing(t *testing.T) {
	manager, dir := newManager(t)
	self, known := entityid.New(), entityid.New()
	manager.SetNetwork(func() string { return self }, nil)
	seed(t, dir, deviceclient.Session{BackendID: known, BackendName: "Saved", Endpoint: "https://saved.invalid", SessionID: "session", Credential: "credential"})
	var calls atomic.Int32
	selfServer := discoveryServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(computerInfo{BackendID: self, Name: "Self", Open: true})
	})
	knownServer := discoveryServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(computerInfo{BackendID: known, Name: "Saved", Open: true})
	})
	candidates := []DiscoveredComputer{{BackendID: self, Address: selfServer.URL, Network: "lan"}, {BackendID: known, Address: knownServer.URL, Network: "lan"}}
	got, err := manager.probeCandidates(context.Background(), candidates)
	if err != nil || len(got) != 0 || calls.Load() != 0 {
		t.Fatalf("known hints discovery=%+v calls=%d error=%v", got, calls.Load(), err)
	}
	for i := range candidates {
		candidates[i].BackendID = ""
		candidates[i].Network = "tailnet"
	}
	got, err = manager.probeCandidates(context.Background(), candidates)
	if err != nil || len(got) != 0 || calls.Load() != 2 {
		t.Fatalf("unidentified hints discovery=%+v calls=%d error=%v", got, calls.Load(), err)
	}
}
