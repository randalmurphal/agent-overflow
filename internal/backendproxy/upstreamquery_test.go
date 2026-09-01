package backendproxy

import (
	"net/url"
	"testing"
)

// TestUpstreamQuery_OperatorParametersWin — the configured endpoint owns
// its query and the page cannot see it, so a page parameter that
// overwrote one would be the page reconfiguring the hop.
func TestUpstreamQuery_OperatorParametersWin(t *testing.T) {
	page := url.Values{"did": {"screen-abcdef01"}, "conn": {"live-abcdef01"}}
	got, err := url.ParseQuery(upstreamQuery("did=operator-pinned-id&region=eu", page, ""))
	if err != nil {
		t.Fatalf("parse the assembled query: %v", err)
	}
	if got.Get("did") != "operator-pinned-id" {
		t.Errorf("did = %q, want the operator's value", got.Get("did"))
	}
	if got.Get("region") != "eu" {
		t.Errorf("region = %q, want the operator's value preserved", got.Get("region"))
	}
	if got.Get("conn") != "live-abcdef01" {
		t.Errorf("conn = %q, want the page's value where the operator named none", got.Get("conn"))
	}
}

// TestUpstreamQuery_UnparseableOperatorQueryIsForwardedVerbatim — an
// configured URL we cannot parse is one we must not rewrite from a guess.
func TestUpstreamQuery_UnparseableOperatorQueryIsForwardedVerbatim(t *testing.T) {
	const operator = "%zz"
	page := url.Values{"did": {"screen-abcdef01"}}
	if got := upstreamQuery(operator, page, ""); got != operator {
		t.Fatalf("upstreamQuery = %q, want %q", got, operator)
	}
}

// TestUpstreamQuery_TicketWinsOutright — the ticket is not configuration.
// It is this handshake's credential, minted seconds ago and good for one
// use, so an configured URL still carrying a spent one must not be what the
// upgrade presents.
func TestUpstreamQuery_TicketWinsOutright(t *testing.T) {
	got, err := url.ParseQuery(upstreamQuery("ticket=already-spent&region=eu", url.Values{}, "minted-for-this-handshake"))
	if err != nil {
		t.Fatalf("parse the assembled query: %v", err)
	}
	if got.Get("ticket") != "minted-for-this-handshake" {
		t.Errorf("ticket = %q, want the freshly minted one", got.Get("ticket"))
	}
	if len(got["ticket"]) != 1 {
		t.Errorf("ticket appears %d times, want the stale one replaced rather than appended", len(got["ticket"]))
	}
	if got.Get("region") != "eu" {
		t.Errorf("region = %q, want the operator's other values preserved", got.Get("region"))
	}
}
