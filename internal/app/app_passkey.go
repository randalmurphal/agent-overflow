package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"agent-overflow/internal/appidentity"
	"agent-overflow/internal/identity"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
)

// The passkey seam (docs/specs/remote-access.md §4 "Step-up", "Passkeys").
//
// internal/identity runs the ceremonies and internal/transport carries the
// bytes; what neither of them can know is what this backend ANSWERS TO,
// because that is a setting the owner edits and a listener the boot bound.
// This file resolves it, and satisfies the transport's step-up hook over
// the same core.

// passkeyRelyingParty resolves the relying party every ceremony runs
// under, from the canonical domain in Settings → Network and the live
// listener's port.
//
// The canonical domain is the ONLY candidate for an RP ID. WebAuthn
// requires a domain — an address is refused outright, and an authenticator
// binds a credential to the exact string, so a name that changes with the
// network (a LAN IP, a tailnet address) would silently orphan every
// credential the moment the machine moved. A backend with no domain
// therefore has no passkey surface at all, which the surfaces say plainly
// rather than half-offering.
//
// The `.localhost` family is the exception: a browser treats those names
// as a secure context over plain HTTP and resolves them to loopback
// itself, so they are the only names a ceremony can run under with no
// certificate at all. `localhost` bare is not settable — a canonical
// domain must contain a dot — so the spelling that works is
// `<label>.localhost`.
//
// It is NOT what the e2e rig runs on, and the reason is worth knowing
// before reaching for it: the frontend reads every `*.localhost` document
// as local (`transport/bootstrap.ts` `isLoopbackHostname`), so a page
// served under one never latches a terminal transport state and never
// offers to sign in. A remote leg needs an ordinary domain over the
// listener's TLS half; `e2e/tests/harness-passkey-lifecycle.spec.ts`
// argues the whole shape.
func passkeyRelyingParty(a *App) identity.RelyingParty {
	if a == nil || a.settings == nil {
		return identity.RelyingParty{}
	}
	domain := strings.TrimSpace(a.settings.Get().Network.CanonicalDomain)
	if domain == "" {
		return identity.RelyingParty{}
	}
	port := ""
	if srv := a.transportServer.Load(); srv != nil {
		if _, p, err := net.SplitHostPort(srv.Addr()); err == nil {
			port = p
		}
	}
	return identity.RelyingParty{
		ID: domain,
		// What the authenticator's prompt calls this backend. The stable
		// product name, deliberately not appidentity.AppTitle — that varies
		// by boot mode, and an authenticator stores this string beside the
		// credential, so a harness boot would leave a person's passkey list
		// naming two things that are one backend.
		DisplayName: appidentity.Name,
		Origins:     passkeyOrigins(domain, port),
	}
}

// passkeyOrigins is every origin a page running a ceremony may be loaded
// from, for one domain.
//
// Origins are compared scheme-and-authority exact, and a PORT is part of
// an authority, so naming only one would refuse the ceremony this backend
// had just started whenever the browser reached it by the other route.
// Two exist:
//
//   - the direct bind, `https://<domain>:<port>`, which is how a browser
//     reaches a backend serving its own domain certificate;
//   - the default port, `https://<domain>`, which is what a browser sends
//     when a reverse proxy on this machine terminates TLS on 443 and
//     forwards to the loopback bind.
//
// A proxy on some OTHER port is not derivable from anything this process
// knows, and is the deployment that has to be reached on the direct
// authority instead.
//
// The `.localhost` family is the one that runs over cleartext, because a
// browser treats those names as a secure context and WebAuthn is
// unavailable outside one. Every other name is https, since a ceremony
// would not have started.
func passkeyOrigins(domain, port string) []string {
	scheme := "https"
	if isLocalhostName(domain) {
		scheme = "http"
	}
	origins := []string{scheme + "://" + domain}
	if port != "" {
		origins = append(origins, scheme+"://"+net.JoinHostPort(domain, port))
	}
	return origins
}

// isLocalhostName reports whether a browser treats this name as loopback
// and as a secure context without a certificate.
//
// Deliberately NOT loopback.HostHeader, which is a different rule with a
// different job: it decides which Host headers this listener answers, and
// it accepts address literals — which are exactly what an RP ID may never
// be. The two are not interchangeable, and the loopback package's own doc
// says so about every pair in it.
func isLocalhostName(domain string) bool {
	return domain == "localhost" || strings.HasSuffix(domain, ".localhost")
}

// StepUpProof spends the step-up token an RPC presented and reports
// whether it proved this session. Satisfies transport.Config.StepUpProof.
//
// The token is consumed by the asking, whatever the answer — that is what
// single use means, and it is why the transport asks once per call and
// carries the result rather than letting each gate re-ask.
func StepUpProof(a *App, sessionID, token string) bool {
	state := a.identityState()
	if state == nil {
		return false
	}
	return state.sessions.SpendStepUpToken(sessionID, token)
}

// The bound half of the passkey surface (docs/specs/remote-access.md §4).
//
// Six methods, split by what they are FOR rather than by ceremony shape,
// because that split is what their annotations encode:
//
//   - registering and removing a credential are ACCESS administration,
//     beside the pairing and revocation calls, and carry the same
//     `access:admin` scope so the surface moves as a unit. One of them
//     also carries //ao:stepup: the BEGIN, which is what a registration
//     costs a proof for. The finish rides that same proof through the
//     ceremony handle, and its doc comment argues why a second would be
//     worse than nothing;
//   - proving step-up is the FLOOR, because a session that cannot ask to
//     prove itself can never satisfy the gate that just refused it.
//
// Sign-in has no method here at all. Its caller holds nothing, so it is
// an HTTP route (internal/transport authroutes.go).

// PasskeyChallengeResult is a ceremony the backend just started.
//
// Options is the WebAuthn options blob, verbatim from the library, and it
// crosses this layer unread — a typed mirror would be a second definition
// of the specification's shape that agrees with the library's only until
// it grows a field. `json.RawMessage` generates as `any`, and the browser
// half decodes its base64url members before calling
// `navigator.credentials` (frontend/src/lib/transport/passkey.ts).
type PasskeyChallengeResult struct {
	CeremonyID string          `json:"ceremonyId"`
	Options    json.RawMessage `json:"options"`
}

// PasskeySummary is one registered credential as the settings list shows
// it. Deliberately not the store row: the public key, the credential id
// and the attestation blob are not a person's business and would put
// bytes on the wire that no screen renders.
type PasskeySummary struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	CreatedAtMs  int64  `json:"createdAtMs"`
	LastUsedAtMs int64  `json:"lastUsedAtMs,omitempty"`
	// RelyingPartyID is the domain this credential was registered under.
	// An authenticator binds a credential to that exact string, so a
	// changed canonical domain leaves the credential real and unusable.
	RelyingPartyID string `json:"relyingPartyId"`
	// Usable is false for exactly that case: the credential is listed
	// anyway, because hiding it would leave a person unable to remove
	// something their authenticator still offers them.
	Usable bool `json:"usable"`
	// CloneWarning is set when this authenticator's signature counter
	// failed to advance. An ANOMALY to show, never a verdict: the counter
	// is optional, authenticators that keep none report zero forever, and
	// a sign-in proceeds either way.
	CloneWarning bool `json:"cloneWarning,omitempty"`
	// BackedUp reports that the credential is synced to the person's
	// account rather than living on one device, which is what makes it
	// available on a phone they have not paired.
	BackedUp bool `json:"backedUp,omitempty"`
	// Transports is what the authenticator said it can be reached by
	// ("internal", "hybrid", "usb"). Presentation only.
	Transports []string `json:"transports,omitempty"`
}

// PasskeyStepUpGrant is a proven step-up: one token, spendable once, on
// the session that asked for it.
type PasskeyStepUpGrant struct {
	Token string `json:"token"`
	// ExpiresAtMs is when it stops being spendable. Absolute, like every
	// other deadline on this wire, because a client handed a duration has
	// to decide when its own clock started counting.
	ExpiresAtMs int64 `json:"expiresAtMs"`
}

// BeginPasskeyRegistration starts enrolling a new credential for the
// owner account.
//
// `access:admin` puts it beside pairing and revocation, which is what it
// is: adding a way for a device to get in. //ao:stepup for the reason
// minting a pairing link carries it — this issues something that admits a
// future caller, and a standing grant that could register a credential
// could register its way around its own revocation.
//
//ao:scope access:admin
//ao:route home
//ao:stepup
func (a *App) BeginPasskeyRegistration(label string) (PasskeyChallengeResult, error) {
	state, err := a.accessState()
	if err != nil {
		return PasskeyChallengeResult{}, err
	}
	challenge, reason := state.sessions.BeginPasskeyRegistration(state.owner.ID, label)
	if reason.Refused() {
		return PasskeyChallengeResult{}, transport.AuthRefused(reason.Code())
	}
	return passkeyChallenge(challenge), nil
}

// FinishPasskeyRegistration records the credential the authenticator just
// produced.
//
// The account is the one the BEGIN named and never one the response could
// claim, which is the session core's rule; this method only carries bytes.
//
// Deliberately NO //ao:stepup, and the argument is the ceremony's own
// shape: this call is unreachable without a ceremony id, and the only
// thing that mints one is a BeginPasskeyRegistration that already passed
// step-up. That handle is in-memory, single-use, expires in
// identity.PasskeyCeremonyTTL, and cannot be answered without a signature
// over its challenge — so one proof per registration is the granularity,
// and a second would guard nothing the first does not.
//
// It is also actively harmful, which is how the omission was found. A
// REMOTE registration proves step-up with a passkey, and by the time the
// finish runs, the credential the authenticator just created is
// discoverable and NOT yet registered here — so the second ceremony can
// be answered by the one credential this backend cannot verify, and the
// registration fails on its own success (`e2e/tests/harness-passkey-
// lifecycle.spec.ts`). Registering remotely also cost two prompts.
//
//ao:scope access:admin
//ao:route home
func (a *App) FinishPasskeyRegistration(ceremonyID string, response json.RawMessage) (PasskeySummary, error) {
	state, err := a.accessState()
	if err != nil {
		return PasskeySummary{}, err
	}
	row, reason := state.sessions.FinishPasskeyRegistration(ceremonyID, response)
	if reason.Refused() {
		return PasskeySummary{}, transport.AuthRefused(reason.Code())
	}
	return passkeySummary(row, state.sessions.PasskeyRelyingParty().ID), nil
}

// ListPasskeys returns the owner's registered credentials, oldest first.
//
//ao:scope access:admin
//ao:route home
func (a *App) ListPasskeys() ([]PasskeySummary, error) {
	state, err := a.accessState()
	if err != nil {
		return nil, err
	}
	rows, err := state.sessions.ListPasskeys(state.owner.ID)
	if err != nil {
		return nil, fmt.Errorf("access: list passkeys: %w", err)
	}
	current := state.sessions.PasskeyRelyingParty().ID
	out := make([]PasskeySummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, passkeySummary(row, current))
	}
	return out, nil
}

// DeletePasskey removes one credential. Reports whether a row was there,
// so a second click from a stale list answers what it asked rather than a
// lookup failure.
//
// Carries no //ao:stepup, on the same argument every subtraction on the
// access surface makes: it issues nothing, and a device the owner granted
// `access:admin` must be able to remove a passkey from a phone it can
// still reach. It also ends no SESSION — a session a passkey signed in is
// an ordinary session on an ordinary device row, revoked the way every
// other one is — and the surface says so rather than implying otherwise.
//
//ao:scope access:admin
//ao:route home
func (a *App) DeletePasskey(passkeyID string) (bool, error) {
	state, err := a.accessState()
	if err != nil {
		return false, err
	}
	removed, err := state.sessions.DeletePasskey(state.owner.ID, passkeyID)
	if err != nil {
		return false, fmt.Errorf("access: remove passkey: %w", err)
	}
	return removed, nil
}

// BeginPasskeyStepUp starts the ceremony that proves a person is at the
// keyboard right now, for the session making the call.
//
// `session` is the FLOOR, and it has to be: this is how a session
// SATISFIES the gate that just refused it, so requiring any grant would
// make step-up reachable only to sessions that had already been granted
// something — and the calls behind step-up are precisely the ones no
// grant can open. It discloses nothing either: the challenge is random,
// the session is the connection's own, and finishing it requires a
// signature from a credential registered on this account.
//
//ao:scope session
//ao:route home
func (a *App) BeginPasskeyStepUp(ctx context.Context) (PasskeyChallengeResult, error) {
	state, err := a.accessState()
	if err != nil {
		return PasskeyChallengeResult{}, err
	}
	sessionID := transport.SessionFromContext(ctx)
	if sessionID == "" {
		// An in-process caller, a background saga, or a test. There is no
		// session to elevate, and every gate admits such a caller before it
		// ever asks — so a ceremony here would mint a token nothing needs.
		return PasskeyChallengeResult{}, fmt.Errorf("access: step-up is for a call made from a session")
	}
	challenge, reason := state.sessions.BeginPasskeyStepUp(sessionID)
	if reason.Refused() {
		return PasskeyChallengeResult{}, transport.AuthRefused(reason.Code())
	}
	return passkeyChallenge(challenge), nil
}

// FinishPasskeyStepUp verifies the assertion and answers the single-use
// token the caller attaches to its next call.
//
// The session the token binds to is the one the CEREMONY recorded, not
// one this call names, so a caller cannot obtain a proof for somebody
// else's session — and the token is judged against the presenting
// connection's session when it is spent (transport.Config.StepUpProof).
//
//ao:scope session
//ao:route home
func (a *App) FinishPasskeyStepUp(ceremonyID string, response json.RawMessage) (PasskeyStepUpGrant, error) {
	state, err := a.accessState()
	if err != nil {
		return PasskeyStepUpGrant{}, err
	}
	// No peer address: an RPC has no request here, and the audit entry the
	// session core writes carries the user, device and session instead —
	// which is the attribution that identifies a call on an open socket.
	grant, reason := state.sessions.FinishPasskeyStepUp(ceremonyID, response, "")
	if reason.Refused() {
		return PasskeyStepUpGrant{}, transport.AuthRefused(reason.Code())
	}
	return PasskeyStepUpGrant{Token: grant.Token, ExpiresAtMs: grant.ExpiresAtMillis}, nil
}

// passkeyChallenge is the one projection of a started ceremony onto the
// wire, so the two begins cannot disagree about its shape.
func passkeyChallenge(challenge identity.PasskeyChallenge) PasskeyChallengeResult {
	return PasskeyChallengeResult{
		CeremonyID: challenge.CeremonyID,
		Options:    challenge.Options,
	}
}

// passkeySummary projects one stored credential for the settings list.
// currentRPID is what this backend answers to now, which is what decides
// whether the credential can still be used.
func passkeySummary(row store.Passkey, currentRPID string) PasskeySummary {
	return PasskeySummary{
		ID:             row.ID,
		Label:          row.Label,
		CreatedAtMs:    row.CreatedAt,
		LastUsedAtMs:   row.LastUsedAt,
		RelyingPartyID: row.RPID,
		Usable:         currentRPID != "" && row.RPID == currentRPID,
		CloneWarning:   row.CloneWarning,
		BackedUp:       row.BackupState,
		Transports:     slicesx.OrEmpty(row.Transports),
	}
}
