package app

import (
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The App's half of the session core (docs/specs/remote-access.md §3-§4).
//
// internal/identity owns credentials and internal/transport owns the wire;
// neither may import the other. This file is where they meet: it holds the
// one *identity.Sessions, satisfies the hooks transport declares, and
// implements transport.AuthEndpoints. Everything here is a thin
// adaptation — no policy decision lives in this file, because a decision
// made here would be one the session core could not enforce for a caller
// that reached it another way.

// identityState is the App's session core plus the local page channel's
// current credential.
//
// Separate from the App's other fields because every one of them is
// optional: a test fixture builds an App without ever calling Start, and
// every accessor below has to answer honestly for that App rather than
// panicking. Nil sessions means "identity is not wired", which is a
// state, not a fault.
type identityState struct {
	sessions *identity.Sessions
	// owner is the account the local page channel binds to. Read once at
	// boot; the store's single-owner index is what makes it stable.
	owner store.User

	// mu guards the cached local credential. Held only across a re-issue,
	// which happens at most once per localReissueMargin.
	mu sync.Mutex
	// local is the credential handed to the page, cached so a bootstrap
	// refetch does not write to the database. Re-issued when it comes
	// within localReissueMargin of its expiry.
	local transport.TokenGrant
}

// localReissueMargin is how far ahead of expiry the local page
// credential is re-issued. Wide enough that a page which fetched the
// manifest and then sat on it still holds something live, narrow enough
// that a re-issue is rare — the credential's own window is measured in
// hours.
const localReissueMargin = time.Hour

// initIdentity boots the session core and mints the local page channel's
// session. Called from Start after the store is open, because every row it
// touches lives there.
//
// A failure here is NOT fatal to the boot. The launch credential still
// authorizes every request, so an App whose identity core failed serves
// the local page exactly as it did before this existed; what it loses is
// attribution and the ability to revoke. Refusing to boot instead would
// turn a credential-table problem into "the app does not start", which is
// the harder failure to recover from and the one a person cannot act on.
func (a *App) initIdentity(backendID string) {
	if backendID == "" {
		// Nothing to bind a MAC to. Every credential minted under an empty
		// backend id would verify against a restored database from any
		// other machine, which is the one property the backend binding
		// exists to prevent.
		log.Printf("identity: no backend id yet; session core not started")
		return
	}
	sessions, result, err := identity.Bootstrap(a.store, backendID, "Owner")
	if err != nil {
		log.Printf("identity: session core unavailable: %v", err)
		return
	}
	state := &identityState{sessions: sessions, owner: result.Owner}
	if len(result.RecoveryCodes) > 0 {
		// The codes themselves are shown once, by the surface that asks
		// for them (phase 5). Logging the COUNT is the record that a set
		// was minted; logging the codes would put offline credentials in
		// a file that outlives the process.
		log.Printf("identity: minted %d recovery codes for the owner account", len(result.RecoveryCodes))
	}
	a.identity.Store(state)
	// The other half of AttachSessionConns's ordering handshake: when the
	// transport was constructed first, its registry is already parked here.
	if conns := a.liveConns.Load(); conns != nil {
		sessions.AttachConns(*conns)
	}

	// The local page channel: a loopback-only session this backend mints
	// for ITSELF, so a local connection names a session instead of being
	// trusted for arriving over loopback.
	session, tokens, err := sessions.EnsureLocalChannelSession(result.Owner.ID)
	if err != nil {
		log.Printf("identity: local page channel unavailable: %v", err)
		return
	}
	state.storeLocal(localGrant(tokens))
	log.Printf("identity: local page channel session %s ready", session.ID)

	// One prune per boot, with a margin so a device list can still show
	// recent history. Every row it can touch would already refuse every
	// presentation; a periodic sweep would be machinery for a table whose
	// growth is bounded by how often a person pairs a device.
	sessions.PruneCredentials(24 * time.Hour)
}

// AttachSessionConns hands the session core the live-connection registry,
// so revoking a session force-closes the sockets carrying it. Called from
// the transport boot once the server exists.
//
// The registry is parked on the App and the attach is completed by
// whichever side boots second: the transport is constructed before
// Start runs initIdentity, so attaching only through an
// already-booted core would quietly wire nothing on every ordinary
// boot (revocation would then wait on the per-connection watchdog
// instead of closing sockets synchronously — the defect the live
// pairing exercise caught on 2026-08-31). The store-then-check on both
// sides is the handshake that makes the order irrelevant; AttachConns
// is an idempotent set, so both sides completing it is fine.
func AttachSessionConns(a *App, conns identity.LiveConns) {
	a.liveConns.Store(&conns)
	if state := a.identityState(); state != nil {
		state.sessions.AttachConns(conns)
	}
}

// identityState returns the booted session core, or nil when identity is
// not wired — a test fixture that never called Start, or a boot whose
// core failed.
func (a *App) identityState() *identityState {
	state := a.identity.Load()
	if state == nil || state.sessions == nil {
		return nil
	}
	return state
}

// storeLocal caches the local page channel's credential.
func (s *identityState) storeLocal(grant transport.TokenGrant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.local = grant
}

// SessionForRequest resolves the durable session a request presents.
// Satisfies transport.Config.SessionForRequest.
//
// Three outcomes, and the middle one is the one to keep:
//
//   - no session credential at all → proceed, naming none. That is every
//     launch-credential client (the harness CLI, the e2e rig, a `--connect`
//     stub that predates this), and they must keep working unchanged.
//   - a credential the session core refuses → refuse the request. This is
//     what makes a revoked credential fail rather than silently downgrade
//     to an unattributed connection.
//   - a credential it admits → proceed, naming the session.
func SessionForRequest(a *App, r *http.Request) (string, bool) {
	credential := transport.SessionCredential(r)
	if credential == "" {
		return "", true
	}
	state := a.identityState()
	if state == nil {
		// A credential was presented and nothing can judge it. Proceeding
		// would name a session this process cannot revoke.
		return "", false
	}
	session, reason := state.sessions.Verify(credential)
	if reason.Refused() {
		return "", false
	}
	if refused := state.sessions.CheckDeviceProof(session, r.Header.Get(transport.DeviceKeyHeader)); refused.Refused() {
		// The device-key requirement lives HERE rather than on each route,
		// so a session whose device enrolled a key presents it on every
		// request that names that session — including the ticket route,
		// which would otherwise be a way around it.
		return "", false
	}
	return session.ID, true
}

// SessionLive reports whether a session id still admits work. Satisfies
// transport.Config.SessionLive: the ticket redemption and the
// per-connection re-check both hold an id and no request.
func SessionLive(a *App, sessionID string) bool {
	state := a.identityState()
	if state == nil {
		return false
	}
	_, reason := state.sessions.Live(sessionID)
	return !reason.Refused()
}

// SessionScopes reports the grants a session holds right now, or the
// closed-vocabulary reason it holds none. Satisfies
// transport.Config.SessionScopes, which the per-RPC scope gate reads.
//
// It goes through Live, the same per-RPC path every other liveness answer
// uses, so a revoked or expired session refuses on the next call rather
// than on the next watchdog tick — and so the generation discipline that
// keeps a racing read from re-caching a dead row covers the scope gate
// too. Nothing is cached on the transport side; that is the point.
//
// The returned slice is the session row's own and is read-only to its
// caller: the gate ranges over it and keeps no reference.
func SessionScopes(a *App, sessionID string) ([]string, string) {
	state := a.identityState()
	if state == nil {
		// A connection cannot name a session without identity having
		// admitted it, so this is unreachable in a booted app. Refuse
		// rather than admit: an App that lost its session core must not
		// start authorizing on an empty grant set.
		return nil, identity.ReasonUnknownSession.Code()
	}
	session, reason := state.sessions.Live(sessionID)
	if reason.Refused() {
		return nil, reason.Code()
	}
	return session.Scopes, ""
}

// PageSessionCredential returns the local page channel's credential for
// the bootstrap exchange to plant as a cookie. Satisfies
// transport.Config.PageSessionCredential.
//
// Re-issues within localReissueMargin of expiry rather than on a timer:
// the manifest is refetched on every reconnect, so the one moment a fresh
// credential is needed is also the one moment somebody asks for it.
func PageSessionCredential(a *App) string {
	state := a.identityState()
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.local.Credential == "" {
		return ""
	}
	deadline := time.UnixMilli(state.local.ExpiresAtMs).Add(-localReissueMargin)
	if time.Now().Before(deadline) {
		return state.local.Credential
	}
	_, tokens, err := state.sessions.EnsureLocalChannelSession(state.owner.ID)
	if err != nil {
		// Hand back what we have. It is closer to its expiry than we would
		// like, but it still verifies, and answering "" would sign the
		// local page out over a transient database error.
		log.Printf("identity: re-issue local page credential: %v", err)
		return state.local.Credential
	}
	state.local = localGrant(tokens)
	return state.local.Credential
}

// AuthEndpoints returns the transport-facing adapter for the device-facing
// credential routes.
//
// A separate type, and a bootstrap-boundary FUNCTION rather than two
// exported App methods, for the reason internal/app/AGENTS.md gives: an
// exported method on App is promoted onto main.App and becomes a wire RPC.
// Redeeming a pairing link over the RPC wire would let a caller who
// already holds a session enroll another device — the one thing the HTTP
// route's shape (no session, a spent token, an owner confirmation) exists
// to constrain.
func AuthEndpoints(a *App) transport.AuthEndpoints { return authEndpoints{app: a} }

// authEndpoints adapts the session core onto the transport's dumb DTOs.
type authEndpoints struct{ app *App }

// RedeemPairing spends a pairing link. Satisfies transport.AuthEndpoints.
func (e authEndpoints) RedeemPairing(req transport.PairingRedemption) (transport.TokenGrant, string) {
	state := e.app.identityState()
	if state == nil {
		return transport.TokenGrant{}, identity.ReasonUnknownCredential.Code()
	}
	redemption, reason := state.sessions.RedeemPairing(identity.RedemptionRequest{
		Token:         req.Token,
		KeyThumbprint: req.KeyThumbprint,
		Label:         req.Label,
		Platform:      req.Platform,
		Peer:          req.Peer,
	})
	if reason.Refused() {
		return transport.TokenGrant{}, reason.Code()
	}
	grant := localGrant(redemption.Tokens)
	grant.VerificationNumber = redemption.VerificationNumber
	grant.PairingID = redemption.PairingID
	return grant, ""
}

// RenewSession rotates one credential pair. Satisfies
// transport.AuthEndpoints.
func (e authEndpoints) RenewSession(req transport.SessionRenewal) (transport.TokenGrant, string) {
	state := e.app.identityState()
	if state == nil {
		return transport.TokenGrant{}, identity.ReasonUnknownCredential.Code()
	}
	tokens, reason := state.sessions.Refresh(identity.RefreshRequest{
		Secret:        req.RefreshSecret,
		KeyThumbprint: req.KeyThumbprint,
		Peer:          req.Peer,
	})
	if reason.Refused() {
		return transport.TokenGrant{}, reason.Code()
	}
	return localGrant(tokens), ""
}

// localGrant is the one translation between the session core's TokenSet
// and the transport's wire shape. One function so a field added to either
// side is added once.
func localGrant(tokens identity.TokenSet) transport.TokenGrant {
	return transport.TokenGrant{
		SessionID:            tokens.SessionID,
		Credential:           tokens.Credential,
		ExpiresAtMs:          tokens.ExpiresAtMillis,
		RefreshSecret:        tokens.RefreshSecret,
		RefreshExpiresAtMs:   tokens.RefreshExpiresAtMillis,
		AwaitingConfirmation: tokens.AwaitingConfirmation,
	}
}

// identitySlot is the App field type. Declared here rather than inline on
// the struct so the whole subsystem stays in one file.
type identitySlot = atomic.Pointer[identityState]
