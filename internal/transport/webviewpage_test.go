package transport

import (
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strings"
	"testing"

	"agent-overflow/internal/pagehost"
)

// The webview half of the page-URL contract. A browser can only be told
// things through its URL, so its ticket rides `?t=`. A host that owns
// the window it is navigating gets the URL and the ticket apart, and
// nothing credential-shaped reaches the URL at all — which is the whole
// property these cases pin, since a URL is copyable, lands in launcher
// logs and window diagnostics, and outlives its single use.

// TestWebviewPageURLCarriesNoTicket is the invariant the wave exists for.
func TestWebviewPageURLCarriesNoTicket(t *testing.T) {
	f := newServerFixture(t)

	pageURL := f.srv.WebviewPageURL()
	if pageURL == "" {
		t.Fatal("WebviewPageURL is empty on a started server")
	}
	parsed, err := neturl.Parse(pageURL)
	if err != nil {
		t.Fatalf("parse webview page url: %v", err)
	}
	if got := parsed.Query().Get(PageTicketParam); got != "" {
		t.Fatalf("webview page url carries a ticket %q", got)
	}
	if !pagehost.IsWebviewURL(pageURL) {
		t.Fatalf("webview page url is not marked as webview-hosted: %q", pageURL)
	}
	// Nothing else on the URL may be the launch credential either.
	if strings.Contains(pageURL, f.srv.Token()) {
		t.Fatal("the session token appears in the webview page url")
	}
}

// TestInjectedTicketBuysTheCookie proves the ticket a window host
// injects is an ORDINARY page ticket: same book, same exchange, same
// cookie. Only the delivery channel moved.
func TestInjectedTicketBuysTheCookie(t *testing.T) {
	f := newServerFixture(t)

	ticket, err := f.srv.MintPageTicket()
	if err != nil {
		t.Fatalf("mint page ticket: %v", err)
	}
	resp := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	cookie := pageCookieFrom(t, resp)
	if !cookie.HttpOnly {
		t.Fatal("the injected ticket bought a script-readable cookie")
	}

	// Single use, like every other page ticket.
	second := bootstrapWithTicket(t, f.srv.Addr(), ticket)
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Fatalf("replayed ticket status = %d, want 404", second.StatusCode)
	}
}

// TestWebviewPageURLMintsNothing is the reason the URL and the ticket are
// separate calls at all: a host re-reads this address on every reload and
// on every rebind, and a producer that minted per read would churn the
// ticket book for pages nobody is loading.
func TestWebviewPageURLMintsNothing(t *testing.T) {
	f := newServerFixture(t)

	before := f.srv.cred.tickets.outstanding()
	for range 5 {
		if got := f.srv.WebviewPageURL(); got == "" {
			t.Fatal("WebviewPageURL is empty on a started server")
		}
	}
	if after := f.srv.cred.tickets.outstanding(); after != before {
		t.Fatalf("outstanding tickets moved %d -> %d; the URL producer minted", before, after)
	}
}

// TestPageURLRouteSplitsForWebviewHosts covers the /pageurl answer the
// Windows launcher reads. The default shape stays exactly what a
// browser-pointing caller (`ao-harness`, the e2e rig) parses today.
func TestPageURLRouteSplitsForWebviewHosts(t *testing.T) {
	f := newServerFixture(t)

	ask := func(t *testing.T, query string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "http://"+f.srv.Addr()+PageURLPath+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("page url request: %v", err)
		}
		return resp
	}

	t.Run("browsers still get one ticketed URL as plain text", func(t *testing.T) {
		resp := ask(t, "")
		defer resp.Body.Close()
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
			t.Fatalf("content-type = %q, want text/plain", got)
		}
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		pageURL := strings.TrimSpace(string(body[:n]))
		if ticketFromURL(t, pageURL) == "" {
			t.Fatalf("plain-text page url carries no ticket: %q", pageURL)
		}
	})

	t.Run("window hosts get the URL and the ticket apart", func(t *testing.T) {
		resp := ask(t, "?"+pagehost.Param+"="+pagehost.Webview)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("content-type = %q, want application/json", got)
		}
		var answer pagehost.Answer
		if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
			t.Fatalf("decode answer: %v", err)
		}
		if answer.URL == "" || answer.Ticket == "" {
			t.Fatalf("answer = %+v, want both halves", answer)
		}
		if strings.Contains(answer.URL, answer.Ticket) {
			t.Fatal("the ticket rode the URL it was supposed to be split from")
		}
		if !pagehost.IsWebviewURL(answer.URL) {
			t.Fatalf("answer URL is not marked as webview-hosted: %q", answer.URL)
		}
		// The ticket is live: the launcher injects it and the document
		// exchanges it exactly as a URL-borne one is exchanged.
		exchanged := bootstrapWithTicket(t, f.srv.Addr(), answer.Ticket)
		defer exchanged.Body.Close()
		if exchanged.StatusCode != http.StatusOK {
			t.Fatalf("exchange status = %d, want 200", exchanged.StatusCode)
		}
	})

	t.Run("refuses a caller with no credential", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, "http://"+f.srv.Addr()+PageURLPath+"?"+pagehost.Param+"="+pagehost.Webview, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("page url request: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// TestPageURLDecoratorAppliesToBothShapes: the boot's own parameters (the
// client id, the harness page marker) are the same in either shape, so a
// window host and a browser open the SAME page — a URL missing the client
// id silently changes which ui_state bucket the frontend reads.
func TestPageURLDecoratorAppliesToBothShapes(t *testing.T) {
	f := newServerFixtureWith(t, func(cfg *Config) {
		cfg.DecoratePageURL = func(base string) string { return base + "&cid=boot-1" }
	})

	req := func(t *testing.T, query string) string {
		t.Helper()
		r, err := http.NewRequest(http.MethodGet, "http://"+f.srv.Addr()+PageURLPath+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("page url request: %v", err)
		}
		defer resp.Body.Close()
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		return strings.TrimSpace(string(body[:n]))
	}

	if plain := req(t, ""); !strings.Contains(plain, "cid=boot-1") {
		t.Fatalf("plain page url lost the boot's parameters: %q", plain)
	}
	raw := req(t, "?"+pagehost.Param+"="+pagehost.Webview)
	var answer pagehost.Answer
	if err := json.Unmarshal([]byte(raw), &answer); err != nil {
		t.Fatalf("decode answer %q: %v", raw, err)
	}
	if !strings.Contains(answer.URL, "cid=boot-1") {
		t.Fatalf("webview page url lost the boot's parameters: %q", answer.URL)
	}
}
