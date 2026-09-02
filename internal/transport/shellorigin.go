package transport

import (
	"net/http"
	"os"
	"strings"
)

// The one page origin that is not this backend's own, and the CORS answer
// that admits it (docs/specs/remote-access.md § "The phone client", and
// §10 "One seam, two realizations").
//
// Every client before wave 6f-c was served its bundle by the backend it
// then talked to, so "the origin this request was addressed to" was the
// whole allow-list and `OriginAllowed` needed nothing else. The phone
// shell serves the SAME bundle from a fixed origin of its own and reaches
// the backend across the tailnet, so its requests are cross-origin by
// construction — not by configuration, and not per install.
//
// **Why one constant beats a pattern.** `shell.agent-overflow.invalid` is
// under the reserved `.invalid` TLD (RFC 6761 §6.4), which no resolver
// answers and no registry sells, so no page anywhere on a network can
// ever hold this origin — the shell has it because Capacitor assigns the
// WebView's document that authority locally. A pattern, a per-install
// setting, or a "any https origin the owner adds" knob would each be a
// wider door than the exact string that cannot be reached.
//
// **What admitting it does and does not mean.** It admits the origin, and
// nothing else: every route behind it still demands its own credential —
// the session credential plus a device-key proof for the auth exchanges
// and the manifest, a single-use ticket for the attachment bytes, a
// ticket or the page credential for the socket. What the CORS answer adds
// is the browser's permission for the shell page to READ the response it
// was already allowed to cause.

// ShellOrigin is the phone shell's page origin, fixed at build time by
// the Capacitor config (`mobile/capacitor.config.ts`: `androidScheme:
// 'https'` plus `hostname`). Change one and the other stops working, so
// the two are named in each other's comments.
const ShellOrigin = "https://shell.agent-overflow.invalid"

// ShellOriginExtraEnv names ONE additional admitted page origin, and is
// harness-only: it exists so `e2e/tests/compact-shell-origin.spec.ts` can
// serve the bundle from a throwaway HTTP server on an ephemeral port and
// prove this whole path against the real Go server, which no fixed
// constant can do. Nothing in a shipped build sets it, no setting writes
// it, and it is read fresh per request rather than cached so a test can
// launch a backend with it and a following one without.
//
// It is deliberately an env var rather than a config field: a config
// field is a knob somebody can turn on in a running install, while an env
// var is set by whoever spawns the process — which for this backend is
// either the user's own shell or the harness launcher.
const ShellOriginExtraEnv = "AO_SHELL_ORIGIN_EXTRA"

// shellOriginAllowed reports whether raw is a page origin this backend
// serves cross-origin, and is the ONE place that answers it: both
// `OriginAllowed` (which the WebSocket upgrade also runs) and the CORS
// middleware call it, so an origin that may open a socket and an origin
// that may read a response can never drift apart.
func shellOriginAllowed(raw string) bool {
	if raw == "" {
		return false
	}
	if strings.EqualFold(raw, ShellOrigin) {
		return true
	}
	extra := os.Getenv(ShellOriginExtraEnv)
	return extra != "" && strings.EqualFold(raw, extra)
}

// The headers a shell page presents and reads. Named rather than
// wildcarded, because `*` is not permitted to name a header a
// credentialled request sends and because the list IS the statement of
// what this door is for.
var (
	// What the client sends: the paired session credential, the
	// device-key proof bound to (method, path), and the content type of
	// the JSON bodies the auth routes take.
	shellAllowedRequestHeaders = strings.Join([]string{
		SessionCredentialHeader,
		DeviceKeyHeader,
		"Content-Type",
	}, ", ")

	// What the client reads off the answer. Content-Type is already a
	// CORS-safelisted response header; the two the attachment routes set
	// are not, and a shell that could not read them would mis-render a
	// download it was otherwise allowed to make.
	shellExposedResponseHeaders = strings.Join([]string{
		"Content-Type",
		"Content-Length",
		"Cache-Control",
	}, ", ")
)

// withShellCORS answers the cross-origin preflight for one route and
// stamps the allow headers on its real response.
//
// Four rules it keeps, each of which is the thing that would otherwise go
// wrong:
//
//   - **One origin, echoed exactly, never `*`.** A wildcard would admit
//     every page on the internet to read whatever a ticket in a URL
//     happens to authorize.
//   - **`Vary: Origin` unconditionally**, including on the same-origin
//     answer that carries no allow header at all. The response body for
//     an admitted origin and for a foreign one are the same bytes with
//     different headers, so a cache keyed without the origin would serve
//     one page the other's permission.
//   - **No `Access-Control-Allow-Credentials`.** A shell page holds no
//     cookie for this backend and presents its session in a header. The
//     flag would ask browsers to attach ambient credentials to these
//     requests, which is precisely the thing `OriginAllowed` exists to
//     stop, and it is what makes the wildcard ban a rule rather than a
//     habit.
//   - **A foreign origin gets NOTHING added.** Not a refusal, not a
//     distinguishable status: the route answers exactly as it always did
//     and the browser withholds the body itself, so this middleware
//     cannot be used to ask which origins a backend knows about.
//
// `methods` is what this route serves, for the preflight's answer. A
// preflight is answered BEFORE the wrapped handler, and before any rate
// limiter the caller composed outside it: it carries no credential, does
// no work, and a shell whose preflights were being throttled would fail
// in a way nothing on the page could explain.
func withShellCORS(methods string, next http.HandlerFunc) http.HandlerFunc {
	allowMethods := methods + ", " + http.MethodOptions
	return func(w http.ResponseWriter, r *http.Request) {
		// Stamped on every answer this route gives, admitted or not:
		// the header set varies by origin even when it varies by
		// becoming empty.
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if !shellOriginAllowed(origin) {
			// Includes every same-origin request, which is the case this
			// branch exists to leave byte-identical to what it was
			// before this file.
			next(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", shellExposedResponseHeaders)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", allowMethods)
			h.Set("Access-Control-Allow-Headers", shellAllowedRequestHeaders)
			// Ten minutes, which is Chromium's own ceiling for a
			// preflight cache. Longer is silently clamped, and shorter
			// buys a preflight per exchange on a link the phone pays
			// latency on.
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// shellPreflightHandler answers OPTIONS for a route the mux registered
// method-qualified (the attachment pair). Such a pattern makes the mux
// itself answer 405 to an OPTIONS request, which a browser reads as "this
// route refuses the preflight" and the whole transfer never starts — so
// each of them registers its own OPTIONS pattern pointing here.
//
// A preflight for an origin this backend does not serve gets the same 404
// every unadmitted thing on this listener gets, rather than a 405 that
// would confirm the route exists.
func shellPreflightHandler(methods string) http.HandlerFunc {
	return withShellCORS(methods, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
}
