package surfaces

import (
	"strings"
	"testing"
)

// These are the shape rules for the inventory itself. The gate in
// surfaces_gate_test.go checks the rows against the code; these check
// that a row is worth reading — a row whose Credential is a typo, or
// whose Why restates its fields, passes the gate and tells the next
// reader nothing.

var (
	bindingClasses = map[BindingClass]bool{
		BindLoopback:   true,
		BindLANCapable: true,
	}
	credentials = map[Credential]bool{
		CredPageSession:      true,
		CredBearerToken:      true,
		CredTransferGrant:    true,
		CredCapabilityHeader: true,
		CredUnguessablePath:  true,
		CredPeerLocality:     true,
		CredNone:             true,

		CredSessionCredential: true,
		CredPairingToken:      true,
		CredRefreshSecret:     true,
		CredPasskeyAssertion:  true,
		CredTransferTicket:    true,
	}
	postures = map[ContentPosture]bool{
		PostureAppOrigin:   true,
		PostureStructured:  true,
		PostureProxied:     true,
		PostureDiagnostic:  true,
		PostureOpaqueMedia: true,
		PostureNone:        true,
	}
	authors = map[BytesAuthor]bool{
		AuthorBuild:       true,
		AuthorRuntime:     true,
		AuthorUpstream:    true,
		AuthorAgentOrUser: true,
	}
)

// minimumWhy is a floor, not a quality bar. It rules out the empty
// string and the one-word placeholder; it cannot rule out a bad
// sentence, and nothing here pretends otherwise.
const minimumWhy = 40

func TestListenerRowsAreWellFormed(t *testing.T) {
	names := map[string]bool{}
	for i, listener := range Listeners {
		if listener.Name == "" {
			t.Fatalf("Listeners[%d] has no name", i)
		}
		if names[listener.Name] {
			t.Errorf("Listeners has %q twice; names key the Route and Origin rows", listener.Name)
		}
		names[listener.Name] = true

		if listener.Package == "" {
			t.Errorf("Listeners[%q] names no owning package", listener.Name)
		}
		if !bindingClasses[listener.Binding] {
			t.Errorf("Listeners[%q].Binding = %q, which is not a declared BindingClass", listener.Name, listener.Binding)
		}
		if !credentials[listener.Credential] {
			t.Errorf("Listeners[%q].Credential = %q, which is not a declared Credential", listener.Name, listener.Credential)
		}
		if !postures[listener.Posture] {
			t.Errorf("Listeners[%q].Posture = %q, which is not a declared ContentPosture", listener.Name, listener.Posture)
		}
		if len(listener.Why) < minimumWhy {
			t.Errorf("Listeners[%q].Why is %d chars; say what capability sits behind the credential, not what the fields already say", listener.Name, len(listener.Why))
		}
		// Implicit and Sites are two spellings of one fact, so pin them
		// to each other. Otherwise a row that lost its Sites — a typo, a
		// bad merge — would silently reclassify itself as a listener a
		// child process opens, and the forward gate would stop checking
		// it.
		if listener.Implicit != (len(listener.Sites) == 0) {
			t.Errorf("Listeners[%q]: Implicit=%v with %d site(s); a row has source files or it is implicit, never both and never neither",
				listener.Name, listener.Implicit, len(listener.Sites))
		}
		for _, site := range listener.Sites {
			if strings.Contains(site, "\\") {
				t.Errorf("Listeners[%q] site %q uses a backslash; Sites are slash-separated repository-relative paths", listener.Name, site)
			}
			if !strings.HasPrefix(site, listener.Package+"/") {
				t.Errorf("Listeners[%q] site %q is outside its declared package %q", listener.Name, site, listener.Package)
			}
		}
	}
}

func TestRouteRowsAreWellFormed(t *testing.T) {
	byListener := map[string]Listener{}
	for _, listener := range Listeners {
		byListener[listener.Name] = listener
	}

	seen := map[string]bool{}
	for i, route := range Routes {
		if route.Pattern == "" {
			t.Fatalf("Routes[%d] has no pattern", i)
		}
		key := route.Listener + "\x00" + route.Pattern
		if seen[key] {
			t.Errorf("Routes has %q on %q twice", route.Pattern, route.Listener)
		}
		seen[key] = true

		listener, ok := byListener[route.Listener]
		if !ok {
			t.Errorf("Routes[%q on %q] names a listener no row declares", route.Pattern, route.Listener)
			continue
		}
		if listener.Implicit {
			t.Errorf("Routes[%q] is served from %q, which is implicit; we register no routes on a listener a child process opens", route.Pattern, route.Listener)
		}
		if !credentials[route.Credential] {
			t.Errorf("Routes[%q on %q].Credential = %q, which is not a declared Credential", route.Pattern, route.Listener, route.Credential)
		}
		if !postures[route.Posture] {
			t.Errorf("Routes[%q on %q].Posture = %q, which is not a declared ContentPosture", route.Pattern, route.Listener, route.Posture)
		}
		if len(route.Why) < minimumWhy {
			t.Errorf("Routes[%q on %q].Why is %d chars; too short to say anything the fields do not", route.Pattern, route.Listener, len(route.Why))
		}
	}
}

func TestOriginRowsAreWellFormed(t *testing.T) {
	byListener := map[string]Listener{}
	for _, listener := range Listeners {
		byListener[listener.Name] = listener
	}

	names := map[string]bool{}
	for i, origin := range Origins {
		if origin.Name == "" {
			t.Fatalf("Origins[%d] has no name", i)
		}
		if names[origin.Name] {
			t.Errorf("Origins has %q twice", origin.Name)
		}
		names[origin.Name] = true

		if _, ok := byListener[origin.Listener]; !ok {
			t.Errorf("Origins[%q] names a listener no row declares: %q", origin.Name, origin.Listener)
		}
		if !authors[origin.Author] {
			t.Errorf("Origins[%q].Author = %q, which is not a declared BytesAuthor", origin.Name, origin.Author)
		}
		if !postures[origin.Posture] {
			t.Errorf("Origins[%q].Posture = %q, which is not a declared ContentPosture", origin.Name, origin.Posture)
		}
		if len(origin.Why) < minimumWhy {
			t.Errorf("Origins[%q].Why is %d chars; say whose bytes these are and how they are constrained", origin.Name, len(origin.Why))
		}
	}
}

// TestEveryServingListenerHasAnOrigin holds the two lists against each
// other: a listener that answers with bytes is an origin, and pretending
// otherwise is how a surface stops being described. PostureNone is the
// only listener that legitimately has no origin.
func TestEveryServingListenerHasAnOrigin(t *testing.T) {
	served := map[string]bool{}
	for _, origin := range Origins {
		served[origin.Listener] = true
	}
	for _, listener := range Listeners {
		switch {
		case listener.Posture == PostureNone && served[listener.Name]:
			t.Errorf("Listeners[%q] serves no bytes but an Origin row claims it does", listener.Name)
		case listener.Posture != PostureNone && !served[listener.Name]:
			t.Errorf("Listeners[%q] answers with %q but no Origin row describes whose bytes those are", listener.Name, listener.Posture)
		}
	}
}

// TestAuthoredBytesNeverExecute is the rule /design/ broke: it served
// agent-written files at the SPA origin, where the bundle's own code runs
// and the page credential lives.
//
// It stopped being a tripwire over an empty set in wave 6b. Attachment
// bytes — a screenshot the person pasted — now leave the app transport as
// bytes rather than as data inside a WebSocket frame, so one row honestly
// carries AuthorAgentOrUser and this test has something to check. What
// keeps it passing is the POSTURE, not the absence of authored content:
// those bytes are opaque media under a signature-verified image
// Content-Type with nosniff, which an engine paints and never parses.
// Provider output still reaches the page as data over the WebSocket and
// is rendered by bundle code.
func TestAuthoredBytesNeverExecute(t *testing.T) {
	for _, origin := range Origins {
		if origin.Author == AuthorAgentOrUser && origin.Posture == PostureAppOrigin {
			t.Errorf("Origins[%q] serves agent- or user-authored bytes at an origin where the app executes. "+
				"Serve them from a distinct origin, or deliver them as data the bundle renders.", origin.Name)
		}
	}
}

// TestRegistryRowsAreWellFormed holds the same bar the three surface
// tables are held to, plus the one property that is specific to a
// reference row: it has to be anchored to a surface this package already
// enumerates. A registry pointing at a listener or a route nobody
// declares is describing a door that does not exist.
func TestRegistryRowsAreWellFormed(t *testing.T) {
	byListener := map[string]Listener{}
	for _, listener := range Listeners {
		byListener[listener.Name] = listener
	}
	routesOn := map[string]bool{}
	for _, route := range Routes {
		routesOn[route.Listener+"\x00"+route.Pattern] = true
	}

	seen := map[string]bool{}
	for i, registry := range Registries {
		if registry.Name == "" {
			t.Fatalf("Registries[%d] has no name", i)
		}
		if seen[registry.Name] {
			t.Errorf("Registries has %q twice", registry.Name)
		}
		seen[registry.Name] = true

		listener, ok := byListener[registry.Listener]
		if !ok {
			t.Errorf("Registries[%q] names a listener no row declares: %q", registry.Name, registry.Listener)
		} else if listener.Implicit {
			t.Errorf("Registries[%q] rides %q, which is implicit; we dispatch nothing on a listener a child process opens", registry.Name, registry.Listener)
		}
		if len(registry.Routes) == 0 {
			t.Errorf("Registries[%q] names no route; a registry nothing is reached over is not a registry", registry.Name)
		}
		for _, pattern := range registry.Routes {
			if !routesOn[registry.Listener+"\x00"+pattern] {
				t.Errorf("Registries[%q] names route %q on %q, which no Route row declares", registry.Name, pattern, registry.Listener)
			}
		}

		if registry.Source == "" || registry.Symbol == "" {
			t.Errorf("Registries[%q] does not say where its table lives", registry.Name)
		}
		if len(registry.RowFields) == 0 {
			t.Errorf("Registries[%q] names no required row field; the gate would then assert nothing about its entries", registry.Name)
		}
		if len(registry.Gates) == 0 {
			t.Errorf("Registries[%q] names no gate; a table nothing reads still looks complete", registry.Name)
		}
		if len(registry.Why) < minimumWhy {
			t.Errorf("Registries[%q].Why is %d chars; too short to say anything the fields do not", registry.Name, len(registry.Why))
		}
	}
}
