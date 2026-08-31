package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// The device-facing credential routes (docs/specs/remote-access.md §4).
//
// Three POSTs, one shape: a device that holds no page credential presents
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

// SessionCredentialHeader carries a session credential for a client that
// can set headers. The cookie below is for the one that cannot.
const SessionCredentialHeader = "X-AO-Session"

// DeviceKeyHeader carries the thumbprint of the device key a paired
// client holds. It is the proof-of-possession carrier for this phase.
//
// A thumbprint is not a signature: it proves the client knows which key
// the device enrolled, not that it currently holds the private half. The
// spec's end state is a per-request DPoP proof (§4), and that needs a
// signing scheme on the wire which does not exist yet. Naming the header
// for the KEY rather than for the thumbprint is deliberate — phase 5
// replaces the value with a proof and keeps every call site.
const DeviceKeyHeader = "X-AO-Device-Key"

// WSTicketParam carries a WebSocket ticket on the upgrade URL. A ticket,
// never a credential: it is single-use, expires in seconds, and names a
// session rather than authorizing one.
const WSTicketParam = "ticket"

// sessionCookiePrefix names the page's session cookie. Port-qualified for
// the same reason the page cookie is (see pageCookieName): two backends on
// one host would otherwise overwrite each other's value.
const sessionCookiePrefix = "ao_session_"

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
	KeyThumbprint string `json:"keyThumbprint"`
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
	// KeyThumbprint comes from DeviceKeyHeader, never from the body: a
	// proof a caller may write into the same document it is proving
	// something about is not a proof.
	KeyThumbprint string `json:"-"`
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
	req.KeyThumbprint = r.Header.Get(DeviceKeyHeader)
	req.Peer = r.RemoteAddr
	grant, reason := endpoints.RenewSession(req)
	writeAuthResult(w, s.csp, grant, reason)
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
