package transport

import (
	"net"
	"net/http"
	"strings"
)

// ContentSecurityPolicy is a complete, prebuilt Content-Security-Policy
// header value. Only the two constants below exist: the policy is chosen
// once, at server construction, from the boot mode, and every response
// then writes that same string. Nothing is assembled per request
// (spec §14: "CSP and security headers are constant strings set from a
// prebuilt header block, with no per-request construction").
//
// It is a named type rather than a bare string so WriteSecurityHeaders
// cannot be called without naming a policy — a route that forgets is a
// compile error, not a response that quietly ships without a CSP.
type ContentSecurityPolicy string

// CSPProduction is the policy for every response serving the embedded
// SPA bundle — this package's asset handler and clientmode's stub alike.
// Each directive was derived from what the shipped bundle was observed
// to load, not from a template:
//
//   - default-src 'self' is the floor every unlisted directive falls
//     back to (media, worker, manifest, frame). The bundle loads none of
//     those today, so the floor is what a future one trips.
//   - script-src 'self', with no 'unsafe-inline' and no 'unsafe-eval'.
//     The production bundle carries no eval, no new Function, no
//     WebAssembly and no Worker. The first-paint theme script lives in
//     its own file (frontend/public/boot-theme.js) precisely so this
//     directive needs neither a hash nor a nonce: a hash would have to
//     track bytes the frontend build owns, and a nonce would mean
//     building this header per request.
//   - style-src needs 'unsafe-inline' and cannot be nonced. Svelte
//     writes style attributes on every render, KaTeX and mermaid inject
//     their own, and inline style ATTRIBUTES are not noncable at all.
//   - img-src admits http: and https: because chat markdown renders
//     remote images by design; data: carries the spinner sprite strips
//     and the in-app browser's frame JPEGs; blob: carries attachment
//     previews and the markdown image host.
//   - font-src needs data: beside 'self': the frontend build inlines
//     small woff2 faces as data URIs and serves the rest from /assets.
//   - connect-src 'self' covers both the manifest fetch and the
//     WebSocket, which is same-origin by construction — the manifest's
//     wsUrl is derived from the request's own Host (deriveWSURL) and the
//     SPA refuses a wsUrl that is not same-origin.
//   - object-src 'none', base-uri 'self', form-action 'none' and
//     frame-ancestors 'none' cost nothing: the app embeds no plugins,
//     rewrites no base URI, submits no form (every <form> is an onsubmit
//     handler that preventDefaults) and is never framed. frame-ancestors
//     is the modern half of the X-Frame-Options: DENY below.
const CSPProduction ContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: http: https:; " +
	"font-src 'self' data:; " +
	"connect-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// CSPDevServer is CSPProduction with exactly one directive relaxed, for
// the boot whose AssetHandler proxies a live Vite dev server
// (Config.DevAssetProxy).
//
// The relaxation is connect-src, and only connect-src. Vite's HMR client
// opens its socket at the origin it was itself served from — this server,
// when it is proxying — but falls back to a DIRECT socket to the
// bundler's own host:port when that first attempt fails, and that
// fallback address is baked into the client when the dev server starts
// (`directSocketHost` in @vite/client). It names a different port from
// the one the page loaded, so 'self' cannot cover it, and the address is
// not knowable where this constant is written.
//
// Nothing else is relaxed, because observation found nothing else that
// needed it: @vite/client arrives as an external module script, the
// Svelte and Tailwind dev plugins inject their CSS through JS-created
// <style> elements that style-src 'unsafe-inline' already admits, and
// neither the client nor the modules it serves use eval or new Function.
// Dev mode is not a licence to widen script-src; this is the one
// directive HMR genuinely cannot reach otherwise.
const CSPDevServer ContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: http: https:; " +
	"font-src 'self' data:; " +
	"connect-src 'self' ws: wss:; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// WriteSecurityHeaders writes the standard security headers used by
// every HTTP-side response in this package and in clientmode (which
// imports this helper). The set is deliberately small and conservative:
//
//   - Content-Security-Policy: the caller's prebuilt policy, unmodified.
//     Passing it rather than reading a package global is what keeps the
//     strict/relaxed choice a single decision at server construction.
//   - X-Content-Type-Options: nosniff refuses content-type guessing.
//   - X-Frame-Options: DENY refuses framing by any parent document; the
//     CSP's frame-ancestors 'none' says the same thing to newer engines.
//   - Referrer-Policy: no-referrer keeps a page URL's one-time ticket
//     out of outbound referers if the SPA navigates externally.
//
// Cache-Control is intentionally NOT set here — callers pick the right
// caching policy per route (immutable for hashed assets, no-store for
// the SPA shell + bootstrap, no-cache for the SPA fallback).
func WriteSecurityHeaders(h http.Header, csp ContentSecurityPolicy) {
	h.Set("Content-Security-Policy", string(csp))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}

// WriteCrossOriginIsolationHeaders opts the response into cross-origin
// isolation so the SPA runs with `crossOriginIsolated === true` and
// performance.measureUserAgentSpecificMemory becomes available.
// Diagnostic mode only (Config.CrossOriginIsolate, set via
// AGENT_OVERFLOW_RENDERER_DIAG): COEP require-corp blocks subresources
// that don't send CORP, including remote images in chat markdown.
// CORP: same-origin is included on every response as the matching resource policy.
func WriteCrossOriginIsolationHeaders(h http.Header) {
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Embedder-Policy", "require-corp")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// IsLoopbackHost reports whether the request's Host header names a
// loopback interface. Used by the DNS-rebinding defence in both the
// embedded-webview server (server.go) and the --connect stub
// (clientmode.go) so the same rule applies in both deployments.
//
// Accepts: "127.0.0.1", "127.0.0.1:<port>", "localhost",
// "localhost:<port>", "[::1]", "[::1]:<port>". Empty Host is rejected
// (HTTP/1.1 requires it; allowing empty would punch a hole in the
// rebind defence for handcrafted clients).
//
// Anything else, including non-loopback IPv4/IPv6 literals or arbitrary
// DNS names that happen to resolve to 127.0.0.1, is rejected.
func IsLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	hostOnly, _, err := net.SplitHostPort(host)
	if err != nil {
		// SplitHostPort fails on bare hosts and malformed inputs.
		// A bracketed IPv6 with no port (`[::1]`) is the legitimate
		// no-port case; anything else with a stray colon is malformed
		// and we refuse rather than guess.
		if strings.Contains(host, ":") {
			if !(strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]")) {
				return false
			}
			hostOnly = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
		} else {
			hostOnly = host
		}
	}
	switch strings.ToLower(hostOnly) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}
