package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// stubAuth is a recording AuthEndpoints. It answers whatever the test
// arranged and remembers what the route handed it, which is the half
// worth pinning: this package's job is the carriers, not the decision.
type stubAuth struct {
	mu         sync.Mutex
	pairing    []PairingRedemption
	renewals   []SessionRenewal
	assertions []PasskeyAssertion
	grant      TokenGrant
	reason     string
	renewGood  bool
	// passkeys is what PasskeysAvailable answers. False by default, which
	// is the state of a backend with no canonical domain.
	passkeys bool
	// challenge is what a begin hands back when passkeys are available.
	challenge PasskeyChallenge
	// beginCount records how many ceremonies were started, so a route that
	// answered without asking the endpoint is visible.
	beginCount int
}

func (s *stubAuth) RedeemPairing(req PairingRedemption) (TokenGrant, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairing = append(s.pairing, req)
	return s.grant, s.reason
}

func (s *stubAuth) RenewSession(req SessionRenewal) (TokenGrant, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewals = append(s.renewals, req)
	if s.renewGood {
		return s.grant, ""
	}
	return s.grant, s.reason
}

func (s *stubAuth) PasskeysAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.passkeys
}

func (s *stubAuth) BeginPasskeySignIn() (PasskeyChallenge, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginCount++
	if !s.passkeys {
		return PasskeyChallenge{}, "passkey_unavailable"
	}
	return s.challenge, ""
}

func (s *stubAuth) FinishPasskeySignIn(req PasskeyAssertion) (TokenGrant, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assertions = append(s.assertions, req)
	return s.grant, s.reason
}

func (s *stubAuth) lastAssertion(t *testing.T) PasskeyAssertion {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.assertions) == 0 {
		t.Fatal("the route never reached the endpoint")
	}
	return s.assertions[len(s.assertions)-1]
}

func (s *stubAuth) lastPairing(t *testing.T) PairingRedemption {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pairing) == 0 {
		t.Fatal("the route never reached the endpoint")
	}
	return s.pairing[len(s.pairing)-1]
}

func (s *stubAuth) lastRenewal(t *testing.T) SessionRenewal {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.renewals) == 0 {
		t.Fatal("the route never reached the endpoint")
	}
	return s.renewals[len(s.renewals)-1]
}

// postJSON posts a document and returns the response. No credential: the
// credential routes are the ones a caller reaches without one.
func postJSON(t *testing.T, addr, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestPairRouteCarriesTheRedemptionAndAnswersAGrant(t *testing.T) {
	auth := &stubAuth{grant: TokenGrant{
		SessionID: "sess-1", Credential: "ao1.credential", ExpiresAtMs: 42,
		RefreshSecret: "refresh-1", AwaitingConfirmation: true,
		VerificationNumber: "004217", PairingID: "pair-1",
	}}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })

	resp := postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{
		Token: "pairing-token", KeyThumbprint: "thumb-phone",
		Label: "A Phone", Platform: "ios",
	}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var grant TokenGrant
	if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if grant.Credential != "ao1.credential" || grant.VerificationNumber != "004217" {
		t.Fatalf("grant did not survive the wire: %+v", grant)
	}
	if !grant.AwaitingConfirmation {
		t.Fatal("the pending state did not survive the wire; the device would present a credential it cannot use")
	}

	req := auth.lastPairing(t)
	if req.Token != "pairing-token" || req.KeyThumbprint != "thumb-phone" || req.Label != "A Phone" {
		t.Fatalf("the route reshaped the redemption: %+v", req)
	}
	// The bearer identifier rides the BODY, and the route fills in the
	// request facts a signed proof binds. Both matter: a proof that named
	// its own method and path would prove nothing about where it arrived.
	if req.Method != http.MethodPost || req.Path != AuthPairPath {
		t.Fatalf("the route did not carry the request binding: method %q path %q",
			req.Method, req.Path)
	}
	if req.Peer == "" {
		t.Fatal("the peer was not filled in; the audit entry would name nobody")
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q; a credential must never be cacheable", got)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("the credential routes skipped the standard security headers")
	}
}

// TestPairRouteAnswersTheTypedReason — the reason code is the whole
// message, and it has to reach the client's presentation module intact.
func TestPairRouteAnswersTheTypedReason(t *testing.T) {
	auth := &stubAuth{reason: "unknown_credential"}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })

	resp := postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{Token: "nope"}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var refusal authRefusal
	if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if refusal.Reason != "unknown_credential" {
		t.Fatalf("reason = %q, want the endpoint's code", refusal.Reason)
	}
}

// TestRenewalTakesTheKeyFromTheHeaderOnly — a proof a caller may write
// into the same document it is proving something about is not a proof.
func TestRenewalTakesTheKeyFromTheHeaderOnly(t *testing.T) {
	auth := &stubAuth{renewGood: true, grant: TokenGrant{SessionID: "sess-1", Credential: "ao1.next"}}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })

	body := map[string]string{"refreshSecret": "secret-1", "nextRefreshSecret": "proposed-successor", "keyThumbprint": "thumb-from-the-body"}
	resp := postJSON(t, f.srv.Addr(), AuthTokenPath, body,
		map[string]string{DeviceKeyHeader: "thumb-from-the-header"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	req := auth.lastRenewal(t)
	if req.NextRefreshSecret != "proposed-successor" {
		t.Fatal("renewal lost its successor")
	}
	if req.RefreshSecret != "secret-1" {
		t.Fatalf("refresh secret = %q", req.RefreshSecret)
	}
	if req.DeviceProof != "thumb-from-the-header" {
		t.Fatalf("key thumbprint = %q, want the header's", req.DeviceProof)
	}
}

// TestPairRouteCarriesTheSignedProofFromTheHeader: a device that can sign
// presents its proof on DeviceKeyHeader, never in the body — a proof a
// caller may write into the same document it is proving something about
// is not a proof. Both carriers reach the app side, which decides between
// them.
func TestPairRouteCarriesTheSignedProofFromTheHeader(t *testing.T) {
	auth := &stubAuth{grant: TokenGrant{SessionID: "sess-1", Credential: "ao1.credential"}}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })

	resp := postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{
		Token: "pairing-token", KeyThumbprint: "ignored-when-a-proof-is-present",
	}, map[string]string{DeviceKeyHeader: "header.proof.signature"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	req := auth.lastPairing(t)
	if req.DeviceProof != "header.proof.signature" {
		t.Fatalf("device proof = %q, want the header's", req.DeviceProof)
	}
}

func TestCredentialRoutesRefuseEverythingButPost(t *testing.T) {
	auth := &stubAuth{}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
	for _, path := range []string{AuthPairPath, AuthTokenPath} {
		resp, err := http.Get("http://" + f.srv.Addr() + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405", path, resp.StatusCode)
		}
	}
}

// TestCredentialRoutesRefuseAForeignOrigin — these routes hand out
// credentials, so a request another origin initiated must never be
// answered with one.
func TestCredentialRoutesRefuseAForeignOrigin(t *testing.T) {
	auth := &stubAuth{}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
	resp := postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{Token: "x"},
		map[string]string{"Origin": "http://elsewhere.example"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign-origin redemption = %d, want 404", resp.StatusCode)
	}
	auth.mu.Lock()
	reached := len(auth.pairing)
	auth.mu.Unlock()
	if reached != 0 {
		t.Fatal("a foreign-origin request reached the endpoint")
	}
}

func TestCredentialRoutesAreAbsentWithoutEndpoints(t *testing.T) {
	f := newServerFixture(t)
	resp := postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{Token: "x"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unwired credential route = %d, want 404", resp.StatusCode)
	}
}

func TestCredentialRouteRefusesATrailingDocument(t *testing.T) {
	auth := &stubAuth{}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
	req, err := http.NewRequest(http.MethodPost, "http://"+f.srv.Addr()+AuthPairPath,
		strings.NewReader(`{"token":"a"}{"token":"b"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("two documents in one body = %d, want 400", resp.StatusCode)
	}
	auth.mu.Lock()
	reached := len(auth.pairing)
	auth.mu.Unlock()
	if reached != 0 {
		t.Fatal("the first of two documents was authorized")
	}
}

// mintTicket buys a WebSocket ticket for the session the fixture names.
func mintTicket(t *testing.T, f *sessionFixture) string {
	t.Helper()
	resp := postJSON(t, f.srv.Addr(), AuthTicketPath, struct{}{}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ticket status = %d, want 200", resp.StatusCode)
	}
	var grant ticketGrant
	if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
		t.Fatalf("decode ticket: %v", err)
	}
	if grant.Ticket == "" {
		t.Fatal("the route answered an empty ticket")
	}
	return grant.Ticket
}

// getIntegrationBootstrap fetches /bootstrap.json from a sessionFixture,
// whose launch token differs from the serverFixture's.
func getIntegrationBootstrap(t *testing.T, addr string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/bootstrap.json", nil)
	if err != nil {
		t.Fatalf("build bootstrap request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer integration-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("bootstrap request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestTicketNamesTheSessionOnTheUpgrade is the whole point of the
// primitive: the credential stays off the WS URL and a short-lived
// single-use ticket rides it instead.
func TestTicketNamesTheSessionOnTheUpgrade(t *testing.T) {
	f := newSessionFixture(t)
	ticket := mintTicket(t, f)

	url := "ws://" + f.addr + "/ws?token=integration-token&" + WSTicketParam + "=" + ticket
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial with a ticket: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	deadline := time.Now().Add(2 * time.Second)
	for f.srv.SessionConns().CountForSession("sess-1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the ticketed connection never joined the live-session registry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A paired device holds no page credential after a backend restart —
// its one-time ticket is spent and its page cookie died with the launch
// that planted it — so the spent WS ticket alone must admit the
// upgrade. The waiver is scoped to the ticket arm: the ambient-cookie
// arm still demands the page credential.
func TestTicketAdmitsTheUpgradeWithoutAPageCredential(t *testing.T) {
	f := newSessionFixture(t)
	ticket := mintTicket(t, f)

	url := "ws://" + f.addr + "/ws?" + WSTicketParam + "=" + ticket
	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial with only a ticket: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	deadline := time.Now().Add(2 * time.Second)
	for f.srv.SessionConns().CountForSession("sess-1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the ticket-only connection never joined the live-session registry")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTicketIsRefusedTwice(t *testing.T) {
	f := newSessionFixture(t)
	ticket := mintTicket(t, f)
	url := "ws://" + f.addr + "/ws?token=integration-token&" + WSTicketParam + "=" + ticket

	conn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	if _, _, err := websocket.Dial(context.Background(), url, nil); err == nil {
		t.Fatal("a spent ticket opened a second connection")
	}
}

// TestTicketForARevokedSessionIsRefused — the ticket names a session; it
// does not resurrect one. A revocation during the seconds a ticket is in
// flight has to win.
func TestTicketForARevokedSessionIsRefused(t *testing.T) {
	f := newSessionFixture(t)
	ticket := mintTicket(t, f)
	f.setSessionDead("sess-1")

	url := "ws://" + f.addr + "/ws?token=integration-token&" + WSTicketParam + "=" + ticket
	if _, _, err := websocket.Dial(context.Background(), url, nil); err == nil {
		t.Fatal("a ticket for a dead session opened a connection")
	}
}

func TestTicketRouteRefusesACallerNamingNoSession(t *testing.T) {
	f := newSessionFixture(t)
	f.session = ""
	resp := postJSON(t, f.addr, AuthTicketPath, struct{}{}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sessionless ticket request = %d, want 404", resp.StatusCode)
	}
	f.session = "sess-1"
	f.setRefuse(true)
	resp = postJSON(t, f.addr, AuthTicketPath, struct{}{}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("refused-session ticket request = %d, want 404", resp.StatusCode)
	}
}

// TestBootstrapPlantsTheSessionCookie — local clients get their session
// credential over the exchange that already hands out the page cookie, so
// neither needs a route of its own.
func TestBootstrapPlantsTheSessionCookie(t *testing.T) {
	f := newSessionFixture(t)
	f.setPageCredential("ao1.local-credential")
	f.srv.MarkReady()

	resp := getIntegrationBootstrap(t, f.addr)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap status = %d", resp.StatusCode)
	}
	var planted *http.Cookie
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, sessionCookiePrefix) {
			planted = cookie
		}
	}
	if planted == nil {
		t.Fatal("the bootstrap exchange planted no session cookie")
	}
	if planted.Value != "ao1.local-credential" {
		t.Fatalf("session cookie value = %q", planted.Value)
	}
	if !planted.HttpOnly {
		t.Fatal("the session cookie is readable by page script")
	}
	if planted.SameSite != http.SameSiteStrictMode {
		t.Fatal("the session cookie rides requests another site initiates")
	}
}

func TestBootstrapPlantsNoCookieWhenThereIsNoSession(t *testing.T) {
	f := newSessionFixture(t)
	f.srv.MarkReady()
	resp := getIntegrationBootstrap(t, f.addr)
	defer func() { _ = resp.Body.Close() }()
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, sessionCookiePrefix) {
			t.Fatalf("an empty credential was planted as %q", cookie.Value)
		}
	}
}

// TestSessionCredentialPrefersTheForwardedHeader — a relay forwarding a
// credential on purpose is making a statement about whose request this is;
// an ambient cookie is the browser's default. The deliberate one wins.
func TestSessionCredentialPrefersTheForwardedHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9999/ws", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName(req.Host), Value: "from-the-cookie"})
	if got := SessionCredential(req); got != "from-the-cookie" {
		t.Fatalf("cookie carrier = %q", got)
	}
	req.Header.Set(SessionCredentialHeader, "from-the-header")
	if got := SessionCredential(req); got != "from-the-header" {
		t.Fatalf("with both carriers = %q, want the header's", got)
	}
}

func TestSessionCredentialIsPortQualified(t *testing.T) {
	if sessionCookieName("127.0.0.1:1111") == sessionCookieName("127.0.0.1:2222") {
		t.Fatal("two backends on one host would overwrite each other's session cookie")
	}
	if got := sessionCookieName("example.test"); got != strings.TrimSuffix(sessionCookiePrefix, "_") {
		t.Fatalf("portless authority named the cookie %q", got)
	}
}

// TestGrantCarriesItsScopesAsAnArray — a device learns which surfaces it
// holds from the response that enrolled or renewed it, so the field has to
// survive the wire, and it has to survive it as an ARRAY even when the
// session was granted nothing.
//
// A `null` there would read as "this backend does not publish grants",
// which is the answer an older build gives, and a client cannot tell that
// apart from "granted nothing" — so the one case would silently take the
// other's fallback and offer surfaces the session does not hold.
func TestGrantCarriesItsScopesAsAnArray(t *testing.T) {
	t.Run("named grants survive", func(t *testing.T) {
		auth := &stubAuth{grant: TokenGrant{
			SessionID: "sess-1", Credential: "ao1.credential",
			Scopes: []string{string(ScopeThreadsRead), string(ScopeFilesRead)},
		}}
		f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
		resp := postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{
			Token: "pairing-token", KeyThumbprint: "thumb",
		}, nil)
		var grant TokenGrant
		if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
			t.Fatalf("decode grant: %v", err)
		}
		if len(grant.Scopes) != 2 || grant.Scopes[0] != "threads:read" || grant.Scopes[1] != "files:read" {
			t.Fatalf("scopes did not survive the wire: %+v", grant.Scopes)
		}
	})

	t.Run("an empty grant set encodes as []", func(t *testing.T) {
		auth := &stubAuth{grant: TokenGrant{
			SessionID: "sess-1", Credential: "ao1.credential", Scopes: []string{},
		}}
		f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
		resp := postJSON(t, f.srv.Addr(), AuthPairPath, PairingRedemption{
			Token: "pairing-token", KeyThumbprint: "thumb",
		}, nil)
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), `"scopes":[]`) {
			t.Fatalf("empty grant set did not encode as an array: %s", body)
		}
	})
}

// The two passkey routes are a sign-in that needs no code to type: a
// browser this backend has never seen asks for a challenge, answers it,
// and holds the same credential pair a pairing redemption would have
// produced.
func TestPasskeySignInAnswersTheSameGrantPairingDoes(t *testing.T) {
	auth := &stubAuth{
		passkeys: true,
		challenge: PasskeyChallenge{
			CeremonyID: "ceremony-1",
			Options:    json.RawMessage(`{"challenge":"abc","rpId":"backend.example"}`),
		},
		grant: TokenGrant{
			SessionID: "sess-1", Credential: "ao1.credential", ExpiresAtMs: 42,
			RefreshSecret: "refresh-1", Scopes: []string{"threads:read"},
		},
	}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })

	resp := postJSON(t, f.srv.Addr(), AuthPasskeyBeginPath, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("begin status = %d, want 200", resp.StatusCode)
	}
	var challenge PasskeyChallenge
	if err := json.NewDecoder(resp.Body).Decode(&challenge); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	if challenge.CeremonyID != "ceremony-1" {
		t.Fatalf("ceremony id did not survive the wire: %+v", challenge)
	}
	// The options blob crosses unread. A typed copy here would be a second
	// definition of the WebAuthn shape, agreeing with the library's only
	// until it grows a field.
	if string(challenge.Options) != `{"challenge":"abc","rpId":"backend.example"}` {
		t.Fatalf("options were reshaped: %s", challenge.Options)
	}

	resp = postJSON(t, f.srv.Addr(), AuthPasskeyFinishPath, PasskeyAssertion{
		CeremonyID: "ceremony-1",
		Response:   json.RawMessage(`{"id":"cred"}`),
		Label:      "A Laptop",
		Platform:   "linux",
	}, map[string]string{DeviceKeyHeader: "signed-proof"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("finish status = %d, want 200", resp.StatusCode)
	}
	var grant TokenGrant
	if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	if grant.Credential != "ao1.credential" || grant.SessionID != "sess-1" {
		t.Fatalf("grant did not survive the wire: %+v", grant)
	}
	// Nothing to confirm on a second screen: the assertion IS the owner.
	if grant.AwaitingConfirmation || grant.VerificationNumber != "" {
		t.Fatalf("a passkey sign-in asked for a confirmation: %+v", grant)
	}

	req := auth.lastAssertion(t)
	if req.CeremonyID != "ceremony-1" || req.Label != "A Laptop" {
		t.Fatalf("the route reshaped the assertion: %+v", req)
	}
	if req.DeviceProof != "signed-proof" {
		t.Fatalf("device proof = %q, want the header's value", req.DeviceProof)
	}
	if req.Method != http.MethodPost || req.Path != AuthPasskeyFinishPath {
		t.Fatalf("the route did not carry the request binding: method %q path %q", req.Method, req.Path)
	}
	if req.Peer == "" {
		t.Fatal("the peer was not filled in; the audit entry would name nobody")
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q; a credential must never be cacheable", got)
	}
}

// A proof a caller may write into the same document it is proving
// something about is not a proof, so the body carrier is never read while
// a header is present — the same rule /auth/pair applies, restated here
// because these are separate wire shapes and neither may drift.
func TestPasskeyFinishNeverReadsAProofFromTheBody(t *testing.T) {
	auth := &stubAuth{passkeys: true, grant: TokenGrant{SessionID: "s", Credential: "c"}}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })

	body := map[string]any{
		"ceremonyId":    "ceremony-1",
		"response":      json.RawMessage(`{}`),
		"keyThumbprint": "thumb-from-body",
		"deviceProof":   "forged-from-body",
	}
	postJSON(t, f.srv.Addr(), AuthPasskeyFinishPath, body, map[string]string{
		DeviceKeyHeader: "the-real-proof",
	})

	req := auth.lastAssertion(t)
	if req.DeviceProof != "the-real-proof" {
		t.Fatalf("device proof = %q; the body was allowed to name it", req.DeviceProof)
	}
	// The thumbprint DOES survive: it is an identifier a keyless device
	// asks to be known by, not a proof, and the app side ignores it
	// whenever a header proof is present.
	if req.KeyThumbprint != "thumb-from-body" {
		t.Fatalf("thumbprint = %q, want the body's", req.KeyThumbprint)
	}
}

// A backend with no canonical domain has no relying party, so the route
// exists and answers the typed refusal rather than coming and going with
// a setting — which would make a rebind part of naming a domain.
func TestPasskeyBeginAnswersATypedRefusalWithNoRelyingParty(t *testing.T) {
	auth := &stubAuth{passkeys: false}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })

	resp := postJSON(t, f.srv.Addr(), AuthPasskeyBeginPath, nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — the refusal shape every credential route shares", resp.StatusCode)
	}
	var refusal authRefusal
	if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if refusal.Reason != "passkey_unavailable" {
		t.Fatalf("reason = %q, want the typed code the client can explain", refusal.Reason)
	}
	if auth.beginCount != 1 {
		t.Fatalf("begin reached the endpoint %d times; the route answered on its own", auth.beginCount)
	}
}

// Every route in this family answers POST and nothing else, and the
// passkey pair is not an exception: a GET that started a ceremony would be
// one a foreign page could trigger by navigation.
func TestPasskeyRoutesAnswerPostOnly(t *testing.T) {
	auth := &stubAuth{passkeys: true}
	f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })

	for _, path := range []string{AuthPasskeyBeginPath, AuthPasskeyFinishPath} {
		resp, err := http.Get("http://" + f.srv.Addr() + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 405", path, resp.StatusCode)
		}
	}
}

// Availability reaches the page that needs it through the manifest,
// because a browser deciding whether to offer "sign in with a passkey"
// holds no credential and has opened no socket.
func TestBootstrapPublishesPasskeyAvailability(t *testing.T) {
	read := func(t *testing.T, f *serverFixture) Bootstrap {
		t.Helper()
		resp := getBootstrap(t, f.srv.Addr())
		defer func() { _ = resp.Body.Close() }()
		var manifest Bootstrap
		if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		return manifest
	}

	t.Run("available", func(t *testing.T) {
		auth := &stubAuth{passkeys: true}
		f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
		if !read(t, f).PasskeysAvailable {
			t.Fatal("the manifest hid an available passkey surface; the pairing screen would never offer it")
		}
	})
	t.Run("no relying party", func(t *testing.T) {
		auth := &stubAuth{passkeys: false}
		f := newServerFixtureWith(t, func(cfg *Config) { cfg.AuthEndpoints = auth })
		if read(t, f).PasskeysAvailable {
			t.Fatal("the manifest offered a surface that would refuse every ceremony")
		}
	})
	t.Run("no identity wired at all", func(t *testing.T) {
		f := newServerFixture(t)
		if read(t, f).PasskeysAvailable {
			t.Fatal("a backend with no session core advertised passkeys")
		}
	})
}

type recoveryAuthEndpoints struct{ *stubAuth }

func (*recoveryAuthEndpoints) SupportsRefreshRecovery() bool { return true }

func TestRecoverableTokenRouteRequiresCapabilityAndSuccessor(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(fmt.Sprint(capable), func(t *testing.T) {
			stub := &stubAuth{renewGood: true, grant: TokenGrant{SessionID: "session", Credential: "credential"}}
			f := newServerFixtureWith(t, func(cfg *Config) {
				if capable {
					cfg.AuthEndpoints = &recoveryAuthEndpoints{stub}
				} else {
					cfg.AuthEndpoints = stub
				}
			})
			resp := postJSON(t, f.srv.Addr(), AuthTokenRecoverPath, map[string]string{"refreshSecret": "old"}, nil)
			defer resp.Body.Close()
			want := http.StatusNotFound
			if capable {
				want = http.StatusBadRequest
			}
			if resp.StatusCode != want {
				t.Fatalf("status=%d want=%d", resp.StatusCode, want)
			}
			stub.mu.Lock()
			called := len(stub.renewals) != 0
			stub.mu.Unlock()
			if called {
				t.Fatal("invalid recovery reached identity")
			}
			if !capable {
				return
			}
			resp2 := postJSON(t, f.srv.Addr(), AuthTokenRecoverPath, map[string]string{"refreshSecret": "old", "nextRefreshSecret": "next"}, nil)
			defer resp2.Body.Close()
			if resp2.StatusCode != 200 || resp2.Header.Get(RefreshRecoveryHeader) != "1" {
				t.Fatal("recoverable route did not advertise support")
			}
			req := stub.lastRenewal(t)
			if req.Path != AuthTokenRecoverPath || req.NextRefreshSecret != "next" {
				t.Fatal("recovery path lost proof binding or successor")
			}
		})
	}
}
