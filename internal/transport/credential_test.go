package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The credential channel, end to end. Each case here is one link in the
// chain a page walks: it arrives with a one-time ticket on its URL,
// trades it for a cookie it cannot read, and uses that cookie for the
// manifest refetch and the WebSocket upgrade. Clients that are not
// browsers present the session token instead, through the same
// validation function.

// pageURLTicket asks a live server for a page URL and returns its
// ticket.
func pageURLTicket(t *testing.T, srv *Server) string {
	t.Helper()
	parsed, err := neturl.Parse(srv.AppURL())
	if err != nil {
		t.Fatalf("parse app url: %v", err)
	}
	ticket := parsed.Query().Get(PageTicketParam)
	if ticket == "" {
		t.Fatal("app url carries no page ticket")
	}
	return ticket
}

// bootstrapWithTicket performs the first-contact manifest fetch: a GET
// carrying nothing but the one-time ticket, exactly as a freshly loaded
// page does.
func bootstrapWithTicket(t *testing.T, addr, ticket string) *http.Response {
	t.Helper()
	url := fmt.Sprintf("http://%s/bootstrap.json?%s=%s", addr, PageTicketParam, neturl.QueryEscape(ticket))
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("bootstrap with ticket: %v", err)
	}
	return resp
}

// pageCookieFrom pulls the issued page cookie out of a response, failing
// the test when none was set.
func pageCookieFrom(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, pageCookiePrefix) {
			return cookie
		}
	}
	t.Fatal("response set no page cookie")
	return nil
}

// TestCredential_TicketExchangeIssuesHttpOnlyCookie is the first
// contact: a page URL's ticket buys a session cookie, and every
// attribute of that cookie is a decision the browser has to honour for
// the credential to stay out of script's reach.
func TestCredential_TicketExchangeIssuesHttpOnlyCookie(t *testing.T) {
	f := newServerFixture(t)
	ticket := pageURLTicket(t, f.srv)

	resp := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap with a fresh ticket = %d, want 200", resp.StatusCode)
	}
	cookie := pageCookieFrom(t, resp)
	if !cookie.HttpOnly {
		t.Error("page cookie is not HttpOnly, so page script can read the credential")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("page cookie SameSite = %v, want Strict", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("page cookie Path = %q, want /", cookie.Path)
	}
	if cookie.Domain != "" {
		t.Errorf("page cookie Domain = %q, want host-only", cookie.Domain)
	}
	if !cookie.Expires.IsZero() || cookie.MaxAge != 0 {
		t.Error("page cookie is not a session cookie; it is minted per launch and must not outlive one")
	}
	// A loopback listener is plain http, where a Secure cookie would
	// never be stored at all.
	if cookie.Secure {
		t.Error("page cookie is Secure on a plain-http listener; the browser would discard it")
	}
	if cookie.Value == ticket {
		t.Error("the cookie is the ticket; a spent ticket must not be reusable as a session")
	}
}

// TestCredential_TicketIsSingleUse is the property the URL exists for:
// what travels through history, shell arguments and logs buys one cookie
// and nothing more.
func TestCredential_TicketIsSingleUse(t *testing.T) {
	f := newServerFixture(t)
	ticket := pageURLTicket(t, f.srv)

	first := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first exchange = %d, want 200", first.StatusCode)
	}

	// Same ticket, a client holding no cookie — a copied URL opened in
	// another browser, or a stale bookmark.
	second := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	_ = second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Fatalf("second exchange of the same ticket = %d, want 404", second.StatusCode)
	}
}

// TestCredential_CookieCarriesTheReloadAndRefetch covers the two
// requests that follow first contact: the SPA's manifest refetch, and a
// reload of the URL after the ticket was scrubbed from it. Both present
// the cookie alone.
func TestCredential_CookieCarriesTheReloadAndRefetch(t *testing.T) {
	f := newServerFixture(t)
	ticket := pageURLTicket(t, f.srv)

	first := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	_ = first.Body.Close()
	cookie := pageCookieFrom(t, first)

	req, err := http.NewRequest(http.MethodGet, "http://"+f.srv.Addr()+"/bootstrap.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("refetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ticketless refetch with the cookie = %d, want 200", resp.StatusCode)
	}
	// The refetch must not burn a second cookie issuance: the request
	// already authenticated, so there is nothing to exchange.
	if len(resp.Cookies()) != 0 {
		t.Errorf("refetch re-issued cookies: %v", resp.Cookies())
	}
}

// TestCredential_BootstrapWithoutAnyCredentialIsRefused pins the shape
// of the refusal as much as the refusal: 404 is what a path that does
// not exist answers, so nothing about the response identifies this
// server to a scanner.
func TestCredential_BootstrapWithoutAnyCredentialIsRefused(t *testing.T) {
	f := newServerFixture(t)

	for _, tc := range []struct{ name, query string }{
		{name: "nothing presented", query: ""},
		{name: "ticket never minted", query: "?" + PageTicketParam + "=never-minted"},
		{name: "session token in the ticket slot", query: "?" + PageTicketParam + "=test-token"},
		{name: "wrong session token", query: "?" + SessionTokenParam + "=wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get("http://" + f.srv.Addr() + "/bootstrap.json" + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
			if len(resp.Cookies()) != 0 {
				t.Errorf("a refused request was issued cookies: %v", resp.Cookies())
			}
		})
	}
}

// TestCredential_StaleTicketAfterRestartFailsCleanly is the bookmarked
// URL after the backend restarted. The new launch minted a new
// credential and knows nothing of the old ticket, so the page gets the
// ordinary refusal the SPA already handles — not a wedge, and not a
// different code path.
func TestCredential_StaleTicketAfterRestartFailsCleanly(t *testing.T) {
	first := newServerFixture(t)
	staleTicket := pageURLTicket(t, first.srv)
	staleAddr := first.srv.Addr()

	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := first.srv.Shutdown(shutCtx); err != nil {
		t.Fatalf("shutdown first launch: %v", err)
	}

	// A second launch on a port of its own, standing in for the restart.
	second := newServerFixture(t)
	if second.srv.Addr() == staleAddr {
		t.Skip("second launch reused the first launch's port")
	}
	resp := bootstrapWithTicket(t, second.srv.Addr(), staleTicket)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("stale ticket at a restarted backend = %d, want 404", resp.StatusCode)
	}
	// And the restarted backend still serves a page that asks properly,
	// which is what makes the refusal recoverable by re-opening the app.
	fresh := bootstrapWithTicket(t, second.srv.Addr(), pageURLTicket(t, second.srv))
	defer fresh.Body.Close()
	if fresh.StatusCode != http.StatusOK {
		t.Fatalf("fresh ticket after restart = %d, want 200", fresh.StatusCode)
	}
}

// TestCredential_UpgradeWithCookie is the browser's own path: no
// credential in the URL, the cookie doing the work.
func TestCredential_UpgradeWithCookie(t *testing.T) {
	f := newServerFixture(t)
	ticket := pageURLTicket(t, f.srv)
	exchange := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	_ = exchange.Body.Close()
	cookie := pageCookieFrom(t, exchange)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	conn, _, err := websocket.Dial(context.Background(), "ws://"+f.srv.Addr()+"/ws", &websocket.DialOptions{
		HTTPHeader: header,
	})
	if err != nil {
		t.Fatalf("upgrade with the page cookie: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// TestCredential_UpgradeRefusesForeignOrigin is the load-bearing
// cross-origin control. Cookies are scoped by host and not by port, so a
// page served by any other listener on this host is sent our cookie by
// the browser, and a WebSocket handshake is not subject to the
// cross-origin read rules that keep such a page away from
// /bootstrap.json. The Origin check refuses the handshake before the
// credential is even consulted, which is why this case presents a
// perfectly valid cookie.
func TestCredential_UpgradeRefusesForeignOrigin(t *testing.T) {
	f := newServerFixture(t)
	ticket := pageURLTicket(t, f.srv)
	exchange := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	_ = exchange.Body.Close()
	cookie := pageCookieFrom(t, exchange)

	_, port, err := splitHostPortForTest(f.srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{
		"http://evil.example",
		// Another listener on this very host: same cookie scope,
		// different principal.
		"http://127.0.0.1:" + neighbourPort(port),
		// A sandboxed document names no authority at all.
		"null",
		// Right authority, wrong scheme.
		"https://127.0.0.1:" + port,
	} {
		t.Run(origin, func(t *testing.T) {
			header := http.Header{}
			header.Set("Cookie", cookie.Name+"="+cookie.Value)
			header.Set("Origin", origin)
			conn, resp, err := websocket.Dial(context.Background(), "ws://"+f.srv.Addr()+"/ws", &websocket.DialOptions{
				HTTPHeader: header,
			})
			if err == nil {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				t.Fatalf("upgrade from origin %q succeeded", origin)
			}
			if resp != nil && resp.StatusCode != http.StatusNotFound {
				t.Fatalf("refusal status = %d, want 404 (indistinguishable from an unknown path)", resp.StatusCode)
			}
		})
	}
}

// TestCredential_UpgradeAcceptsOwnOrigin is the other half: the page the
// server itself served names this very authority, and must be let
// through.
func TestCredential_UpgradeAcceptsOwnOrigin(t *testing.T) {
	f := newServerFixture(t)
	ticket := pageURLTicket(t, f.srv)
	exchange := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	_ = exchange.Body.Close()
	cookie := pageCookieFrom(t, exchange)

	header := http.Header{}
	header.Set("Cookie", cookie.Name+"="+cookie.Value)
	header.Set("Origin", "http://"+f.srv.Addr())
	conn, _, err := websocket.Dial(context.Background(), "ws://"+f.srv.Addr()+"/ws", &websocket.DialOptions{
		HTTPHeader: header,
	})
	if err != nil {
		t.Fatalf("upgrade from this server's own origin: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// TestCredential_UpgradeWithoutOriginTakesTheTokenPath is every client
// that is not a browser: the harness CLI, the e2e rig's Node socket, the
// WSL notification client. They send no Origin, and they present the
// session token — through the same Authenticate the cookie path uses.
func TestCredential_UpgradeWithoutOriginTakesTheTokenPath(t *testing.T) {
	f := newServerFixture(t)

	t.Run("query carrier", func(t *testing.T) {
		conn, _, err := websocket.Dial(context.Background(),
			"ws://"+f.srv.Addr()+"/ws?"+SessionTokenParam+"=test-token", nil)
		if err != nil {
			t.Fatalf("upgrade with the session token in the query: %v", err)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})

	t.Run("bearer carrier", func(t *testing.T) {
		header := http.Header{}
		header.Set("Authorization", "Bearer test-token")
		conn, _, err := websocket.Dial(context.Background(), "ws://"+f.srv.Addr()+"/ws", &websocket.DialOptions{
			HTTPHeader: header,
		})
		if err != nil {
			t.Fatalf("upgrade with a bearer session token: %v", err)
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})

	t.Run("no credential at all", func(t *testing.T) {
		conn, resp, err := websocket.Dial(context.Background(), "ws://"+f.srv.Addr()+"/ws", nil)
		if err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			t.Fatal("upgrade without any credential succeeded")
		}
		if resp != nil && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("refusal status = %d, want 404", resp.StatusCode)
		}
	})
}

// TestCredential_PageURLRouteMintsForTokenHolders covers the route that
// exists because a page URL is single-use: the shells that navigate more
// than once over a backend's life (the Windows launcher's reload, the
// harness CLI, the e2e rig's per-test browser context) ask for a fresh
// one with the session token they already hold.
func TestCredential_PageURLRouteMintsForTokenHolders(t *testing.T) {
	f := newServerFixture(t)

	ask := func(t *testing.T, mutate func(*http.Request)) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+f.srv.Addr()+PageURLPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("page url request: %v", err)
		}
		return resp
	}

	withToken := func(r *http.Request) { r.Header.Set("Authorization", "Bearer test-token") }

	t.Run("mints a distinct ticket per call", func(t *testing.T) {
		seen := map[string]bool{}
		for range 3 {
			resp := ask(t, withToken)
			if resp.StatusCode != http.StatusOK {
				_ = resp.Body.Close()
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			body := make([]byte, 4096)
			n, _ := resp.Body.Read(body)
			_ = resp.Body.Close()
			ticket := ticketFromURL(t, strings.TrimSpace(string(body[:n])))
			if seen[ticket] {
				t.Fatal("two calls handed out the same ticket")
			}
			seen[ticket] = true
		}
	})

	t.Run("refuses a caller with no credential", func(t *testing.T) {
		resp := ask(t, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})

	// A browser is never the caller here, but the Origin rule is the
	// server's, not the route's: nothing that another origin initiated
	// gets to mint a ticket.
	t.Run("refuses a foreign origin", func(t *testing.T) {
		resp := ask(t, func(r *http.Request) {
			withToken(r)
			r.Header.Set("Origin", "http://evil.example")
		})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// TestCredential_CookieNameIsPortQualified pins why the name carries the
// port: two backends on one host (the user's app and a harness instance,
// an app and a --connect stub) write cookies to the same host scope, and
// a shared name would have each overwriting the other's session.
func TestCredential_CookieNameIsPortQualified(t *testing.T) {
	if got, want := pageCookieName("127.0.0.1:34567"), pageCookiePrefix+"34567"; got != want {
		t.Errorf("pageCookieName(host:port) = %q, want %q", got, want)
	}
	if pageCookieName("127.0.0.1:34567") == pageCookieName("127.0.0.1:34568") {
		t.Error("two ports share a cookie name; each backend would clobber the other's session")
	}
	// A reverse proxy fronting us on a default port sends a bare
	// authority; the name still has to be stable and legal.
	if got := pageCookieName("app.example.com"); got == "" || strings.HasSuffix(got, "_") {
		t.Errorf("pageCookieName(bare host) = %q, want a stable name", got)
	}
}

// TestCredential_OutstandingTicketsAreBounded pins the ring: a producer
// of page URLs (the settings panel rebuilds the LAN share URL on every
// mount) must not be able to grow this without limit, and the newest URL
// — the one a user just copied — must always be the one that survives.
func TestCredential_OutstandingTicketsAreBounded(t *testing.T) {
	cred, err := NewCredential("session")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	var tickets []string
	for range maxOutstandingTickets * 3 {
		ticket, err := cred.MintPageTicket()
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		tickets = append(tickets, ticket)
	}
	cred.mu.Lock()
	held := len(cred.tickets)
	cred.mu.Unlock()
	if held > maxOutstandingTickets {
		t.Fatalf("outstanding tickets = %d, want at most %d", held, maxOutstandingTickets)
	}
	if !cred.consumeTicket(tickets[len(tickets)-1]) {
		t.Fatal("the newest ticket was evicted")
	}
	if cred.consumeTicket(tickets[0]) {
		t.Fatal("the oldest ticket survived past the cap")
	}
}

// TestCredential_AuthenticateIgnoresForeignCookieOfTheSameName covers a
// cookie another page on this host wrote on a different path: browsers
// send it alongside ours, sometimes first. Reading every cookie of the
// name is what keeps it from being mistaken for the live session.
func TestCredential_AuthenticateIgnoresForeignCookieOfTheSameName(t *testing.T) {
	cred, err := NewCredential("session")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:34567/bootstrap.json", nil)
	name := pageCookieName(req.Host)
	req.AddCookie(&http.Cookie{Name: name, Value: "someone-elses"})
	if cred.Authenticate(req) {
		t.Fatal("a foreign cookie authenticated")
	}
	req.AddCookie(&http.Cookie{Name: name, Value: "session"})
	if !cred.Authenticate(req) {
		t.Fatal("the live cookie was missed because a foreign one came first")
	}
}

// splitHostPortForTest is net.SplitHostPort with the test's error
// handling folded in.
func splitHostPortForTest(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", errors.New("address carries no port")
	}
	return addr[:i], addr[i+1:], nil
}

// neighbourPort returns a port that is not this server's, standing in
// for another listener on the same host.
func neighbourPort(port string) string {
	if port == "1" {
		return "2"
	}
	return "1"
}
