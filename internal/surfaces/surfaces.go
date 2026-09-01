// Package surfaces is the authored inventory of every network surface
// this repository opens, plus the origins those surfaces serve bytes
// from. It holds no behaviour: nothing dispatches on it, and importing
// it costs one slice of structs. Its value is that the gate in
// surfaces_gate_test.go reads the source tree and fails when a listener
// or an HTTP route exists here without a row — so a new surface cannot
// arrive unnoticed, only unremarked-upon.
//
// Why an inventory rather than a review habit. The worked case is
// /design/, which served agent-written files at the SPA origin with no
// credential, no response headers, no per-thread check and symlinks
// unresolved. It was found by an audit and closed by deleting the
// feature. Nothing about its construction was unusual; what was unusual
// was that somebody happened to look. This package is the part that does
// not depend on somebody happening to look.
//
// Each row records the four properties that decide what a surface is
// worth: where it binds, what credential it demands, what bytes leave
// it, and a Why in the register internal/transport's channelPolicies
// uses — the reasoning, not a restatement of the fields. The point of
// the Why is the case where the credential is weaker than the capability
// behind it. The browser MCP endpoint is the live example: an
// unguessable path is all that stands in front of page evaluation and
// workspace file reads, which is a much larger grant than "carries no
// session credential" makes it sound.
//
// Scope is docs/specs/remote-access.md §13. Listeners, HTTP routes, and
// content origins landed in phase 0 and are enumerated ROW BY ROW,
// because each one is a decision somebody made once. RPC methods and
// event channels landed in phase 3 and are enumerated as REGISTRIES —
// one row each, naming the authored table and the gate that reads it —
// because 360 methods and 72 channels are generated or table-driven, and
// copying them here would produce a second list whose only reliable
// property is disagreeing with the first. The Registry row records what
// this package can say that the tables cannot say about themselves:
// which listener carries them, over which routes, and what decides one
// entry for one caller.
package surfaces

// BindingClass is what a listener's bind address admits. It is a
// property of the address the code can be configured to bind, not of the
// address a particular boot happened to use: a listener that reaches the
// LAN only when the user turns it on is still LAN-capable.
type BindingClass string

const (
	// BindLoopback means the bind address is loopback and stays
	// loopback — either a hard-coded literal or a configured value the
	// code refuses unless it is loopback. Reachability is then bounded
	// by the host, and every process on that host is inside the
	// boundary.
	BindLoopback BindingClass = "loopback"

	// BindLANCapable means the bind address is configurable and can
	// name an interface other hosts can reach. Everything about the
	// surface has to hold against a peer that is not on this machine.
	BindLANCapable BindingClass = "lan-capable"
)

// Credential is what a caller must present to be answered. It names the
// strongest thing checked before dispatch; validation that is not a
// secret (peer locality, Origin, Content-Type) belongs in the Why,
// because a check the caller cannot fail is not a credential.
type Credential string

const (
	// CredPageSession is the transport's page credential: an HttpOnly
	// port-qualified cookie, an Authorization: Bearer header, or
	// ?token= for clients that can only build a URL. One validation
	// function, three carriers (internal/transport/credential.go).
	CredPageSession Credential = "page session credential"

	// CredBearerToken is a process-lifetime random token presented in
	// an Authorization header and compared in constant time.
	CredBearerToken Credential = "bearer token"

	// CredCapabilityHeader is a process-lifetime random token presented
	// in a purpose-named header rather than Authorization, because the
	// caller is a provider hook rather than an HTTP client we wrote.
	CredCapabilityHeader Credential = "capability token header"

	// CredUnguessablePath is a random component of the request path and
	// nothing else. It is a real secret, but it travels in the request
	// line rather than a header, so it lands in any log that records
	// URLs.
	CredUnguessablePath Credential = "unguessable path"

	// CredPeerLocality is no secret at all: the surface answers any
	// caller whose kernel-reported peer address is loopback. Every
	// process on the host is authorized.
	CredPeerLocality Credential = "peer locality only"

	// CredSessionCredential is a durable, signed session credential
	// (internal/identity): it names a device, a user, and a scope set,
	// verifies against a stored signing key, and stops working the
	// instant its row is revoked. The only credential on this list a
	// person can withdraw.
	CredSessionCredential Credential = "session credential"

	// CredPairingToken is a single-use pairing token, spent by the first
	// caller that presents it and dead within minutes either way. It
	// authenticates nothing about WHO is asking — that is what the
	// device key and the owner's verification-number match are for — it
	// authenticates only that the caller received a link the owner
	// produced.
	CredPairingToken Credential = "single-use pairing token"

	// CredRefreshSecret is one link of a rotating refresh chain,
	// presented together with the device key it was issued to. Spending
	// a link twice is what reveals a copy, so this credential's value is
	// as much in its reuse being detectable as in its being secret.
	CredRefreshSecret Credential = "rotating refresh secret"

	// CredPasskeyAssertion is a WebAuthn signature over a challenge this
	// backend issued moments ago, made by a credential the owner
	// registered from an already-authenticated surface. Not a bearer
	// secret at all: nothing transmitted here can be replayed, because
	// the challenge is single-use and the private key never leaves the
	// authenticator.
	CredPasskeyAssertion Credential = "passkey assertion"

	// CredNone is no check of any kind. Only defensible on a listener
	// that serves nothing, or one whose entire boundary is the bind
	// address plus an explicit opt-in.
	CredNone Credential = "none"
)

// ContentPosture is what leaves the surface, judged by what an engine
// would do with it rather than by what the handler calls it.
type ContentPosture string

const (
	// PostureAppOrigin is HTML, JavaScript and CSS that execute as the
	// application at that origin. Only the build's own bundle may ever
	// carry this posture; see Origin and BytesAuthor.
	PostureAppOrigin ContentPosture = "app origin"

	// PostureStructured is JSON or a framed protocol. Nothing an engine
	// treats as a document, which is what X-Content-Type-Options:
	// nosniff is there to keep true.
	PostureStructured ContentPosture = "structured"

	// PostureProxied is bytes relayed unchanged from a fixed upstream.
	// The posture is the upstream's; this process adds nothing and
	// inspects nothing.
	PostureProxied ContentPosture = "proxied"

	// PostureDiagnostic is developer-facing diagnostic output: an HTML
	// index and binary profile blobs, served with no security headers
	// because the stdlib handler writes them and we do not wrap it.
	PostureDiagnostic ContentPosture = "diagnostic"

	// PostureNone is a listener that serves no bytes at all.
	PostureNone ContentPosture = "none"
)

// BytesAuthor says who wrote the bytes an Origin serves. The rule the
// inventory exists to keep is that AuthorAgentOrUser never appears
// beside PostureAppOrigin: bytes an agent or the user authored do not
// execute where the application executes. TestAuthoredBytesNeverExecute
// enforces it.
type BytesAuthor string

const (
	// AuthorBuild is the embedded frontend bundle. Its bytes are fixed
	// at build time and change only by shipping a new binary.
	AuthorBuild BytesAuthor = "build"

	// AuthorRuntime is this process describing itself — JSON answers,
	// protocol frames, diagnostic output.
	AuthorRuntime BytesAuthor = "runtime"

	// AuthorUpstream is a remote service's bytes, relayed unchanged.
	AuthorUpstream BytesAuthor = "upstream"

	// AuthorAgentOrUser is content a provider process or the person
	// using the app produced. No origin carries it today. The constant
	// exists so that adding one is a decision somebody writes down
	// rather than a property nobody names.
	AuthorAgentOrUser BytesAuthor = "agent or user"
)

// Listener is one bound port.
type Listener struct {
	// Name identifies the surface in prose and in gate failures.
	Name string

	// Package is the import path suffix that owns the bind, without the
	// module prefix ("internal/transport").
	Package string

	Binding    BindingClass
	Credential Credential
	Posture    ContentPosture

	// Sites are the repository-relative source files whose code creates
	// this listener. The gate matches by file rather than by line so an
	// unrelated edit above the bind does not invalidate the row. One
	// file belongs to exactly one row: a file that ever creates two
	// distinct surfaces has to be split, which is the better outcome
	// anyway.
	//
	// Empty exactly when Implicit is set.
	Sites []string

	// Implicit marks a listener a child process opens on our behalf.
	// The gate cannot find it by scanning our source, so nothing but
	// this row records that it exists.
	Implicit bool

	Why string
}

// Route is one pattern registered on an http.ServeMux.
type Route struct {
	// Pattern is the registration string verbatim, method prefix and
	// all ("POST /register", "/{$}"). Verbatim so the gate compares
	// what the source says rather than an interpretation of it.
	Pattern string

	// Listener is the Name of the Listener this route is served from.
	Listener string

	Credential Credential
	Posture    ContentPosture
	Why        string
}

// Origin is a scheme+host+port that serves bytes, described by whose
// bytes they are.
type Origin struct {
	Name string

	// Listener is the Name of the Listener that serves this origin.
	Listener string

	Author  BytesAuthor
	Posture ContentPosture
	Why     string
}

// Registry is a policy table that decides what a caller reaches THROUGH
// a surface, rather than a surface of its own. Two exist: the RPC method
// table and the event-channel table.
//
// A Registry row is a REFERENCE, not a copy. It names where the authored
// table lives, what every row of it must carry, and which function reads
// it at call time — the facts that go stale when somebody moves or
// renames a gate, which is exactly what happened to this tree's origin
// partition. The entries themselves stay in their own package, gated by
// their own tests, because two lists of the same 360 names would agree
// only until the first edit.
type Registry struct {
	// Name identifies the registry in prose and in gate failures.
	Name string

	// Listener is the Name of the Listener that carries it, and Routes
	// are the Patterns on that listener a caller reaches it over. Both
	// are checked against the rows above, so a registry cannot outlive
	// the surface it rides.
	Listener string
	Routes   []string

	// Source is the repository-relative file holding the authored table,
	// and Symbol is the declaration inside it whose elements are the
	// rows. The gate finds them; a rename or a move fails here.
	Source string
	Symbol string

	// RowFields are the fields EVERY element of Symbol must set. They
	// are the decisions the registry exists to record, so an element
	// that omits one is an entry somebody added without classifying —
	// the failure this reference is worth having.
	RowFields []string

	// Gates are the functions that decide one entry for one caller,
	// repository-relative file first. Named because a deleted gate is
	// the drift this row cannot otherwise notice: a table nothing reads
	// still looks complete.
	Gates []string

	Why string
}

// Listeners is every port this repository's code opens, plus the ones
// its child processes open on its behalf.
//
// Verified against the tree on 2026-09-01: 10 listeners across 8
// packages, one of them implicit. The tailnet node is the row added since
// the count docs/specs/remote-access.md §13 recorded on 2026-08-30.
var Listeners = []Listener{
	{
		Name:       "app transport",
		Package:    "internal/transport",
		Binding:    BindLANCapable,
		Credential: CredPageSession,
		Posture:    PostureAppOrigin,
		Sites:      []string{"internal/transport/server.go"},
		Why: "The whole application. Everything the app can do to the " +
			"machine is reachable through the WebSocket this listener " +
			"upgrades, so it is the only surface here whose credential " +
			"is proportionate by construction rather than by being small. " +
			"LAN-capable because Rebind moves it to a routable address at " +
			"the user's request; every bind — boot, ephemeral fallback, " +
			"rebind, rebind retry, rollback — goes through one helper in " +
			"server.go, so the address can change without the posture " +
			"changing with it. It terminates TLS on that SAME port when " +
			"the boot resolved a certificate (internal/servercert): the " +
			"first byte of a connection decides, so a client that pinned " +
			"the self-signed certificate the pairing payload named gets " +
			"an encrypted channel while a browser, which cannot pin, " +
			"keeps the cleartext one. WHICH certificate answers is per " +
			"handshake: a request whose SNI names the user's canonical " +
			"domain is served that domain's certificate — obtained by " +
			"internal/acmecert over DNS-01, or the external pair the user " +
			"configured — and everything else is served the self-signed " +
			"one, so a browser reaches the same listener over real HTTPS " +
			"without un-pinning any paired client. TLS here is " +
			"CONFIDENTIALITY, never authorization — the credential below " +
			"is unchanged by which half a request arrived on. Three " +
			"checks run ahead of it: an Origin allow-list derived from " +
			"the request's own authority, a Host-header guard that on a " +
			"loopback bind rejects every DNS name except the configured " +
			"canonical domain, and — once a call is authenticated — the " +
			"per-call scope gate over the session's grants.",
	},
	{
		Name:       "tailnet node",
		Package:    "internal/tailnet",
		Binding:    BindLANCapable,
		Credential: CredPageSession,
		Posture:    PostureAppOrigin,
		Sites:      []string{"internal/tailnet/node.go"},
		Why: "The same application as the transport row above, reached from " +
			"the owner's own tailnet instead of the LAN. Off by default and " +
			"lazily constructed: while the setting is false this package " +
			"builds nothing, opens nothing and writes nothing. When it is " +
			"on, an embedded userspace node (tsnet, netstack — no TUN and " +
			"no privilege) accepts on a virtual address only devices the " +
			"owner enrolled can route to, and hands the listener to " +
			"internal/transport's ServeAuxiliary, so the credential, the " +
			"origin allow-list, the Host guard and the per-call scope gate " +
			"are the SAME objects the main bind uses — there is no second " +
			"API and no second credential class. LAN-capable is the " +
			"honest class: the peer is not on this machine, so everything " +
			"about the surface has to hold against an off-host caller, and " +
			"the transport's own rule that a non-loopback upgrade must " +
			"name a live session applies unchanged (the peer address is " +
			"the node's 100.64/10 tailnet IP). What the tailnet itself " +
			"adds is transport-level: every byte is already encrypted and " +
			"authenticated by WireGuard between two enrolled devices " +
			"before this process sees it, which is why no TLS sniffer is " +
			"installed on the cleartext listener. A SECOND listener on 443 " +
			"is attached when the tailnet has HTTPS enabled, terminating a " +
			"certificate tsnet obtains and renews for the node's own ts.net " +
			"name; nothing in this repository mints, stores or presents " +
			"that certificate.",
	},
	{
		Name:       "--connect client stub",
		Package:    "internal/clientmode",
		Binding:    BindLANCapable,
		Credential: CredPageSession,
		Posture:    PostureAppOrigin,
		Sites:      []string{"internal/clientmode/clientmode.go"},
		Why: "Serves the same embedded bundle against a backend on another " +
			"host. It mints its OWN page credential and keeps the upstream " +
			"one server-side, carrying /ws through a reverse proxy with that " +
			"credential attached in Go — the page never holds a credential " +
			"for the upstream, and SPA code is identical across embedded, " +
			"--connect and remote-browser boots. Which upstream credential " +
			"depends on how the stub was started: the backend's launch token " +
			"as a bearer header, or — when it was started from a pairing " +
			"link — the rotating device session it holds durably, presented " +
			"over TLS pinned to the certificate the payload named " +
			"(internal/deviceclient). LAN-capable for the same reason as the " +
			"transport: BindAddr is the user's.",
	},
	{
		Name:       "harness control plane",
		Package:    "internal/harness/control",
		Binding:    BindLoopback,
		Credential: CredBearerToken,
		Posture:    PostureStructured,
		Sites:      []string{"internal/harness/control/server.go"},
		Why: "How mock providers collect their scenario assignment and " +
			"report what they were sent. Present only in a --harness boot, " +
			"but enumerated unconditionally: a listener that exists in some " +
			"boots is a listener. The token is per-process random and " +
			"compared in constant time, and a failed check answers 404 " +
			"rather than 401 so an unauthenticated caller cannot " +
			"distinguish a wrong token from a wrong port.",
	},
	{
		Name:       "browser MCP endpoint",
		Package:    "internal/browser",
		Binding:    BindLoopback,
		Credential: CredUnguessablePath,
		Posture:    PostureStructured,
		Sites:      []string{"internal/browser/mcp.go"},
		Why: "The row this inventory exists for. Behind the path sit page " +
			"evaluation and workspace file reads — a far larger grant than " +
			"'holds no session credential' suggests, and the credential is " +
			"a path component, which is the carrier most likely to be " +
			"written to a log. It cannot bind lazily: the URL rides the " +
			"provider CLI's argv at spawn. Three checks narrow who can " +
			"reach it, none of which is a secret and so none of which is " +
			"the Credential: the peer must be loopback per the accepted " +
			"socket's RemoteAddr, the request must carry no Origin (a " +
			"local process never sends one, a document always does), and " +
			"it must declare Content-Type: application/json, which forces " +
			"a preflight that the 405 on OPTIONS then refuses.",
	},
	{
		Name:       "claudetui hook relay",
		Package:    "internal/provider/claudetui",
		Binding:    BindLoopback,
		Credential: CredCapabilityHeader,
		Posture:    PostureStructured,
		Sites:      []string{"internal/provider/claudetui/hookrelay.go"},
		Why: "Receives Claude Code's hook callbacks and turns them back " +
			"into envelopes for the parser, including the compaction " +
			"lifecycle and AskUserQuestion answers. The token goes in " +
			"X-AO-Hook-Token rather than Authorization because the caller " +
			"is a hook command we configure, not an HTTP client we wrote, " +
			"and the peer-locality check runs first.",
	},
	{
		Name:       "claudetui upstream gateway",
		Package:    "internal/provider/claudetui",
		Binding:    BindLoopback,
		Credential: CredPeerLocality,
		Posture:    PostureProxied,
		Sites:      []string{"internal/provider/claudetui/gateway.go"},
		Why: "Injected as ANTHROPIC_BASE_URL for the spawned claude so the " +
			"app can classify and drive turns. It forwards to a FIXED " +
			"upstream and injects no credential of its own — the caller's " +
			"headers are the only authorization — so peer locality is a " +
			"proportionate boundary here in a way it would not be on a " +
			"surface that held a credential. Rejecting a non-loopback peer " +
			"also removes the asymmetry with the hook relay beside it.",
	},
	{
		Name:       "pprof diagnostics",
		Package:    "internal/observability/pprofserve",
		Binding:    BindLoopback,
		Credential: CredNone,
		Posture:    PostureDiagnostic,
		Sites:      []string{"internal/observability/pprofserve/pprofserve.go"},
		Why: "Off unless AO_PPROF_ADDR names an address, and the bind " +
			"address is parsed and refused unless it is loopback, so the " +
			"opt-in plus the bind constraint are the entire boundary — " +
			"there is no credential to add without inventing a second " +
			"one. Heap and goroutine dumps disclose whatever the process " +
			"holds, so widening the bind is not a configuration change but " +
			"a different surface.",
	},
	{
		Name:       "dev supervisor port probe",
		Package:    "cmd/agent-overflow-dev",
		Binding:    BindLoopback,
		Credential: CredNone,
		Posture:    PostureNone,
		Sites:      []string{"cmd/agent-overflow-dev/main.go"},
		Why: "Not a server. It binds the frontend dev port for as long as " +
			"it takes to learn whether the port is free, then closes it, " +
			"so no handler is ever attached and no byte is ever written. " +
			"Enumerated because the gate matches on the bind, and a row " +
			"saying 'this one serves nothing' is the answer to why it is " +
			"exempt — an exclusion list would say the same thing " +
			"somewhere harder to find. Lives in the dev-only supervisor " +
			"binary, which no release artifact contains.",
	},
	{
		Name:       "managed Chrome DevTools port",
		Package:    "internal/browser",
		Binding:    BindLoopback,
		Credential: CredUnguessablePath,
		Posture:    PostureStructured,
		Implicit:   true,
		Why: "The implicit one, and the reason Implicit exists as a field. " +
			"No line of this repository binds it: chromedp's ExecAllocator " +
			"appends --remote-debugging-port=0 whenever the caller has not " +
			"set the flag (verified in chromedp v0.16.0 allocate.go:165), " +
			"Chrome binds it on 127.0.0.1, and chromedp reads the resulting " +
			"URL from the child's stderr. Behind it is full control of the " +
			"managed browser, guarded by the random browser-target id in " +
			"the WebSocket path. It went unnamed by any inventory until " +
			"the 2026-08-30 audit, which is the case for enumerating what " +
			"our children open and not only what we open.",
	},
}

// Routes is every pattern registered on an http.ServeMux in the tree.
//
// The spec asks phase 0 for the transport and --connect muxes. The rows
// below cover all four muxes instead, because scoping the gate to two
// packages would mean an exclusion rule for the other two, and an
// exclusion rule is the shape of thing this package exists to avoid. The
// extra cost is eight rows.
var Routes = []Route{
	// internal/transport — the app transport mux.
	{
		Pattern:    "/bootstrap.json",
		Listener:   "app transport",
		Credential: CredPageSession,
		Posture:    PostureStructured,
		Why: "Exchanges a one-time ?t= ticket for the HttpOnly page " +
			"cookie and returns the connection parameters. The one route " +
			"that mints a credential rather than spending one, which is " +
			"why the Host guard and the Origin allow-list both cover it. " +
			"A live durable session also admits it (without the cookie " +
			"mint): the paired-device page holds no page credential " +
			"after a backend restart, and the manifest must not be " +
			"stricter than the /ws upgrade it describes.",
	},
	{
		Pattern:    "/ws",
		Listener:   "app transport",
		Credential: CredPageSession,
		Posture:    PostureStructured,
		Why: "The WebSocket upgrade: every RPC and every event, so the " +
			"whole authorization model hangs off this route. Origin is " +
			"validated in our own code because a handshake is not subject " +
			"to the cross-origin read rules, and a foreign Origin is " +
			"refused on loopback too. The launch credential alone admits " +
			"a peer on THIS machine; a peer that is not must also name a " +
			"live durable session, because a connection with no session " +
			"id is one no revocation can reach.",
	},
	{
		Pattern:    "/pageurl",
		Listener:   "app transport",
		Credential: CredPageSession,
		Posture:    PostureStructured,
		Why: "Answers a freshly minted page URL, for consumers that " +
			"navigate more than once and cannot reuse a single-use ticket " +
			"— the Windows launcher's reload, ao-harness, the e2e rig. " +
			"Two shapes: plain text with the ticket on the URL for a " +
			"browser, JSON with the two halves apart for a host that " +
			"owns its window and injects the ticket instead. Minting a " +
			"credential requires presenting one.",
	},
	{
		Pattern:    "/healthz",
		Listener:   "app transport",
		Credential: CredNone,
		Posture:    PostureStructured,
		Why: "The only route on this listener that checks nothing, and " +
			"deliberately: both consumers — the SPA's pre-WS compatibility " +
			"check and the update watchdog — run precisely when no valid " +
			"credential is held, and a gated health route answers 404 for a " +
			"restarted backend, which is indistinguishable from down and is " +
			"the exact condition it exists to detect. What it discloses is " +
			"the version string and the backend id, both of which the " +
			"bundle already serves to anyone who can load the page, and " +
			"neither of which authorizes anything. Two checks it does keep, " +
			"neither a credential: the same Host guard as the credentialled " +
			"routes, and no Access-Control-Allow-Origin, so a foreign page " +
			"may issue the request and can never read the answer. Readiness " +
			"stays on /bootstrap.json's 503 rather than being folded in " +
			"here — a probe that conflates booting with unreachable is what " +
			"both consumers are trying to avoid.",
	},
	{
		Pattern:    "/auth/pair",
		Listener:   "app transport",
		Credential: CredPairingToken,
		Posture:    PostureStructured,
		Why: "Pairing redemption, and the one route whose caller is " +
			"expected to hold nothing else — a device that has never met " +
			"this backend has only the token from the owner's link. What " +
			"stands in front of it is not a stronger credential but a " +
			"weaker grant: the token is single-use and minutes old, the " +
			"device must present the key it generated first, and the " +
			"session it produces admits NOTHING until the owner matches a " +
			"verification number derived from that same key. Registered " +
			"only when Config.AuthEndpoints is set.",
	},
	{
		Pattern:    "/auth/token",
		Listener:   "app transport",
		Credential: CredRefreshSecret,
		Posture:    PostureStructured,
		Why: "Credential rotation. The secret alone is deliberately not " +
			"enough on any listener, loopback included: the device key " +
			"rides X-AO-Device-Key and is checked before the secret is " +
			"spent, so a copy of the secret cannot self-renew. Presenting " +
			"a SPENT secret revokes the whole family rather than being " +
			"answered, which is what makes a copy detectable at all.",
	},
	{
		Pattern:    "/auth/passkey/begin",
		Listener:   "app transport",
		Credential: CredNone,
		Posture:    PostureStructured,
		Why: "Starts a passkey sign-in ceremony for a browser that holds " +
			"nothing at all — which is the point of the route, since a " +
			"passkey is how a device this backend has never seen signs in " +
			"with no code to type. What it hands back authorizes nothing: " +
			"a random challenge, spendable only by producing a signature " +
			"from a credential the owner registered here earlier, single " +
			"use, and expiring in minutes. It names no account and accepts " +
			"no body, so it cannot be asked whether a given person has a " +
			"passkey. Registered whenever Config.AuthEndpoints is set; a " +
			"backend with no canonical domain answers the typed " +
			"passkey_unavailable rather than making the route come and go " +
			"with a setting.",
	},
	{
		Pattern:    "/auth/passkey/finish",
		Listener:   "app transport",
		Credential: CredPasskeyAssertion,
		Posture:    PostureStructured,
		Why: "Completes a sign-in ceremony and answers with the same " +
			"credential pair pairing redemption produces, because what a " +
			"device holds afterwards is identical. The assertion is " +
			"verified against the challenge this backend issued, at the " +
			"relying party it pinned when it issued it, and the ceremony " +
			"is deleted on the FIRST attempt whether it verified or not. " +
			"A device key rides X-AO-Device-Key exactly as it does on " +
			"/auth/pair: the passkey proves the person, and the device row " +
			"is what a revocation later reaches.",
	},
	{
		Pattern:    "/auth/ticket",
		Listener:   "app transport",
		Credential: CredSessionCredential,
		Posture:    PostureStructured,
		Why: "Mints the single-use, seconds-lived ticket a client presents " +
			"on the /ws upgrade, so a session credential never rides a " +
			"WebSocket URL — where it would land in history, proxy logs " +
			"and screenshots. Authenticated through the same hook the " +
			"upgrade uses, so it can never be a way around a proof that " +
			"path demands. A caller naming no session gets the " +
			"unfingerprintable 404: there is nothing to bind a ticket to.",
	},
	{
		Pattern:    "/rpc",
		Listener:   "app transport",
		Credential: CredPageSession,
		Posture:    PostureStructured,
		Why: "One-shot HTTP RPC for the `ao` CLI, registered only when " +
			"Config.ScopedTokens is set. Its token is scoped to a subset " +
			"of methods rather than being a page credential, so a CLI " +
			"token cannot be replayed as a page.",
	},
	{
		Pattern:    "/",
		Listener:   "app transport",
		Credential: CredPageSession,
		Posture:    PostureAppOrigin,
		Why: "The SPA bundle, and the only route serving bytes an engine " +
			"executes. Backed by http.FS over the embedded frontend/dist " +
			"(never http.Dir, which would make the developer's filesystem " +
			"traversable), and wrapped so every response carries the " +
			"Content-Security-Policy and nosniff. In a dev boot the same " +
			"pattern proxies the Vite server, which is why the policy has " +
			"a dev variant chosen once at construction.",
	},

	// internal/clientmode — the --connect stub mux.
	{
		Pattern:    "/{$}",
		Listener:   "--connect client stub",
		Credential: CredPageSession,
		Posture:    PostureAppOrigin,
		Why: "The SPA shell, and the exact root only. Registered as {$} " +
			"rather than / so root-level bundle files reach the file " +
			"server below instead of being answered with HTML: under " +
			"nosniff that made /boot-theme.js unrunnable and silently " +
			"disabled the first-paint theme stamp on this origin alone.",
	},
	{
		Pattern:    "/",
		Listener:   "--connect client stub",
		Credential: CredPageSession,
		Posture:    PostureAppOrigin,
		Why: "The rest of the same embedded bundle, from one file server, " +
			"matching what the transport does with the same FS. Unknown " +
			"paths 404 rather than returning the shell — the SPA has no " +
			"client-side router, so an unknown path is a missing bundle " +
			"file and saying so is more useful than hiding it.",
	},
	{
		Pattern:    "/bootstrap.json",
		Listener:   "--connect client stub",
		Credential: CredPageSession,
		Posture:    PostureStructured,
		Why: "Issues this origin's own page cookie. Deliberately not a " +
			"proxy of the upstream's bootstrap: the stub cannot set a " +
			"cookie for another origin, so the page's credential has to be " +
			"minted here.",
	},
	{
		Pattern:    "/ws",
		Listener:   "--connect client stub",
		Credential: CredPageSession,
		Posture:    PostureProxied,
		Why: "Validates this origin's page credential, then carries the " +
			"socket to the upstream with the upstream's own credential " +
			"attached server-side: the launch token as a bearer header, or " +
			"a single-use ticket minted per upgrade when the stub holds a " +
			"paired device session, which is the whole of what admits an " +
			"off-host peer. Two credentials meet here and neither crosses: " +
			"the page's stops at the stub, the upstream's never reaches the " +
			"page.",
	},

	// internal/harness/control — the mock-provider control plane.
	{
		Pattern:    "POST /register",
		Listener:   "harness control plane",
		Credential: CredBearerToken,
		Posture:    PostureStructured,
		Why:        "A mock provider announces itself and receives its scenario assignment.",
	},
	{
		Pattern:    "GET /commands",
		Listener:   "harness control plane",
		Credential: CredBearerToken,
		Posture:    PostureStructured,
		Why:        "A mock provider polls for the next instruction in its scenario.",
	},
	{
		Pattern:    "POST /report",
		Listener:   "harness control plane",
		Credential: CredBearerToken,
		Posture:    PostureStructured,
		Why: "A mock provider reports what it was sent on the wire. The " +
			"body carries the exact prompt text and the mock's cwd, which " +
			"is why the token is not decorative even on a loopback bind.",
	},

	// internal/observability/pprofserve — the opt-in profiling mux.
	{
		Pattern:    "/debug/pprof/",
		Listener:   "pprof diagnostics",
		Credential: CredNone,
		Posture:    PostureDiagnostic,
		Why: "The stdlib index, which serves HTML and links every profile " +
			"below. Unwrapped, so it carries none of the response headers " +
			"the app's own routes do — acceptable only because the " +
			"listener is opt-in and loopback-bound.",
	},
	{
		Pattern:    "/debug/pprof/cmdline",
		Listener:   "pprof diagnostics",
		Credential: CredNone,
		Posture:    PostureDiagnostic,
		Why:        "The process's own argv, which includes whatever paths the boot was given.",
	},
	{
		Pattern:    "/debug/pprof/profile",
		Listener:   "pprof diagnostics",
		Credential: CredNone,
		Posture:    PostureDiagnostic,
		Why: "A CPU profile for ?seconds=N. Streams for its full duration, " +
			"which is why the server sets no WriteTimeout.",
	},
	{
		Pattern:    "/debug/pprof/symbol",
		Listener:   "pprof diagnostics",
		Credential: CredNone,
		Posture:    PostureDiagnostic,
		Why:        "Resolves program counters to function names.",
	},
	{
		Pattern:    "/debug/pprof/trace",
		Listener:   "pprof diagnostics",
		Credential: CredNone,
		Posture:    PostureDiagnostic,
		Why:        "An execution trace, streamed for its full duration.",
	},
}

// Origins is every scheme+host+port that serves bytes, recorded by whose
// bytes they are. The one listener absent from this list serves none:
// the dev supervisor's probe never attaches a handler.
var Origins = []Origin{
	{
		Name:     "SPA origin (embedded webview and remote browser)",
		Listener: "app transport",
		Author:   AuthorBuild,
		Posture:  PostureAppOrigin,
		Why: "The application itself. Everything served here is the " +
			"embedded frontend/dist bundle, fixed at build time. Content a " +
			"provider or the user authored reaches the page as DATA over " +
			"the WebSocket and is rendered by bundle code — it is never " +
			"served as a document at this origin, which is the property " +
			"/design/ broke and TestAuthoredBytesNeverExecute now holds.",
	},
	{
		Name:     "SPA origin (tailnet node)",
		Listener: "tailnet node",
		Author:   AuthorBuild,
		Posture:  PostureAppOrigin,
		Why: "The same embedded bundle from the same mux, reached at the " +
			"node's own name instead of a LAN address. It is a DISTINCT " +
			"browser origin all the same — a different authority means " +
			"separate storage and a separate page cookie — which is why it " +
			"gets a row rather than being folded into the transport's. " +
			"Nothing about the bytes differs: the handler is the same " +
			"object, so the Content-Security-Policy and the " +
			"authored-content rule above hold here unchanged.",
	},
	{
		Name:     "SPA origin (--connect stub)",
		Listener: "--connect client stub",
		Author:   AuthorBuild,
		Posture:  PostureAppOrigin,
		Why: "The same embedded bundle on a second origin, under the same " +
			"Content-Security-Policy. Same posture by construction: the " +
			"stub serves Config.Assets and nothing else, and its /ws " +
			"proxy carries frames rather than documents.",
	},
	{
		Name:     "upstream relay (claudetui gateway)",
		Listener: "claudetui upstream gateway",
		Author:   AuthorUpstream,
		Posture:  PostureProxied,
		Why: "Anthropic's API responses, relayed unchanged to the spawned " +
			"claude process. No engine renders these bytes and this " +
			"process authors none of them; it classifies requests on the " +
			"way through and forwards the response body as it arrives.",
	},
	{
		Name:     "harness control plane",
		Listener: "harness control plane",
		Author:   AuthorRuntime,
		Posture:  PostureStructured,
		Why:      "JSON this process writes about scenario state. Read by mock providers only.",
	},
	{
		Name:     "browser MCP endpoint",
		Listener: "browser MCP endpoint",
		Author:   AuthorRuntime,
		Posture:  PostureStructured,
		Why: "JSON-RPC results. Tool output can embed page content the " +
			"managed browser fetched, but it crosses as a string inside a " +
			"JSON result — this origin never serves it as a document, and " +
			"nothing loads this origin in a browsing context.",
	},
	{
		Name:     "claudetui hook relay",
		Listener: "claudetui hook relay",
		Author:   AuthorRuntime,
		Posture:  PostureStructured,
		Why:      "JSON acknowledgements and question answers for Claude Code's hook commands.",
	},
	{
		Name:     "pprof diagnostics",
		Listener: "pprof diagnostics",
		Author:   AuthorRuntime,
		Posture:  PostureDiagnostic,
		Why: "The stdlib profiling handlers' own HTML and binary blobs. " +
			"Authored by the standard library rather than by us, but it is " +
			"this process describing itself, so AuthorRuntime is the " +
			"honest label.",
	},
	{
		Name:     "managed Chrome DevTools port",
		Listener: "managed Chrome DevTools port",
		Author:   AuthorRuntime,
		Posture:  PostureStructured,
		Why: "The DevTools protocol: a JSON target list and a WebSocket of " +
			"CDP frames, authored by the Chrome process we launched. " +
			"Recorded here because \"our child opened an origin\" is exactly " +
			"the thing an inventory of our own code would miss.",
	},
}

// Registries is every policy table that decides what a caller reaches
// through a surface above.
//
// Verified against the tree on 2026-08-31: 360 RPC methods and 72 event
// channels. Those counts are an observation rather than a claim the gate
// holds — a new method or channel is an ordinary edit, and a count here
// would make every one of them edit this file for nothing.
var Registries = []Registry{
	{
		Name:      "RPC methods",
		Listener:  "app transport",
		Routes:    []string{"/ws", "/rpc"},
		Source:    "internal/transport/methods_gen.go",
		Symbol:    "GeneratedMethods",
		RowFields: []string{"Name", "ID", "Scope"},
		Gates: []string{
			"internal/transport/authorize.go:AuthorizeSessionMethod",
			"internal/transport/dispatcher.go:ResolveForOrigin",
		},
		Why: "Every exported method on the App receiver is a wire RPC by " +
			"construction, so the table is GENERATED from the //ao:scope " +
			"directive each one carries and methodgen fails the run on a " +
			"method that carries none — there is no default and no silent " +
			"row. Two gates read it. AuthorizeSessionMethod compares a " +
			"session's grants against the row's scope on every call, " +
			"re-reading the grants each time so a revocation lands inside " +
			"an open connection; ResolveForOrigin is the narrower one, and " +
			"judges the RECEIVER rather than the method — the harness " +
			"registers RegisterOptions{LocalOnly} and is refused off-host " +
			"with the same method_not_found shape an unregistered method " +
			"returns, so host tooling stays unenumerable. The per-METHOD " +
			"origin partition that used to sit beside them is deleted: " +
			"every off-host connection names a session, and its grants are " +
			"the answer. `host` is the scope no session may hold.",
	},
	{
		Name:      "event channels",
		Listener:  "app transport",
		Routes:    []string{"/ws"},
		Source:    "internal/transport/event_channels.go",
		Symbol:    "channelPolicies",
		RowFields: []string{"Channel", "Audience", "Retention", "Scope", "Why"},
		Gates: []string{
			"internal/transport/event_visibility.go:eventVisibleToOrigin",
			"internal/transport/event_visibility.go:sessionScopeFilter",
		},
		Why: "Server push is the THIRD DOOR, and it is the one an RPC " +
			"inventory misses: a channel fans out to every subscriber " +
			"regardless of who armed it, so a stream a local pane opened " +
			"reaches a remote client unless a row says otherwise. Each row " +
			"carries three decisions — the audience a frame may reach, " +
			"whether it is retained for replay on reconnect, and the scope " +
			"a session needs to receive it — and both filters run per " +
			"event per subscriber AND per event per connection. The " +
			"spelling half of the table is internal/eventchan, which " +
			"imports nothing so every emitting layer can name a channel; a " +
			"cross-check test fails on either half missing its counterpart, " +
			"which is what makes adding a channel two edits and no more.",
	},
}
