package transport

import "net/http"

// HTTP surface inventory for this listener.
//
// docs/specs/remote-access.md §13 requires every externally-reachable
// surface to be enumerated in one place with four declared properties —
// listener, principal tiers admitted, required scope, content-type
// posture — with the enumeration being CODE rather than prose, so that a
// route cannot exist without an entry.
//
// This table is the HTTP-route half of that inventory for the transport
// listener. It is not a test that checks registration: it IS the
// registration. buildHTTPServer iterates it, so a route with no declared
// posture cannot be served, and the declaration cannot drift from what is
// mounted. (An unclassified route is unbuilt, which is the same
// fail-closed shape LocalOnlyMethods and the event-channel registry use.)
//
// Scope names are the spec's §5 labels. They are DECLARATIONS today, not
// enforcement: the generated scope table lands in phase 3, and until then
// the enforced facts are the token check, the Host guard, and peer
// locality. Writing the intended scope down now is what lets phase 3
// diff intent against what it generates instead of rediscovering it.
type httpSurface struct {
	// Pattern is the ServeMux pattern this surface is mounted at.
	Pattern string
	// Handler is what serves it, wrappers already applied.
	Handler http.Handler
	// Listener names which binds carry this route. Every entry here is on
	// the one transport listener, which is loopback or LAN depending on
	// the bind; the field exists because §13's inventory spans more
	// listeners than this file (the auxiliary loopback servers) and each
	// row must say which one it is.
	Listener string
	// Principals says who is admitted, in the spec's tier vocabulary.
	Principals string
	// Scope is the §5 label required to reach it, or "none" for a surface
	// that predates the scope model and is gated by credential and
	// locality alone.
	Scope string
	// ContentPosture is the content-type contract: what bytes this route
	// emits and whose authority they carry. Agent- or user-authored bytes
	// must never execute at the SPA origin (§13, and the retired /design/
	// route that motivated the rule).
	ContentPosture string
	// Why records the decision behind the three classifications above.
	Why string
}

// httpSurfaces returns the routes this server mounts, in mount order.
//
// Composed per call rather than stored: Rebind rebuilds the http.Server,
// and the ScopedTokens route's existence depends on config. A slice built
// at boot would have to be rebuilt anyway, and a stale one would be a
// registration that no longer matches the config it claims to describe.
func (s *Server) httpSurfaces() []httpSurface {
	assetH := s.cfg.AssetHandler
	if assetH == nil {
		assetH = http.NotFoundHandler()
	}
	assetFinal := withAssetHeaders(assetH)
	if s.cfg.CrossOriginIsolate {
		assetFinal = withCrossOriginIsolation(assetFinal)
	}

	surfaces := []httpSurface{
		{
			Pattern:        "/bootstrap.json",
			Handler:        s.loopbackHostGuard(s.handleBootstrap),
			Listener:       "transport (loopback or LAN per bind)",
			Principals:     "holder of this launch's session token",
			Scope:          "none (pre-scope: the token IS the credential)",
			ContentPosture: "application/json, server-authored, no-store",
			Why: "Hands out the session credential itself, so it is the one " +
				"route whose disclosure is the whole game. Token-gated, and " +
				"Host-guarded in loopback mode so a DNS-rebinding page cannot " +
				"read it. A wrong token answers 404, indistinguishable from " +
				"no such path.",
		},
		{
			Pattern:        "/healthz",
			Handler:        s.loopbackHostGuard(s.handleHealthz),
			Listener:       "transport (loopback or LAN per bind)",
			Principals:     "anyone who can reach the listener",
			Scope:          "none (deliberately unauthenticated; see Why)",
			ContentPosture: "application/json, server-authored, no-store, no CORS header",
			Why: "The pre-WS compatibility check and the update watchdog's " +
				"probe (spec §9). Both run in states where a credential is " +
				"either not yet held or no longer valid: a watchdog must tell " +
				"'backend restarted on a new version' from 'backend down', " +
				"and a token-gated health route answers 404 for the restarted " +
				"case — indistinguishable from down, which is precisely the " +
				"condition it exists to detect. What it discloses is version " +
				"and backend id to peers who can already fetch the unauthenticated " +
				"SPA bundle from the same listener, so gating it would cost the " +
				"function without buying the secrecy. Cross-origin reads are " +
				"still refused: no Access-Control-Allow-Origin, plus the same " +
				"Host guard the credentialled routes use.",
		},
		{
			Pattern:        "/ws",
			Handler:        s.loopbackHostGuard(s.handleWS),
			Listener:       "transport (loopback or LAN per bind)",
			Principals:     "holder of this launch's session token, from an allow-listed Origin on LAN binds",
			Scope:          "none (pre-scope; per-method LocalOnly refusal by peer locality)",
			ContentPosture: "WebSocket, JSON frames, server-authored",
			Why: "The RPC and event wire. Token checked before upgrade, " +
				"Origin checked against the live allow-list on LAN binds, and " +
				"peer locality captured at upgrade drives LocalOnlyMethods and " +
				"event visibility for the connection's lifetime.",
		},
	}

	if s.cfg.ScopedTokens != nil {
		surfaces = append(surfaces, httpSurface{
			Pattern:        ScopedRPCPath,
			Handler:        s.loopbackHostGuard(s.handleScopedRPC),
			Listener:       "transport, loopback peers only",
			Principals:     "holder of an `ao` CLI scoped token (interactive or phase)",
			Scope:          "per-method, from ScopedTokenMethods",
			ContentPosture: "application/json, one frame in and one out, server-authored",
			Why: "A strictly narrower credential class than the session " +
				"token, which is deliberately NOT honoured here. Non-loopback " +
				"peers get 404 so the route stays unfingerprintable. Registered " +
				"only when a token registry exists.",
		})
	}

	// Mounted last: "/" is ServeMux's catch-all, so every more specific
	// pattern above must already be registered or it would still match
	// (ServeMux is longest-pattern-wins, but keeping mount order equal to
	// declaration order makes the table readable as the routing story).
	surfaces = append(surfaces, httpSurface{
		Pattern:        "/",
		Handler:        assetFinal,
		Listener:       "transport (loopback or LAN per bind)",
		Principals:     "anyone who can reach the listener",
		Scope:          "none",
		ContentPosture: "the embedded SPA bundle: build-authored bytes at the SPA origin",
		Why: "The bundle is served unauthenticated by necessity — the page " +
			"has to load before it can present a credential. Only bytes " +
			"embedded at build time are served here; agent- or user-authored " +
			"content must never be reachable at this origin, which is the " +
			"lesson of the retired /design/ route (§13).",
	})

	return surfaces
}
