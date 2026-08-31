package relaysession

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/transport"
)

// TestSessionCarriersMatchTransport is the drift guard for the two
// spellings this package restates. The Windows launcher links this
// package and deliberately does not link the transport server, so a
// rename on the server side that missed these copies would silently stop
// both relays forwarding their credential — and only a live session would
// notice, by nothing at all going wrong.
//
// The import is test-only. Production code in this package must stay
// transport-free.
func TestSessionCarriersMatchTransport(t *testing.T) {
	if got := textproto.CanonicalMIMEHeaderKey(transport.SessionCredentialHeader); got != Header {
		t.Fatalf("Header = %q, transport's is %q", Header, got)
	}
	// The cookie NAME is port-qualified, so only the prefix is knowable
	// here. Asking the transport to write one for a known authority is how
	// the prefix is pinned without a test-only export.
	recorder := httptest.NewRecorder()
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:4321/bootstrap.json", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	transport.WriteSessionCookie(recorder, request, "ao1.example")
	planted := recorder.Result().Cookies()
	if len(planted) != 1 {
		t.Fatalf("the transport planted %d cookies", len(planted))
	}
	if !strings.HasPrefix(planted[0].Name, CookiePrefix) {
		t.Fatalf("transport names the session cookie %q, which does not start with %q",
			planted[0].Name, CookiePrefix)
	}
}

// bootstrapStub serves /bootstrap.json the way the backend does: the
// launch token in a bearer header, a session cookie in the answer.
func bootstrapStub(t *testing.T, credential string, status *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bootstrap.json" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer launch-token" {
			http.NotFound(w, r)
			return
		}
		if code := status.Load(); code != http.StatusOK {
			http.Error(w, "not ready", int(code))
			return
		}
		if credential != "" {
			http.SetCookie(w, &http.Cookie{
				Name: CookiePrefix + "4321", Value: credential, Path: "/", HttpOnly: true,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"wsUrl":"ws://example/ws"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sourceFor builds a Source against a stub the way a relay does: derive
// the bootstrap URL from the WebSocket endpoint, never spell it twice.
func sourceFor(t *testing.T, stub *httptest.Server) *Source {
	t.Helper()
	bootstrap, err := BootstrapURL(strings.Replace(stub.URL, "http://", "ws://", 1) + "/ws")
	if err != nil {
		t.Fatalf("BootstrapURL: %v", err)
	}
	return New(bootstrap, "launch-token", nil)
}

func TestARelayReadsTheCredentialOutOfTheBootstrapExchange(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	stub := bootstrapStub(t, "ao1.local-credential", status)

	source := sourceFor(t, stub)
	if got := source.Credential(context.Background()); got != "ao1.local-credential" {
		t.Fatalf("credential = %q, want the one the exchange planted", got)
	}
}

// TestARelayKeepsWorkingWhenThereIsNoCredential — an older backend, or one
// whose session core did not start. Forwarding is an improvement in
// attribution, never a new requirement for a relay to connect.
func TestARelayKeepsWorkingWhenThereIsNoCredential(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	stub := bootstrapStub(t, "", status)

	source := sourceFor(t, stub)
	if got := source.Credential(context.Background()); got != "" {
		t.Fatalf("credential = %q, want empty", got)
	}
}

// TestAHeldCredentialIsReusedRatherThanRefetched — the backend hands the
// same string out until it re-issues, so a fetch per reconnect would be a
// round trip for an answer that has not changed.
func TestAHeldCredentialIsReusedRatherThanRefetched(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	fetches := &atomic.Int32{}
	inner := bootstrapStub(t, "ao1.local-credential", status)
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(counting.Close)

	source := sourceFor(t, counting)
	for range 3 {
		if got := source.Credential(context.Background()); got != "ao1.local-credential" {
			t.Fatalf("credential = %q", got)
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("%d fetches for three reads, want 1", got)
	}

	// A refused connection is the one signal the held value has gone dead.
	source.Stale()
	if got := source.Credential(context.Background()); got != "ao1.local-credential" {
		t.Fatalf("credential after Stale = %q", got)
	}
	if got := fetches.Load(); got != 2 {
		t.Fatalf("%d fetches after Stale, want 2", got)
	}
	// And the refetch settles the staleness: the next read is cached again.
	_ = source.Credential(context.Background())
	if got := fetches.Load(); got != 2 {
		t.Fatalf("%d fetches after a successful refetch, want 2", got)
	}
}

// TestARelayKeepsAWorkingCredentialAcrossABootingBackend — the backend
// answers 503 while it starts, and discarding what we hold over that would
// drop attribution for every reconnect until it came back.
func TestARelayKeepsAWorkingCredentialAcrossABootingBackend(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	stub := bootstrapStub(t, "ao1.local-credential", status)

	source := sourceFor(t, stub)
	if got := source.Credential(context.Background()); got == "" {
		t.Fatal("the first fetch answered nothing")
	}
	status.Store(http.StatusServiceUnavailable)
	source.Stale()
	if got := source.Credential(context.Background()); got != "ao1.local-credential" {
		t.Fatalf("a read against a booting backend answered %q", got)
	}
}

func TestASourceThatCanFetchNothingAnswersNothing(t *testing.T) {
	if got := New("", "launch-token", nil).Credential(context.Background()); got != "" {
		t.Fatalf("credential = %q from a source with no bootstrap URL", got)
	}
	if got := New("http://example/bootstrap.json", "", nil).Credential(context.Background()); got != "" {
		t.Fatalf("credential = %q from a source with no token", got)
	}
}

func TestBootstrapURLRefusesToGuessAnAuthority(t *testing.T) {
	for name, wsURL := range map[string]string{
		"unparseable":  "://not a url",
		"no scheme":    "backend.example/ws",
		"http scheme":  "http://backend.example/ws",
		"no authority": "ws:///ws",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := BootstrapURL(wsURL)
			if err == nil {
				t.Fatalf("BootstrapURL(%q) = %q, want an error", wsURL, got)
			}
		})
	}
}

func TestBootstrapURLFollowsTheSchemeUpAndKeepsThePathPrefix(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain": {
			in:   "ws://127.0.0.1:4321/ws",
			want: "http://127.0.0.1:4321/bootstrap.json",
		},
		"tls": {
			in:   "wss://backend.example:8443/ws",
			want: "https://backend.example:8443/bootstrap.json",
		},
		"behind a reverse proxy prefix": {
			in:   "wss://edge.example/agent-overflow/ws",
			want: "https://edge.example/agent-overflow/bootstrap.json",
		},
		"query and fragment are not credentials for this fetch": {
			in:   "ws://127.0.0.1:4321/ws?token=launch#frag",
			want: "http://127.0.0.1:4321/bootstrap.json",
		},
		"root path": {
			in:   "ws://host:1234/",
			want: "http://host:1234/bootstrap.json",
		},
		"no path at all": {
			in:   "ws://host:1234",
			want: "http://host:1234/bootstrap.json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := BootstrapURL(tc.in)
			if err != nil {
				t.Fatalf("BootstrapURL(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("BootstrapURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
