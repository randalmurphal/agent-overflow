package main

import (
	"strings"
	"testing"

	"agent-overflow/internal/transport"
)

// TestAppURLWithClientID pins the one rule every windowed boot shares.
// `&` rather than `?` is load-bearing: a transport page URL already
// carries its one-time ticket as a query param, so `?` would fold the
// ticket into the cid value and the page would arrive with nothing to
// exchange.
func TestAppURLWithClientID(t *testing.T) {
	const page = "http://127.0.0.1:34567/?t=page-ticket"
	if got := appURLWithClientID(page, "cid-1"); got != page+"&cid=cid-1" {
		t.Fatalf("appURLWithClientID = %q", got)
	}
	// Both degradations pass through rather than producing a broken URL.
	if got := appURLWithClientID(page, ""); got != page {
		t.Fatalf("empty client id changed the URL: %q", got)
	}
	if got := appURLWithClientID("", "cid-1"); got != "" {
		t.Fatalf("empty URL gained a cid: %q", got)
	}
	if got := appURLWithClientID(page, "a b&c"); !strings.Contains(got, "cid=a+b%26c") {
		t.Fatalf("client id was not query-escaped: %q", got)
	}
}

// newBootstrapTestServer builds a real (unstarted) transport server. A
// zero-value one will not do: the server owns the launch credential that
// mints page tickets, and a bootstrap is partly a description of it.
func newBootstrapTestServer(t *testing.T) *transport.Server {
	t.Helper()
	srv, err := transport.New(transport.Config{
		Dispatcher: transport.NewDispatcher(),
		EventBus:   transport.NewEventBus(4),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	return srv
}

// TestHarnessBootstrapCarriesTheClientID: the harness bootstrap is the
// only description an attaching tool gets, and it used to omit the
// durable UI-state identity entirely — so a harness page (and the
// launcher's WebView2 window over a --soak backend) got a fresh ui_state
// bucket on every port change, and every persisted UI preference an e2e
// spec set read back as unset.
func TestHarnessBootstrapCarriesTheClientID(t *testing.T) {
	// bootSettingsDir honors this override, so the id is minted inside the
	// test's own tree and never beside the developer's settings.json.
	prev := dataDirRoot
	dataDirRoot = t.TempDir()
	t.Cleanup(func() { dataDirRoot = prev })

	want := ensureClientID()
	if want == "" {
		t.Fatal("ensureClientID returned empty under a writable data root")
	}

	bs := newHarnessBootstrap(newBootstrapTestServer(t), harnessPaths{DataRoot: dataDirRoot}, nil)
	if bs.ClientID != want {
		t.Fatalf("bootstrap clientId = %q, want %q", bs.ClientID, want)
	}
	// Stable across boots: a minted-per-boot id would defeat the point.
	if again := newHarnessBootstrap(newBootstrapTestServer(t), harnessPaths{}, nil); again.ClientID != want {
		t.Fatalf("clientId changed between boots: %q then %q", want, again.ClientID)
	}
}
