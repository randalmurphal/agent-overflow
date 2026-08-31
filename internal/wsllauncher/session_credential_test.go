package wsllauncher

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
// spellings this package restates. The launcher deliberately does not link
// the transport server, so a rename on the server side that missed these
// copies would silently stop the launcher forwarding its credential — and
// only a live Windows session would notice, by nothing at all going wrong.
func TestSessionCarriersMatchTransport(t *testing.T) {
	if got := textproto.CanonicalMIMEHeaderKey(transport.SessionCredentialHeader); got != transportSessionHeader {
		t.Fatalf("transportSessionHeader = %q, transport's is %q", transportSessionHeader, got)
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
	if !strings.HasPrefix(planted[0].Name, sessionCookiePrefix) {
		t.Fatalf("transport names the session cookie %q, which does not start with %q",
			planted[0].Name, sessionCookiePrefix)
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
				Name: sessionCookiePrefix + "4321", Value: credential, Path: "/", HttpOnly: true,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"wsUrl":"ws://example/ws"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLauncherReadsTheCredentialOutOfTheBootstrapExchange(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	stub := bootstrapStub(t, "ao1.local-credential", status)

	source := newSessionCredentialSource(strings.Replace(stub.URL, "http://", "ws://", 1)+"/ws", "launch-token")
	if got := source.credentialFor(context.Background(), false); got != "ao1.local-credential" {
		t.Fatalf("credential = %q, want the one the exchange planted", got)
	}
}

// TestLauncherKeepsWorkingWhenThereIsNoCredential — an older backend, or
// one whose session core did not start. Forwarding is an improvement in
// attribution, never a new requirement for the launcher to connect.
func TestLauncherKeepsWorkingWhenThereIsNoCredential(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	stub := bootstrapStub(t, "", status)

	source := newSessionCredentialSource(strings.Replace(stub.URL, "http://", "ws://", 1)+"/ws", "launch-token")
	if got := source.credentialFor(context.Background(), false); got != "" {
		t.Fatalf("credential = %q, want empty", got)
	}
}

// TestLauncherKeepsAWorkingCredentialAcrossABootingBackend — the backend
// answers 503 while it starts, and discarding what we hold over that would
// drop attribution for every reconnect until it came back.
func TestLauncherKeepsAWorkingCredentialAcrossABootingBackend(t *testing.T) {
	status := &atomic.Int32{}
	status.Store(http.StatusOK)
	stub := bootstrapStub(t, "ao1.local-credential", status)

	source := newSessionCredentialSource(strings.Replace(stub.URL, "http://", "ws://", 1)+"/ws", "launch-token")
	if got := source.credentialFor(context.Background(), false); got == "" {
		t.Fatal("the first fetch answered nothing")
	}
	status.Store(http.StatusServiceUnavailable)
	if got := source.credentialFor(context.Background(), true); got != "ao1.local-credential" {
		t.Fatalf("a forced refresh against a booting backend answered %q", got)
	}
}

func TestSessionCredentialSourceRefusesToGuessAnAuthority(t *testing.T) {
	source := newSessionCredentialSource("://not a url", "launch-token")
	if source.bootstrapURL != "" {
		t.Fatalf("an unparseable ws URL produced the bootstrap URL %q", source.bootstrapURL)
	}
	if got := source.credentialFor(context.Background(), false); got != "" {
		t.Fatalf("credential = %q from a source that can fetch nothing", got)
	}
}

func TestSessionCredentialSourceFollowsTheSchemeUp(t *testing.T) {
	source := newSessionCredentialSource("wss://backend.example:8443/ws", "launch-token")
	if source.bootstrapURL != "https://backend.example:8443/bootstrap.json" {
		t.Fatalf("bootstrapURL = %q", source.bootstrapURL)
	}
}
