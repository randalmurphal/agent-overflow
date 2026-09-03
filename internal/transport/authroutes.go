package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// The device-facing credential routes (docs/specs/remote-access.md §4).
//
// Five POSTs, one shape: a device that holds no page credential presents
// what it does hold, and is answered with a typed reason code when that is
// not enough. They are the only routes on this listener a client reaches
// WITHOUT the launch credential, because they are how a client that has
// never met this backend gets one.
//
// What this package owns is the wire: the carriers, the JSON shapes, the
// method and Origin checks, and the budgets. What a pairing token means,
// whether a refresh secret was already spent, and what a reason code
// stands for belong to internal/identity, which this package must not
// import. The seam is AuthEndpoints below, declared here and satisfied by
// an app-side adapter — the same direction as ScopedTokens, and the same
// reason: transport stays store-free.
//
// The owner-facing half of pairing — minting a link, reading the
// verification number, confirming or cancelling — is deliberately NOT
// here. Those calls are made from an already-authenticated surface that
// runs in this process, so they are Go API on the session core; putting
// them on the wire would add an unauthenticated-by-construction surface
// for a caller that does not exist.

// AuthPairPath redeems a pairing link. The one route whose caller is
// expected to hold nothing at all.
const AuthPairPath = "/auth/pair"

// AuthTokenPath rotates a device's credential pair.
const AuthTokenPath = "/auth/token"

// AuthTicketPath mints a single-use WebSocket ticket for the session the
// caller already holds.
const AuthTicketPath = "/auth/ticket"

// AuthPasskeyBeginPath starts a passkey SIGN-IN ceremony, and only that
// one. The other two ceremonies — registering a credential, and proving
// step-up — are made from a surface that already holds a session, so they
// are bound methods on the wire rather than routes here. Putting them on
// an unauthenticated route would add a way to ask for a challenge that no
// caller needs, and registration in particular must never be reachable
// without a session: it is what a later sign-in trusts.
const AuthPasskeyBeginPath = "/auth/passkey/begin"

// AuthPasskeyFinishPath completes a sign-in ceremony and answers with a
// credential pair, exactly as pairing redemption does.
const AuthPasskeyFinishPath = "/auth/passkey/finish"

// SessionCredentialHeader carries a session credential for a client that
// can set headers. The cookie below is for the one that cannot.
const SessionCredentialHeader = "X-AO-Session"

// DeviceKeyHeader carries a paired client's proof of possession.
//
// Named for the KEY rather than for one encoding of it, which is what let
// phase 5 swap the VALUE without moving a single call site. Two shapes
// ride it now, and this package does not distinguish them — which one a
// given device may present is internal/identity's answer, from the device
// row (`proof_kind`, migration v81):
//
//   - a compact JWS signed over this request, for a device that enrolled
//     an ECDSA P-256 key. A signature, so a copy of the string admits
//     nothing: it is bound to one method and path, carries a single-use
//     identifier, and expires in minutes.
//   - the bare enrollment thumbprint, for a device that could not enroll a
//     key. A plain-HTTP LAN page is not a secure context, so it has no
//     `crypto.subtle` at all; spec §15 constraint 6 states that there is
//     deliberately no LAN-HTTP proof path, and this is that class.
//
// A device that enrolled a key is never accepted on the second shape. The
// wire allows both because two kinds of device exist, not because either
// device gets to choose.
const DeviceKeyHeader = "X-AO-Device-Key"

// WSTicketParam carries a WebSocket ticket on the upgrade URL. A ticket,
// never a credential: it is single-use, expires in seconds, and names a
// session rather than authorizing one.
const WSTicketParam = "ticket"

// sessionCookiePrefix names the page's session cookie. Port-qualified for
// the same reason the page cookie is (see pageCookieName): two backends on
// one host would otherwise overwrite each other's value.
const sessionCookiePrefix = ReservedCookiePrefix + "session_"

// maxAuthBody bounds one credential request. These bodies carry a token,
// a thumbprint, and two labels — hundreds of bytes. The cap keeps a wedged
// client from making the backend allocate on its behalf.
const maxAuthBody = 16 << 10

// wsTicketTTL is how long a WebSocket ticket stays spendable. A client
// mints one immediately before it dials, so the window only has to cover a
// round trip and a handshake; anything longer is a token sitting in a
// client's memory for no reason.
const wsTicketTTL = 30 * time.Second

// maxOutstandingWSTickets bounds the ticket book. A reconnect ladder mints
// one per attempt and spends or abandons it within the TTL, so the bound
// is reached only by a client that mints without ever dialling.
const maxOutstandingWSTickets = 64

// PairingRedemption is what a device presents to spend a pairing link.
// A dumb DTO: this package neither validates nor interprets any field.
type PairingRedemption struct {
	// Token is the pairing token from the link's URL fragment.
	Token string `json:"token"`
	// KeyThumbprint identifies the keypair this device generated BEFORE
	// redeeming. There is no path that enrolls a device which presented
	// none.
	//
	// The BEARER carrier only, and the body is the right place for it
	// precisely because it is not a proof: it is an identifier the device
	// asks to be known by. A device that can sign presents DeviceProof
	// instead, from the header, and this field is then never read — see
	// that field.
	KeyThumbprint string `json:"keyThumbprint"`
	// DeviceProof is the signed proof from DeviceKeyHeader, for a device
	// that generated a real key. Never from the body: a proof a caller may
	// write into the same document it is proving something about is not a
	// proof.
	//
	// When present it decides the enrollment ALONE — the thumbprint the
	// backend records is derived from the key inside it, so KeyThumbprint
	// is not consulted and a request carrying both cannot name a key other
	// than the one it proved.
	DeviceProof string `json:"-"`
	// Method and Path are what a signed proof binds, filled in by this
	// package from the request itself. Never from the body, for the reason
	// above: a proof that names its own target proves nothing about where
	// it was presented.
	Method string `json:"-"`
	Path   string `json:"-"`
	// Label and Platform are what the device calls itself in the owner's
	// device list. Advisory, and replaceable from the host later.
	Label    string `json:"label,omitempty"`
	Platform string `json:"platform,omitempty"`
	// Peer is the request's remote address, filled in by this package for
	// the audit trail. Never read from the body.
	Peer string `json:"-"`
}

// SessionRenewal is what a device presents to rotate its credential pair.
type SessionRenewal struct {
	// RefreshSecret is the secret issued alongside the credential this
	// call replaces.
	RefreshSecret string `json:"refreshSecret"`
	// DeviceProof comes from DeviceKeyHeader, never from the body: a proof
	// a caller may write into the same document it is proving something
	// about is not a proof. A signed JWS or a bare thumbprint, per that
	// constant.
	DeviceProof string `json:"-"`
	// Method and Path are what a signed proof binds, read off the request
	// by this package.
	Method string `json:"-"`
	Path   string `json:"-"`
	// Peer is the request's remote address, for the audit trail.
	Peer string `json:"-"`
}

// TokenGrant is one credential pair as it goes on the wire. Shared by
// redemption and renewal because they issue the same thing; a device that
// just paired and a device that just rotated hold identical state
// afterwards, and two shapes would invite a client to handle them apart.
type TokenGrant struct {
	// SessionID is stable across every renewal of this session. Clients
	// may key state on it.
	SessionID string `json:"sessionId"`
	// Credential is the signed session credential.
	Credential string `json:"credential"`
	// ExpiresAtMs is when Credential stops verifying, in Unix
	// milliseconds. Absolute rather than a duration because a client that
	// receives a duration has to decide when its own clock started
	// counting.
	ExpiresAtMs int64 `json:"expiresAtMs"`
	// RefreshSecret renews the pair once, and once only. Empty for a
	// binding class that does not renew.
	RefreshSecret      string `json:"refreshSecret,omitempty"`
	RefreshExpiresAtMs int64  `json:"refreshExpiresAtMs,omitempty"`
	// AwaitingConfirmation reports that this credential is real and admits
	// nothing yet: the owner has not matched VerificationNumber. The
	// client's move is to show the number and retry.
	AwaitingConfirmation bool `json:"awaitingConfirmation,omitempty"`
	// VerificationNumber is the six digits the redeeming device displays
	// so the owner can compare them with the minting surface. Redemption
	// only.
	VerificationNumber string `json:"verificationNumber,omitempty"`
	// PairingID names the link this grant came from, so a client can poll
	// or cancel it. Redemption only.
	PairingID string `json:"pairingId,omitempty"`
	// Scopes is the grant set the issued session holds, so the device can
	// answer "which surfaces do I have" from the response that enrolled or
	// renewed it, rather than one refusal per control
	// (docs/specs/remote-access.md §5, frontend capability model).
	//
	// Always present, never null — the same rule the hello frame's
	// capability list follows, and for the same reason: a missing field
	// would be indistinguishable from a backend too old to send one, while
	// `[]` says plainly that this session was granted nothing. The producer
	// normalises it (internal/app localGrant).
	//
	// It is disclosure, not authorization. A client that edits its copy
	// changes what its own screens offer and nothing else; every RPC is
	// re-checked against the session row.
	Scopes []string `json:"scopes"`
}

// PasskeyChallenge is a ceremony this backend just started: an opaque
// handle and the options a browser hands to `navigator.credentials`.
//
// Options is raw JSON, never a typed struct. The shape is the WebAuthn
// specification's, the library that produced it owns every field, and a
// mirror here would be a second definition that agrees with the first only
// until the library adds a field. This package carries the bytes.
type PasskeyChallenge struct {
	// CeremonyID names the challenge to finish against. Opaque to the
	// client, single-use on the backend, and NOT a credential: it admits
	// nothing on its own, and answering the challenge it names requires a
	// key this backend already knows.
	CeremonyID string `json:"ceremonyId"`
	// Options is the `publicKey` member for navigator.credentials.get,
	// verbatim from the ceremony.
	Options json.RawMessage `json:"options"`
}

// PasskeyAssertion is what a device presents to finish a sign-in ceremony.
//
// A dumb DTO like the two above: nothing here verifies a signature, and
// the fields it fills in itself are exactly the ones a caller must not be
// able to state about its own request.
type PasskeyAssertion struct {
	// CeremonyID names the challenge this answers.
	CeremonyID string `json:"ceremonyId"`
	// Response is the browser's PublicKeyCredential, JSON-encoded with its
	// binary members base64url — the shape the WebAuthn library parses.
	// Raw JSON for the same reason PasskeyChallenge.Options is.
	Response json.RawMessage `json:"response"`
	// KeyThumbprint identifies the keypair this device generated before
	// signing in, exactly as PairingRedemption's does, and is ignored
	// whenever DeviceProof is present. A passkey proves the PERSON; the
	// device row is what a revocation reaches, so a sign-in still enrolls
	// or re-adopts a device.
	KeyThumbprint string `json:"keyThumbprint"`
	// DeviceProof is the signed proof from DeviceKeyHeader. Never from the
	// body, for the reason PairingRedemption.DeviceProof is not.
	DeviceProof string `json:"-"`
	// Method and Path are what a signed proof binds, filled in here from
	// the request itself.
	Method string `json:"-"`
	Path   string `json:"-"`
	// Label and Platform are what a newly enrolled device calls itself.
	Label    string `json:"label,omitempty"`
	Platform string `json:"platform,omitempty"`
	// Peer is the request's remote address, for the audit trail.
	Peer string `json:"-"`
}

// authRefusal is the body of a refused credential request. One field,
// because the code is the whole message: prose belongs to the client's
// presentation module (frontend/src/lib/transport/authReason.ts), which
// can phrase it for the surface the person is actually looking at.
type authRefusal struct {
	Reason string `json:"reason"`
}

// AuthEndpoints is the narrow app-side identity surface behind the
// device-facing routes. The app satisfies it with an adapter over
// internal/identity; this package never learns what a session row is.
//
// Both methods answer a reason CODE rather than an error: every refusal
// on this surface is a typed, client-actionable outcome, and an error
// would invite a call site to log it and answer something generic. An
// empty code means the call succeeded.
type AuthEndpoints interface {
	// RedeemPairing spends a pairing link and enrolls the device that
	// presented it.
	RedeemPairing(req PairingRedemption) (TokenGrant, string)
	// RenewSession rotates one credential pair.
	RenewSession(req SessionRenewal) (TokenGrant, string)
	// PasskeysAvailable reports whether a sign-in ceremony can be started
	// right now.
	//
	// A method rather than a Config bool because the answer moves while the
	// process runs: it depends on the canonical domain, which is a setting
	// the owner edits. Registering the routes on a boot-time snapshot would
	// mean a backend that grew a domain still had no passkey surface until
	// it restarted.
	PasskeysAvailable() bool
	// BeginPasskeySignIn starts a discoverable-credential ceremony. No
	// caller identity is named, and none could be: which credential
	// answers is the authenticator's business, and a backend that asked
	// "which account" would be answering an enumeration question for free.
	BeginPasskeySignIn() (PasskeyChallenge, string)
	// FinishPasskeySignIn verifies an assertion and issues a credential
	// pair — the same TokenGrant redemption and rotation produce, because
	// what a device holds afterwards is identical.
	FinishPasskeySignIn(req PasskeyAssertion) (TokenGrant, string)
}

// handleAuthPair answers AuthPairPath.
//
// No credential is consulted, by design: the pairing token in the body IS
// the credential, and a device redeeming one has nothing else. What still
// applies is everything that is not a secret — the method, the Origin
// allow-list, the Host guard the mux wraps this in, and the per-peer
// budget.
func (s *Server) handleAuthPair(w http.ResponseWriter, r *http.Request) {
	endpoints := s.cfg.AuthEndpoints
	if endpoints == nil || !s.acceptAuthPost(w, r) {
		return
	}
	var req PairingRedemption
	if !decodeAuthBody(w, r, &req) {
		return
	}
	req.DeviceProof = r.Header.Get(DeviceKeyHeader)
	req.Method, req.Path = r.Method, r.URL.Path
	req.Peer = r.RemoteAddr
	grant, reason := endpoints.RedeemPairing(req)
	writeAuthResult(w, s.csp, grant, reason)
}

// handleAuthToken answers AuthTokenPath.
//
// The device key comes from the header and the secret from the body. Both
// are required on every listener including loopback: a credential that
// could renew itself from possession of the secret alone would make
// rotation bookkeeping rather than a control.
func (s *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	endpoints := s.cfg.AuthEndpoints
	if endpoints == nil || !s.acceptAuthPost(w, r) {
		return
	}
	var req SessionRenewal
	if !decodeAuthBody(w, r, &req) {
		return
	}
	req.DeviceProof = r.Header.Get(DeviceKeyHeader)
	req.Method, req.Path = r.Method, r.URL.Path
	req.Peer = r.RemoteAddr
	grant, reason := endpoints.RenewSession(req)
	writeAuthResult(w, s.csp, grant, reason)
}

// handlePasskeyBegin answers AuthPasskeyBeginPath with a fresh challenge.
//
// No credential is consulted, for the reason /auth/pair consults none: the
// caller is a browser that has never met this backend and holds nothing.
// The challenge it receives authorizes nothing — spending it means
// producing a signature from a key this backend already registered — so
// what stands in front of the route is everything that is not a secret:
// the method, the Origin allow-list, the Host guard, and the shared budget.
//
// The request carries no body at all. There is nothing a caller could
// usefully say here, and a body would be a place to say which account —
// which is the question a discoverable ceremony exists not to ask.
func (s *Server) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	endpoints := s.cfg.AuthEndpoints
	if endpoints == nil || !s.acceptAuthPost(w, r) {
		return
	}
	challenge, reason := endpoints.BeginPasskeySignIn()
	writePasskeyChallenge(w, s.csp, challenge, reason)
}

// handlePasskeyFinish answers AuthPasskeyFinishPath with a credential pair.
//
// Same shape as redemption, and deliberately: a verified assertion and a
// spent pairing link both mean "the owner said yes", so both end in one
// TokenGrant and a client handles neither specially.
func (s *Server) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	endpoints := s.cfg.AuthEndpoints
	if endpoints == nil || !s.acceptAuthPost(w, r) {
		return
	}
	var req PasskeyAssertion
	if !decodeAuthBody(w, r, &req) {
		return
	}
	req.DeviceProof = r.Header.Get(DeviceKeyHeader)
	req.Method, req.Path = r.Method, r.URL.Path
	req.Peer = r.RemoteAddr
	grant, reason := endpoints.FinishPasskeySignIn(req)
	writeAuthResult(w, s.csp, grant, reason)
}

// writePasskeyChallenge writes a started ceremony or the typed refusal
// that replaced it, in the shape writeAuthResult uses for a grant: 401
// with `{"reason": code}`, so a client reads one refusal envelope on
// every route of this family.
func writePasskeyChallenge(w http.ResponseWriter, csp ContentSecurityPolicy, challenge PasskeyChallenge, reason string) {
	h := w.Header()
	WriteSecurityHeaders(h, csp)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	if reason != "" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(authRefusal{Reason: reason})
		return
	}
	_ = json.NewEncoder(w).Encode(challenge)
}

// ticketGrant is the body of a minted WebSocket ticket.
type ticketGrant struct {
	// Ticket is spent by the next upgrade that presents it.
	Ticket string `json:"ticket"`
	// ExpiresAtMs is when it stops being spendable.
	ExpiresAtMs int64 `json:"expiresAtMs"`
}

// handleAuthTicket answers AuthTicketPath with a ticket bound to the
// session the caller already holds.
//
// Authenticated by the session credential itself, through the same
// SessionForRequest hook the upgrade uses — so whatever proof that hook
// demands of a request (today: the device key, for a session whose device
// enrolled one) is demanded here too, and this route can never be a way
// around it.
//
// A caller that names no session gets the unfingerprintable 404 rather
// than a refusal: there is nothing to bind a ticket to, and saying so
// would distinguish "this backend has sessions" from "this path does not
// exist".
func (s *Server) handleAuthTicket(w http.ResponseWriter, r *http.Request) {
	if !s.acceptAuthPost(w, r) {
		return
	}
	resolve := s.cfg.SessionForRequest
	if resolve == nil {
		http.NotFound(w, r)
		return
	}
	sessionID, ok := resolve(r)
	if !ok || sessionID == "" {
		http.NotFound(w, r)
		return
	}
	ticket, err := s.wsTickets.mint(sessionID)
	if err != nil {
		http.Error(w, "ticket unavailable", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	WriteSecurityHeaders(h, s.csp)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ticketGrant{
		Ticket:      ticket,
		ExpiresAtMs: time.Now().Add(wsTicketTTL).UnixMilli(),
	})
}

// acceptAuthPost applies the checks every credential route shares,
// writing the refusal itself and reporting whether the handler should
// continue.
//
// Origin before anything else, for the reason it runs first on the
// bootstrap exchange: these routes hand out credentials, and a request
// another origin initiated must never be answered with one.
func (s *Server) acceptAuthPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !OriginAllowed(r, s.currentOriginPatterns()) {
		http.NotFound(w, r)
		return false
	}
	return true
}

// decodeAuthBody reads exactly one JSON document of at most maxAuthBody
// bytes. A trailing second document is a bad request, not a value to
// ignore: it is the shape a request-smuggling relay produces, and
// accepting the first would authorize on a body some other component read
// differently.
func decodeAuthBody(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthBody))
	if err := decoder.Decode(into); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

// writeAuthResult writes a grant or the typed refusal that replaced it.
//
// 401 for every refusal, whatever refused it. The reason code is in the
// body where the client's presentation module reads it, and mapping codes
// onto distinct statuses would put the same fact in two places and let
// them disagree.
func writeAuthResult(w http.ResponseWriter, csp ContentSecurityPolicy, grant TokenGrant, reason string) {
	h := w.Header()
	WriteSecurityHeaders(h, csp)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/json")
	if reason != "" {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(authRefusal{Reason: reason})
		return
	}
	_ = json.NewEncoder(w).Encode(grant)
}

// SessionCredential returns the session credential a request carries, or
// "" when it names no session.
//
// Two carriers, one reader, and the HEADER wins. A relay that forwards a
// credential on purpose (the WSL launcher) is making a statement about
// whose request this is; an ambient cookie is the browser's default
// behavior. When both are present the deliberate one is the one to
// believe.
//
// This package decides where the string is; internal/identity decides
// what it means. Nothing here parses or validates it.
func SessionCredential(r *http.Request) string {
	if presented := r.Header.Get(SessionCredentialHeader); presented != "" {
		return presented
	}
	name := sessionCookieName(r.Host)
	for _, cookie := range r.Cookies() {
		// Every cookie of the name, not just the first: another page on
		// this host can write a same-named cookie on a different path,
		// which the browser then sends ahead of ours.
		if cookie.Name == name && cookie.Value != "" {
			return cookie.Value
		}
	}
	return ""
}

// WriteSessionCookie plants a session credential as the page's HttpOnly
// session cookie. Same attributes as the page cookie and for the same
// reasons (see pageCookie) — page script must never be able to read a
// credential, and the cookie must not ride a request another site
// initiated.
func WriteSessionCookie(w http.ResponseWriter, r *http.Request, credential string) {
	if credential == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName(r.Host),
		Value:    credential,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
}

// sessionCookieName derives the session cookie's name from the authority
// the client dialled, exactly as pageCookieName does.
func sessionCookieName(host string) string {
	return cookieNameForHost(sessionCookiePrefix, host)
}
