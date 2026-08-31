package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// The frozen route set. buildHTTPServer mounts exactly what
// httpSurfaces returns, so a new route cannot reach the wire without
// appearing here — which is the point: docs/specs/remote-access.md §13
// requires every externally-reachable surface to carry a declared
// posture, and unclassified means unbuilt.
//
// Adding a row is a deliberate change to this list AND to the entry's
// four declared properties, in the same commit.
var frozenHTTPSurfacePatterns = []string{
	"/",
	"/bootstrap.json",
	"/healthz",
	"/rpc",
	"/ws",
}

func TestHTTPSurfaces_MatchTheFrozenRouteSet(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		// The scoped-RPC route is config-conditional, so the inventory is
		// only complete with a token registry attached.
		cfg.ScopedTokens = newTokenRegistry()
	})

	got := make([]string, 0, len(frozenHTTPSurfacePatterns))
	for _, surface := range f.srv.httpSurfaces() {
		got = append(got, surface.Pattern)
	}
	sort.Strings(got)

	if strings.Join(got, " ") != strings.Join(frozenHTTPSurfacePatterns, " ") {
		t.Fatalf("mounted routes = %v, want %v — a new route needs a declared posture in httpsurfaces.go and a row here",
			got, frozenHTTPSurfacePatterns)
	}
}

// Every route declares all four §13 properties plus the decision behind
// them. An empty field is a route nobody classified, which the spec
// treats as unbuilt rather than as a default.
func TestHTTPSurfaces_EveryRouteDeclaresItsPosture(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.ScopedTokens = newTokenRegistry()
	})

	for _, surface := range f.srv.httpSurfaces() {
		for field, value := range map[string]string{
			"Listener":       surface.Listener,
			"Principals":     surface.Principals,
			"Scope":          surface.Scope,
			"ContentPosture": surface.ContentPosture,
			"Why":            surface.Why,
		} {
			if strings.TrimSpace(value) == "" {
				t.Errorf("route %s: %s is empty", surface.Pattern, field)
			}
		}
		if surface.Handler == nil {
			t.Errorf("route %s: no handler", surface.Pattern)
		}
	}
}

// The scoped-RPC route exists only when a token registry does. Its
// absence is a real configuration, not an omission, so the inventory has
// to reflect the config it was built from rather than a static list.
func TestHTTPSurfaces_ScopedRPCAbsentWithoutTokenRegistry(t *testing.T) {
	f := newServerFixture(t)

	for _, surface := range f.srv.httpSurfaces() {
		if surface.Pattern == ScopedRPCPath {
			t.Fatalf("scoped RPC route mounted with no ScopedTokens configured")
		}
	}
}

// /healthz answers without a credential ON PURPOSE — it is the pre-WS
// compatibility check and the update watchdog's probe, and both run in
// states where no valid credential is held. See the route's Why for the
// full reasoning.
func TestServer_HealthzAnswersWithoutACredential(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.Version = "1.2.3"
		cfg.BackendIdentity = func() (string, string) { return "backend-uuid-1", "generation-7" }
	})

	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", f.srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}

	var health Health
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.Version != "1.2.3" {
		t.Fatalf("version = %q, want %q", health.Version, "1.2.3")
	}
	if health.BackendID != "backend-uuid-1" {
		t.Fatalf("backendId = %q, want %q", health.BackendID, "backend-uuid-1")
	}
}

// Unauthenticated is not the same as readable by anyone's page. Without
// an Access-Control-Allow-Origin header a foreign document may issue the
// request but can never read the answer, which is what keeps the backend
// id from becoming a cross-origin identifier.
func TestServer_HealthzIsNotCrossOriginReadable(t *testing.T) {
	f := newServerFixture(t)

	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", f.srv.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want unset", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	// A cached health answer is a stale answer, and staleness is the one
	// thing a watchdog cannot tolerate: it would report the old version
	// of a backend that has already restarted.
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// The same DNS-rebinding guard the credentialled routes use. A page that
// resolves a name to 127.0.0.1 must not be able to read the backend id
// off the loopback listener.
func TestServer_HealthzRefusesNonLoopbackHostInLoopbackMode(t *testing.T) {
	f := newServerFixture(t)

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/healthz", f.srv.Addr()), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "rebound.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("healthz status with foreign Host = %d, want 404", resp.StatusCode)
	}
}

func TestServer_HealthzRefusesNonGET(t *testing.T) {
	f := newServerFixture(t)

	resp, err := http.Post(fmt.Sprintf("http://%s/healthz", f.srv.Addr()), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); !strings.Contains(got, http.MethodGet) {
		t.Fatalf("Allow = %q, want it to name GET", got)
	}
}
