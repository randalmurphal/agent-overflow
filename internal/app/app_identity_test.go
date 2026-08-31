package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-overflow/internal/identity"
	"agent-overflow/internal/transport"
)

// identityApp is a store-backed App with the session core booted, which is
// what Start does. Built on newTestAppWithStore so it inherits the same
// spawn and HOME isolation every other fixture in this package gets.
func identityApp(t *testing.T) *App {
	t.Helper()
	app := newTestAppWithStore(t)
	app.initIdentity("backend-under-test")
	if app.identityState() == nil {
		t.Fatal("initIdentity did not boot the session core")
	}
	return app
}

// requestWith builds a request carrying the given session credential in
// the header carrier, plus an optional device key.
func requestWith(t *testing.T, credential, deviceKey string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:4321/ws", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if credential != "" {
		req.Header.Set(transport.SessionCredentialHeader, credential)
	}
	if deviceKey != "" {
		req.Header.Set(transport.DeviceKeyHeader, deviceKey)
	}
	return req
}

// TestARequestCarryingNoSessionStillProceeds is the compatibility case and
// the one that must never regress: every launch-credential client (the
// harness CLI, the e2e rig, a `--connect` stub) presents no session
// credential at all, and naming none is the correct answer for it.
func TestARequestCarryingNoSessionStillProceeds(t *testing.T) {
	app := identityApp(t)
	sessionID, ok := SessionForRequest(app, requestWith(t, "", ""))
	if !ok {
		t.Fatal("a launch-credential request was refused")
	}
	if sessionID != "" {
		t.Fatalf("a request naming no session resolved to %q", sessionID)
	}
}

func TestTheLocalPageChannelNamesItsSession(t *testing.T) {
	app := identityApp(t)
	credential := PageSessionCredential(app)
	if credential == "" {
		t.Fatal("the local page channel produced no credential")
	}
	sessionID, ok := SessionForRequest(app, requestWith(t, credential, ""))
	if !ok || sessionID == "" {
		t.Fatalf("the local credential resolved to (%q, %t)", sessionID, ok)
	}
	if !SessionLive(app, sessionID) {
		t.Fatal("the local session is not live")
	}
	// Stable across calls: the page refetches its manifest on every
	// reconnect, and a credential that moved each time would leave the
	// device list accumulating a session per reconnect.
	if again := PageSessionCredential(app); again != credential {
		t.Fatal("a second bootstrap fetch re-issued the local credential")
	}
}

func TestARevokedSessionIsRefusedBeforeTheUpgrade(t *testing.T) {
	app := identityApp(t)
	credential := PageSessionCredential(app)
	sessionID, ok := SessionForRequest(app, requestWith(t, credential, ""))
	if !ok {
		t.Fatal("the local credential was refused while live")
	}
	if _, err := app.identityState().sessions.RevokeSession(sessionID); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, ok := SessionForRequest(app, requestWith(t, credential, "")); ok {
		t.Fatal("a revoked credential was admitted; the connection would name a dead session")
	}
	if SessionLive(app, sessionID) {
		t.Fatal("a revoked session still reports live")
	}
}

// TestADeviceBoundSessionPresentsItsKeyOnEveryRequest — the device-key
// requirement lives in the request hook rather than on each route, so a
// route added later cannot be a way around it.
func TestADeviceBoundSessionPresentsItsKeyOnEveryRequest(t *testing.T) {
	app := identityApp(t)
	sessions := app.identityState().sessions
	owner := app.identityState().owner

	link, err := sessions.MintPairingLink(identity.PairingRequest{
		UserID:       owner.ID,
		DeviceClass:  identity.DevicePhone,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       []identity.Scope{identity.ScopeThreadsRead},
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	grant, reason := AuthEndpoints(app).RedeemPairing(transport.PairingRedemption{
		Token: link.Token, KeyThumbprint: "thumb-phone", Label: "A Phone", Platform: "ios",
	})
	if reason != "" {
		t.Fatalf("RedeemPairing: %s", reason)
	}
	if !grant.AwaitingConfirmation || grant.VerificationNumber == "" {
		t.Fatalf("a redemption answered no confirmation state: %+v", grant)
	}
	if _, err := sessions.ConfirmPairing(grant.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}

	if _, ok := SessionForRequest(app, requestWith(t, grant.Credential, "")); ok {
		t.Fatal("a device-bound session was admitted with no key presented")
	}
	if _, ok := SessionForRequest(app, requestWith(t, grant.Credential, "thumb-someone-else")); ok {
		t.Fatal("a device-bound session was admitted with the wrong key")
	}
	sessionID, ok := SessionForRequest(app, requestWith(t, grant.Credential, "thumb-phone"))
	if !ok || sessionID != grant.SessionID {
		t.Fatalf("the paired device resolved to (%q, %t)", sessionID, ok)
	}
}

// TestRenewalRotatesThroughTheTransportAdapter walks the wire shape end to
// end: the DTOs the route hands over, and the codes it hands back.
func TestRenewalRotatesThroughTheTransportAdapter(t *testing.T) {
	app := identityApp(t)
	sessions := app.identityState().sessions
	owner := app.identityState().owner

	link, err := sessions.MintPairingLink(identity.PairingRequest{
		UserID:       owner.ID,
		DeviceClass:  identity.DevicePhone,
		BindingClass: identity.BindingDeviceBound,
		Scopes:       []identity.Scope{identity.ScopeThreadsRead},
	})
	if err != nil {
		t.Fatalf("MintPairingLink: %v", err)
	}
	first, reason := AuthEndpoints(app).RedeemPairing(transport.PairingRedemption{
		Token: link.Token, KeyThumbprint: "thumb-phone",
	})
	if reason != "" {
		t.Fatalf("RedeemPairing: %s", reason)
	}
	if _, err := sessions.ConfirmPairing(first.PairingID); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}

	second, reason := AuthEndpoints(app).RenewSession(transport.SessionRenewal{
		RefreshSecret: first.RefreshSecret, KeyThumbprint: "thumb-phone",
	})
	if reason != "" {
		t.Fatalf("RenewSession: %s", reason)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("renewal moved the session id %q -> %q", first.SessionID, second.SessionID)
	}
	if second.RefreshSecret == first.RefreshSecret {
		t.Fatal("renewal re-issued the same refresh secret")
	}
	// The spent predecessor: reuse revokes the family, and the code has to
	// survive the adapter unchanged.
	if _, reason := AuthEndpoints(app).RenewSession(transport.SessionRenewal{
		RefreshSecret: first.RefreshSecret, KeyThumbprint: "thumb-phone",
	}); reason != identity.ReasonRevokedSession.Code() {
		t.Fatalf("reuse answered %q, want %q", reason, identity.ReasonRevokedSession.Code())
	}
}

// TestAnAppWithNoIdentityCoreRefusesACredentialItCannotJudge — a fixture
// that never called Start, or a boot whose core failed. Proceeding would
// name a session this process cannot revoke.
func TestAnAppWithNoIdentityCoreRefusesACredentialItCannotJudge(t *testing.T) {
	app := newTestAppWithStore(t)
	if _, ok := SessionForRequest(app, requestWith(t, "ao1.something", "")); ok {
		t.Fatal("a credential was admitted with nothing able to judge it")
	}
	if sessionID, ok := SessionForRequest(app, requestWith(t, "", "")); !ok || sessionID != "" {
		t.Fatalf("a launch-credential request resolved to (%q, %t)", sessionID, ok)
	}
	if PageSessionCredential(app) != "" {
		t.Fatal("an App with no identity core produced a page credential")
	}
	if SessionLive(app, "sess-1") {
		t.Fatal("an App with no identity core reported a session live")
	}
	if _, reason := AuthEndpoints(app).RedeemPairing(transport.PairingRedemption{Token: "x"}); reason == "" {
		t.Fatal("a redemption succeeded with no session core")
	}
}

// TestInitIdentityRefusesAnEmptyBackendID — every MAC covers the backend
// id, so an empty one would make credentials from any machine's restored
// database verify here.
func TestInitIdentityRefusesAnEmptyBackendID(t *testing.T) {
	app := newTestAppWithStore(t)
	app.initIdentity("")
	if app.identityState() != nil {
		t.Fatal("the session core booted with no backend id")
	}
}

// TestTheSessionCookieCarriesTheLocalCredential pins the delivery half:
// the local page acquires its session over the same exchange that hands it
// the page credential.
func TestTheSessionCookieCarriesTheLocalCredential(t *testing.T) {
	app := identityApp(t)
	credential := PageSessionCredential(app)

	recorder := httptest.NewRecorder()
	request := requestWith(t, "", "")
	transport.WriteSessionCookie(recorder, request, credential)
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Value != credential {
			continue
		}
		// Reading it back through the transport's own reader is what pins
		// the two halves together.
		replay := requestWith(t, "", "")
		replay.AddCookie(cookie)
		if got := transport.SessionCredential(replay); got != credential {
			t.Fatalf("the planted cookie read back as %q", got)
		}
		return
	}
	t.Fatal("no cookie carried the local credential")
}
