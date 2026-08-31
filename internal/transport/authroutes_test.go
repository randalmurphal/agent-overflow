package transport

import (
	"bytes"
	"context"
	"encoding/json"
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
	mu        sync.Mutex
	pairing   []PairingRedemption
	renewals  []SessionRenewal
	grant     TokenGrant
	reason    string
	renewGood bool
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

	body := map[string]string{"refreshSecret": "secret-1", "keyThumbprint": "thumb-from-the-body"}
	resp := postJSON(t, f.srv.Addr(), AuthTokenPath, body,
		map[string]string{DeviceKeyHeader: "thumb-from-the-header"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	req := auth.lastRenewal(t)
	if req.RefreshSecret != "secret-1" {
		t.Fatalf("refresh secret = %q", req.RefreshSecret)
	}
	if req.KeyThumbprint != "thumb-from-the-header" {
		t.Fatalf("key thumbprint = %q, want the header's", req.KeyThumbprint)
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
