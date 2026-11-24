package network

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/transport"
)

// TestBindHost_BranchesOnFlag locks the bind-host mapping so a
// future refactor doesn't accidentally widen the loopback bind to
// 0.0.0.0 (or vice versa), and so a loopback Reach never leaves
// 127.0.0.1 whatever the toggle says.
func TestBindHost_BranchesOnFlag(t *testing.T) {
	for _, tc := range []struct {
		reach   Reach
		bindAll bool
		want    string
	}{
		{Reach{}, false, "127.0.0.1"},
		{Reach{}, true, "0.0.0.0"},
		{LoopbackReach(), false, "127.0.0.1"},
		{LoopbackReach(), true, "127.0.0.1"},
	} {
		if got := tc.reach.BindHost(tc.bindAll); got != tc.want {
			t.Fatalf("Reach{loopback: %v}.BindHost(%v) = %q, want %q", tc.reach.LoopbackOnly(), tc.bindAll, got, tc.want)
		}
	}
}

// TestOriginPatterns_ExactAllowList pins each allow-list as an exact list,
// which also proves no entry carries a wildcard port.
func TestOriginPatterns_ExactAllowList(t *testing.T) {
	bindAllLAN := []string{
		"http://127.0.0.1:34115", "https://127.0.0.1:34115",
		"http://localhost:34115", "https://localhost:34115",
		"http://192.168.1.10:34115", "https://192.168.1.10:34115",
	}
	for _, tc := range []struct {
		name    string
		bindAll bool
		lanIP   string
		domain  string
		port    int
		want    []string
	}{
		// With no LAN bind and no canonical domain there is nothing to add,
		// and nil is not "accept anything": transport.OriginAllowed still
		// admits only the authority the request was addressed to.
		{"loopback is nil", false, "", "", 34115, nil},
		// The wave-9 defect fix. Every entry names the bound port, under
		// both schemes the one listener answers. A wildcard port here
		// admitted a document served by ANY port on this machine, and the
		// browser attaches the page cookie to the handshake such a document
		// opens.
		{"bind-all is exact port", true, "192.168.1.10", "", 34115, bindAllLAN},
		// The LAN-IP-missing case still produces a usable allow-list: the
		// loopback origins on the bound port.
		{"bind-all, no LAN", true, "", "", 8080, []string{
			"http://127.0.0.1:8080", "https://127.0.0.1:8080",
			"http://localhost:8080", "https://localhost:8080",
		}},
		// A canonical domain names its own origins on either bind, in two
		// exact spellings: the bare name for a proxy that fronts this
		// backend on 443, and the bound port for one that does not, which
		// matters when something in front terminates TLS and the request
		// reaching this backend is cleartext.
		{"domain, loopback", false, "", "backend.example", 34115, []string{
			"https://backend.example", "https://backend.example:34115",
		}},
		{"domain, LAN", true, "192.168.1.10", "backend.example", 34115,
			slices.Concat(bindAllLAN, []string{"https://backend.example", "https://backend.example:34115"})},
		// A port this process has not resolved yet drops every port-bearing
		// pattern rather than guessing one. Failing closed leaves the
		// request's own authority as the whole admission.
		{"port 0, bind-all", true, "192.168.1.10", "", 0, nil},
		{"port 0, domain", false, "", "backend.example", 0, []string{"https://backend.example"}},
		{"port -1, bind-all", true, "192.168.1.10", "", -1, nil},
		{"port -1, domain", false, "", "backend.example", -1, []string{"https://backend.example"}},
		{"port 70000, bind-all", true, "192.168.1.10", "", 70000, nil},
		{"port 70000, domain", false, "", "backend.example", 70000, []string{"https://backend.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := OriginPatterns(tc.bindAll, tc.lanIP, tc.domain, tc.port); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("OriginPatterns(%v, %q, %q, %d) = %#v, want %#v", tc.bindAll, tc.lanIP, tc.domain, tc.port, got, tc.want)
			}
		})
	}
}

// TestOriginPatternsRefuseAnotherPortOnThisHost is the enforcement half
// of the exact-port rule, run through the function that actually gates
// the socket. A bind-all install used to admit `http://localhost:5173`,
// which is any dev server, any preview listener, and any other tool that
// serves a document on this machine; the browser attaches this backend's
// page cookie to a handshake such a document opens, because cookies are
// scoped by host and not by port.
func TestOriginPatternsRefuseAnotherPortOnThisHost(t *testing.T) {
	patterns := OriginPatterns(true, "192.168.1.10", "", 34115)

	// The SPA's own origin, on the port this listener bound.
	for _, origin := range []string{
		"http://localhost:34115",
		"http://127.0.0.1:34115",
		"http://192.168.1.10:34115",
		"https://192.168.1.10:34115",
	} {
		request := httptest.NewRequest(http.MethodGet, "http://192.168.1.10:34115/ws", nil)
		request.Header.Set("Origin", origin)
		if !transport.OriginAllowed(request, patterns) {
			t.Errorf("origin %q was refused; it is this listener's own page origin", origin)
		}
	}

	// Any other port on the same hosts, and the LAN neighbour that is not
	// this machine.
	for _, origin := range []string{
		"http://localhost:5173",
		"https://localhost:5173",
		"http://127.0.0.1:5173",
		"http://192.168.1.10:5173",
		"https://192.168.1.10:3000",
		"http://192.168.1.11:34115",
	} {
		request := httptest.NewRequest(http.MethodGet, "http://192.168.1.10:34115/ws", nil)
		request.Header.Set("Origin", origin)
		if transport.OriginAllowed(request, patterns) {
			t.Errorf("origin %q was admitted; only this listener's own port may open a socket here", origin)
		}
	}
}

// TestDiscoverLocalLANIP swaps in fake interface enumeration and checks the
// address discovery picks, twice, so a multi-homed host returns the same
// answer across runs.
func TestDiscoverLocalLANIP(t *testing.T) {
	prevIfaces, prevAddrs := Interfaces, InterfaceAddrs
	t.Cleanup(func() { Interfaces, InterfaceAddrs = prevIfaces, prevAddrs })
	ipNet := func(a, b, c, d byte, bits int) []net.Addr {
		return []net.Addr{&net.IPNet{IP: net.IPv4(a, b, c, d), Mask: net.CIDRMask(bits, 32)}}
	}
	up := net.FlagUp | net.FlagRunning
	for _, tc := range []struct {
		name   string
		ifaces []net.Interface
		addrs  map[int][]net.Addr
		want   string
	}{
		// Two RFC1918 interfaces presented in reverse-Index order. Without
		// sorting, the result would be 10.0.0.5 (the first one seen); with
		// sorting it must be 192.168.1.10 (Index 1).
		{"deterministic order", []net.Interface{
			{Index: 2, Name: "eth1", Flags: up},
			{Index: 1, Name: "eth0", Flags: up},
		}, map[int][]net.Addr{1: ipNet(192, 168, 1, 10, 24), 2: ipNet(10, 0, 0, 5, 8)}, "192.168.1.10"},
		// The ONLY non-loopback IPv4 is a Tailscale CGNAT address
		// (100.64.0.0/10): it is still returned rather than empty, or a
		// Tailscale-only host has no usable bind-all URL.
		{"tailscale only", []net.Interface{
			{Index: 1, Name: "tailscale0", Flags: up},
		}, map[int][]net.Addr{1: ipNet(100, 96, 5, 42, 10)}, "100.96.5.42"},
		// Tailscale on the lower index: the RFC1918 address still wins, so a
		// phone on the same Wi-Fi is not handed a Tailscale URL.
		{"RFC1918 beats tailscale", []net.Interface{
			{Index: 1, Name: "tailscale0", Flags: up},
			{Index: 2, Name: "en0", Flags: up},
		}, map[int][]net.Addr{1: ipNet(100, 96, 5, 42, 10), 2: ipNet(192, 168, 1, 10, 24)}, "192.168.1.10"},
		// Loopback and down interfaces never contribute to the LAN URL.
		{"skips loopback and down", []net.Interface{
			{Index: 1, Name: "lo", Flags: up | net.FlagLoopback},
			{Index: 2, Name: "down0", Flags: 0},
		}, map[int][]net.Addr{1: ipNet(127, 0, 0, 1, 8), 2: ipNet(127, 0, 0, 1, 8)}, ""},
		// A Docker bridge sits at a low index with a private subnet and no
		// LAN behind it, and an interface that is up without carrier reaches
		// nobody. The real NIC behind both must win, or the share URL names
		// an address no peer routes to.
		{"skips bridges and carrierless interfaces", []net.Interface{
			{Index: 1, Name: "docker0", Flags: up},
			{Index: 2, Name: "eth0", Flags: net.FlagUp},
			{Index: 3, Name: "br-3f2a9c1d", Flags: up},
			{Index: 4, Name: "virbr0", Flags: up},
			{Index: 5, Name: "wlan0", Flags: up},
		}, map[int][]net.Addr{
			1: ipNet(172, 17, 0, 1, 16),
			2: ipNet(10, 0, 0, 5, 8),
			3: ipNet(172, 18, 0, 1, 16),
			4: ipNet(192, 168, 122, 1, 24),
			5: ipNet(192, 168, 1, 10, 24),
		}, "192.168.1.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Interfaces = func() ([]net.Interface, error) { return tc.ifaces, nil }
			InterfaceAddrs = func(iface net.Interface) ([]net.Addr, error) { return tc.addrs[iface.Index], nil }
			first, second := DiscoverLocalLANIP(), DiscoverLocalLANIP()
			if first != second {
				t.Fatalf("non-deterministic discovery: first=%q second=%q", first, second)
			}
			if first != tc.want {
				t.Fatalf("DiscoverLocalLANIP = %q, want %q", first, tc.want)
			}
		})
	}
}

// shareURLServer is a live transport server on loopback, which is all
// the URL renderer needs: an address, a token, and a ticket book.
func shareURLServer(t *testing.T) *transport.Server {
	t.Helper()
	srv, err := transport.New(transport.Config{
		Dispatcher:   transport.NewDispatcher(),
		EventBus:     transport.NewEventBus(8),
		Token:        "share-url-token",
		Certificates: transport.NewCertificateSource(),
	})
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("transport.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv
}

// The share URL follows the certificate, not the configuration: a domain
// with no certificate for it is a name this backend cannot serve over
// HTTPS, and handing the user an https:// URL for it would produce a
// browser error rather than a page.
func TestShareURLFollowsTheCertificate(t *testing.T) {
	srv := shareURLServer(t)
	_, port, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}

	configured := Settings{BindAll: true, CanonicalDomain: "backend.example"}
	withoutCert := FromServerWithLAN(srv, configured, "192.168.1.10")
	if !strings.HasPrefix(withoutCert.URL, "http://192.168.1.10:") {
		t.Fatalf("URL = %q, want the LAN address while no certificate is loaded", withoutCert.URL)
	}
	if !withoutCert.Insecure {
		t.Fatal("a cleartext LAN URL is not flagged insecure")
	}

	srv.Certificates().SetDomain("backend.example", &tls.Certificate{})
	withCert := FromServerWithLAN(srv, configured, "192.168.1.10")
	wantPrefix := "https://backend.example:" + port + "/?" + transport.PageTicketParam + "="
	if !strings.HasPrefix(withCert.URL, wantPrefix) {
		t.Fatalf("URL = %q, want %q...", withCert.URL, wantPrefix)
	}
	if withCert.Insecure {
		t.Fatal("an https URL is flagged insecure")
	}
	// Still a one-time ticket per render, exactly as the other two
	// spellings: a share panel read twice hands out two openable URLs.
	if again := FromServerWithLAN(srv, configured, "192.168.1.10"); again.URL == withCert.URL {
		t.Fatalf("two renders handed out the same URL: %q", withCert.URL)
	}

	// The seconds after a domain is CHANGED are the case a bare "is a
	// certificate loaded" test gets wrong: the settings name the new
	// domain while the listener still holds the old certificate, and an
	// https URL for a name nothing answers on is a browser error rather
	// than a page.
	renamed := configured
	renamed.CanonicalDomain = "other.example"
	midChange := FromServerWithLAN(srv, renamed, "192.168.1.10")
	if !strings.HasPrefix(midChange.URL, "http://192.168.1.10:") {
		t.Fatalf("URL = %q, want the LAN address while the new name has no certificate", midChange.URL)
	}
}

// mintTicket hands out one one-time page ticket from the server's book.
func mintTicket(t *testing.T, srv *transport.Server) string {
	t.Helper()
	ticket, err := srv.MintPageTicket()
	if err != nil {
		t.Fatalf("MintPageTicket: %v", err)
	}
	return ticket
}

// ticketOutstanding presents ticket at the bootstrap exchange and reports
// whether the book still held it. A live ticket buys the manifest; one the
// book evicted is refused with the same unfingerprintable 404 a bad
// credential gets. Spends the ticket either way, which is what single use
// means.
func ticketOutstanding(t *testing.T, srv *transport.Server, ticket string) bool {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s%s?%s=%s",
		srv.Addr(), transport.BootstrapPath, transport.PageTicketParam, ticket))
	if err != nil {
		t.Fatalf("bootstrap exchange: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode != http.StatusNotFound
}

// tailnetJoined is a node up and reachable, which is the state that makes
// the second share URL mint a ticket of its own.
func tailnetJoined() Settings {
	return Settings{
		BindAll:         true,
		ListenPort:      4321,
		CanonicalDomain: "backend.example",
		ACMEDNSHook:     []string{"dnstool", "--zone", "example.com"},
		TailnetEnabled:  true,
		TLS:             TLSStatus{Serving: TLSServingSelfSigned, SelfSignedFingerprint: "sha256:beef"},
		Tailnet: TailnetStatus{
			Running: true,
			State:   "Running",
			AuthURL: "https://login.tailscale.com/a/0123456789abcdef",
			DNSName: "node.example.ts.net",
			IPs:     []string{"100.96.5.42"},
		},
	}
}

// The redacted record mints NOTHING, which is the mechanism and not a
// detail: withholding by building the full record and then clearing it
// would spend the same one-time page tickets out of a book of sixteen, so
// every remote read of the settings screen would evict the share URL the
// owner had just copied at their own.
//
// Observed through that book, because minting leaves its trace nowhere
// else — and paired with the control that proves the observation reads
// something, since "the ticket survived" is also what a broken instrument
// says.
func TestRedactedRecordSpendsNoTicket(t *testing.T) {
	srv := shareURLServer(t)
	srv.MarkReady()
	configured := tailnetJoined()

	// Four books' worth of renders. One mint anywhere in that many passes
	// evicts the ticket minted first.
	const renders = 64

	survivor := mintTicket(t, srv)
	for i := 0; i < renders; i++ {
		out := FromServerRedacted(configured)
		if out.Token != "" || out.URL != "" || out.Tailnet.URL != "" || out.Insecure {
			t.Fatalf("redacted record carries a server-derived field: %+v", out)
		}
	}
	if !ticketOutstanding(t, srv, survivor) {
		t.Fatal("a redacted render evicted an outstanding ticket, so it minted one")
	}

	doomed := mintTicket(t, srv)
	for i := 0; i < renders; i++ {
		_ = FromServerWithLAN(srv, configured, "192.168.1.10")
	}
	if ticketOutstanding(t, srv, doomed) {
		t.Fatal("the full render evicted nothing, so the survival above proves nothing")
	}
}

// What the redaction KEEPS is everything remote administration is for. The
// tailnet's sign-in link is the one worth naming: it is a URL, it is
// single use, and it sits beside two that are withheld — but it is what a
// remote owner opens to APPROVE this machine, so losing it would leave
// them able to enable the feature and unable to finish it.
func TestRedactedRecordKeepsWhatRemoteAdministrationNeeds(t *testing.T) {
	configured := tailnetJoined()
	out := FromServerRedacted(configured)

	want := configured
	want.URL = ""
	want.Token = ""
	want.Insecure = false
	want.Tailnet.URL = ""
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("redacted record\n got %+v\nwant %+v", out, want)
	}
	if out.Tailnet.AuthURL != configured.Tailnet.AuthURL {
		t.Errorf("AuthURL = %q, want the tailscale sign-in link to travel", out.Tailnet.AuthURL)
	}
}

// A nil server is the redacted record exactly, which is what keeps the
// withheld set declared once: FromServerWithLAN starts from it and fills
// in, so a fifth server-derived field cannot be added on one path only.
func TestFromServerWithoutAServerIsTheRedactedRecord(t *testing.T) {
	configured := tailnetJoined()
	if got, want := FromServerWithLAN(nil, configured, "192.168.1.10"), FromServerRedacted(configured); !reflect.DeepEqual(got, want) {
		t.Fatalf("nil-server record\n got %+v\nwant %+v", got, want)
	}
}

func TestPairingURLUsesReachableListenerAndMatchingTrust(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tailnet TailnetStatus
		domain  bool
		prefix  string
		pinned  bool
	}{
		{"loopback", TailnetStatus{}, false, "http://127.0.0.1:", true},
		{"tailnet HTTPS without LAN", TailnetStatus{Running: true, DNSName: "ao.test.ts.net", HTTPS: true}, false, "https://ao.test.ts.net/?", false},
		{"tailnet plaintext never promotes to main TLS", TailnetStatus{Running: true, DNSName: "ao.test.ts.net"}, false, "http://ao.test.ts.net:", false},
		{"offline tailnet falls back", TailnetStatus{DNSName: "ao.test.ts.net", HTTPS: true}, false, "http://127.0.0.1:", true},
		{"canonical domain uses WebPKI", TailnetStatus{}, true, "https://backend.example:", false},
		{"tailnet wins over canonical domain", TailnetStatus{Running: true, DNSName: "ao.test.ts.net", HTTPS: true}, true, "https://ao.test.ts.net/?", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := shareURLServer(t)
			settings := Settings{Tailnet: tc.tailnet, TLS: TLSStatus{SelfSignedFingerprint: "sha256:pin"}}
			if tc.domain {
				settings.CanonicalDomain = "backend.example"
				srv.Certificates().SetDomain(settings.CanonicalDomain, &tls.Certificate{})
			}
			// Sixteen links fit in the book. Minting an unused fallback URL
			// on each call would evict the first link before it was scanned.
			first := ""
			for range 16 {
				link, pin := PairingURL(srv, settings, Reach{})
				if !strings.HasPrefix(link, tc.prefix) || (pin != "") != tc.pinned {
					t.Fatalf("PairingURL = %q, pin %q; want %q, pinned %v", link, pin, tc.prefix, tc.pinned)
				}
				_, ticket, found := strings.Cut(link, "?t=")
				if !found || ticket == "" {
					t.Fatal("missing page ticket")
				}
				if first == "" {
					first = ticket
				}
			}
			if !ticketOutstanding(t, srv, first) {
				t.Fatal("pairing evicted a link by minting unused tickets")
			}
		})
	}
}

// An explicit LAN invitation must not silently select Tailscale, including
// when discovery's ordinary fallback finds only a Tailscale CGNAT address.
func TestPairingURLOnNetwork(t *testing.T) {
	previousInterfaces, previousAddrs := Interfaces, InterfaceAddrs
	t.Cleanup(func() { Interfaces, InterfaceAddrs = previousInterfaces, previousAddrs })
	Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 1, Name: "lan", Flags: net.FlagUp | net.FlagRunning}}, nil
	}
	address := "192.168.1.20"
	InterfaceAddrs = func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(address), Mask: net.CIDRMask(24, 32)}}, nil
	}
	srv := shareURLServer(t)
	s := Settings{BindAll: true, CanonicalDomain: "backend.example", TLS: TLSStatus{SelfSignedFingerprint: "sha256:main"},
		Tailnet: TailnetStatus{Running: true, DNSName: "ao.test.ts.net", HTTPS: true}}
	srv.Certificates().SetDomain(s.CanonicalDomain, &tls.Certificate{})
	for _, choice := range []string{"lan", "tailnet"} {
		t.Run(choice, func(t *testing.T) {
			first := ""
			for range 16 {
				link, pin, err := PairingURLOnNetwork(srv, s, Reach{}, choice)
				if err != nil {
					t.Fatal(err)
				}
				if choice == "lan" && (!strings.HasPrefix(link, "http://192.168.1.20:") || pin != "sha256:main") {
					t.Fatalf("LAN invitation selected wrong address/trust: %q, %q", link, pin)
				}
				if choice == "tailnet" && (!strings.HasPrefix(link, "https://ao.test.ts.net/?") || pin != "") {
					t.Fatalf("tailnet invitation selected wrong address/trust: %q, %q", link, pin)
				}
				_, ticket, _ := strings.Cut(link, "?t=")
				if first == "" {
					first = ticket
				}
			}
			if !ticketOutstanding(t, srv, first) {
				t.Fatal("minted unused fallback tickets")
			}
		})
	}
	for _, tc := range []struct {
		name, choice, address string
		settings              Settings
	}{
		{"disabled LAN", "lan", address, Settings{Tailnet: s.Tailnet}},
		{"offline tailnet", "tailnet", address, Settings{BindAll: true}},
		{"CGNAT is not LAN", "lan", "100.77.1.2", s},
		{"no LAN", "lan", "127.0.0.1", s},
		{"unknown choice", "auto", address, s},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address = tc.address
			if link, pin, err := PairingURLOnNetwork(srv, tc.settings, Reach{}, tc.choice); err == nil || link != "" || pin != "" {
				t.Fatalf("unavailable choice returned %q, %q, %v", link, pin, err)
			}
		})
	}
}
