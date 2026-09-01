package network

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/transport"
)

// TestBindHost_BranchesOnFlag locks the bind-host mapping so a
// future refactor doesn't accidentally widen the loopback bind to
// 0.0.0.0 (or vice versa).
func TestBindHost_BranchesOnFlag(t *testing.T) {
	if got := BindHost(false); got != "127.0.0.1" {
		t.Fatalf("BindHost(false) = %q, want 127.0.0.1", got)
	}
	if got := BindHost(true); got != "0.0.0.0" {
		t.Fatalf("BindHost(true) = %q, want 0.0.0.0", got)
	}
}

// TestOriginPatterns_LoopbackIsNil documents the InsecureSkipVerify
// case: on loopback, the upgrader sees nil and waives the origin
// check. LAN bind tightens this to an explicit allow-list, so a page
// loaded from some other origin cannot open a socket with a token it
// happened to learn.
func TestOriginPatterns_LoopbackIsNil(t *testing.T) {
	if got := OriginPatterns(false, "", ""); got != nil {
		t.Fatalf("loopback patterns should be nil, got %v", got)
	}
}

// TestOriginPatterns_BindAllIncludesLAN pins the LAN allow-list
// shape: loopback variants plus the discovered LAN IP. Without the
// LAN entry, a browser on the LAN would fail the origin check and
// the toggle would be useless. Without the loopback entries, opening
// the URL on this same machine would also fail.
func TestOriginPatterns_BindAllIncludesLAN(t *testing.T) {
	patterns := OriginPatterns(true, "192.168.1.10", "")
	want := []string{"http://127.0.0.1:*", "http://localhost:*", "http://192.168.1.10:*"}
	if len(patterns) != len(want) {
		t.Fatalf("bind-all patterns = %v, want %v", patterns, want)
	}
	for i, p := range patterns {
		if p != want[i] {
			t.Fatalf("bind-all patterns[%d] = %q, want %q", i, p, want[i])
		}
	}
}

// TestOriginPatterns_BindAllNoLAN proves the LAN-IP-missing case
// still produces a usable allow-list. The user's browser may not be
// reachable in this branch (the URL falls back to loopback upstream)
// but at least the loopback origins still work.
func TestOriginPatterns_BindAllNoLAN(t *testing.T) {
	patterns := OriginPatterns(true, "", "")
	want := []string{"http://127.0.0.1:*", "http://localhost:*"}
	if len(patterns) != len(want) {
		t.Fatalf("bind-all patterns (no LAN) = %v, want %v", patterns, want)
	}
	for i, p := range patterns {
		if p != want[i] {
			t.Fatalf("bind-all patterns[%d] = %q, want %q", i, p, want[i])
		}
	}
}

// A canonical domain names its own origins on either bind. The
// port-bearing spelling is the one that matters when something in front
// terminates TLS: the page's origin is https://<domain> and the request
// reaching this backend is cleartext, so the authority it computes for
// itself would not match.
func TestOriginPatterns_CanonicalDomainOnEitherBind(t *testing.T) {
	loopbackBind := OriginPatterns(false, "", "backend.example")
	want := []string{"https://backend.example", "https://backend.example:*"}
	if len(loopbackBind) != len(want) {
		t.Fatalf("loopback patterns = %v, want %v", loopbackBind, want)
	}
	for i, pattern := range loopbackBind {
		if pattern != want[i] {
			t.Fatalf("loopback patterns[%d] = %q, want %q", i, pattern, want[i])
		}
	}

	lanBind := OriginPatterns(true, "192.168.1.10", "backend.example")
	wantLAN := []string{
		"http://127.0.0.1:*", "http://localhost:*", "http://192.168.1.10:*",
		"https://backend.example", "https://backend.example:*",
	}
	if len(lanBind) != len(wantLAN) {
		t.Fatalf("LAN patterns = %v, want %v", lanBind, wantLAN)
	}
	for i, pattern := range lanBind {
		if pattern != wantLAN[i] {
			t.Fatalf("LAN patterns[%d] = %q, want %q", i, pattern, wantLAN[i])
		}
	}
}

// TestDiscoverLocalLANIP_DeterministicOrder pins the determinism
// guarantee: a multi-homed host returns the same answer across
// runs. We swap the iface enumeration hook with a fake that returns
// interfaces in a non-Index order; discovery must sort them so two
// calls land on the same IP.
func TestDiscoverLocalLANIP_DeterministicOrder(t *testing.T) {
	prevIfaces := Interfaces
	prevAddrs := InterfaceAddrs
	t.Cleanup(func() {
		Interfaces = prevIfaces
		InterfaceAddrs = prevAddrs
	})

	// Two RFC1918 interfaces, presented to discovery in reverse-Index
	// order. Without sorting, the result would be 10.0.0.5 (the first
	// one we saw); with sorting it must be 192.168.1.10 (Index 1).
	addrsByIndex := map[int][]net.Addr{
		1: {&net.IPNet{IP: net.IPv4(192, 168, 1, 10), Mask: net.CIDRMask(24, 32)}},
		2: {&net.IPNet{IP: net.IPv4(10, 0, 0, 5), Mask: net.CIDRMask(8, 32)}},
	}
	Interfaces = func() ([]net.Interface, error) {
		// Reverse order on purpose. A sort by Index ascending must
		// override this so the result is stable.
		return []net.Interface{
			{Index: 2, Name: "eth1", Flags: net.FlagUp},
			{Index: 1, Name: "eth0", Flags: net.FlagUp},
		}, nil
	}
	InterfaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
		return addrsByIndex[iface.Index], nil
	}

	first := DiscoverLocalLANIP()
	second := DiscoverLocalLANIP()
	if first != second {
		t.Fatalf("non-deterministic discovery: first=%q second=%q", first, second)
	}
	if first != "192.168.1.10" {
		t.Fatalf("Index sort not applied: got %q, want lowest-Index iface IP 192.168.1.10", first)
	}
}

// TestDiscoverLocalLANIP_TailscalePreference proves that on a host
// where the ONLY non-loopback IPv4 is a Tailscale CGNAT address,
// the discovery still returns it (rather than empty). Tailscale is
// the user's typical "remote access" path; not surfacing the URL
// would strand a Tailscale-only host with no usable bind-all UX.
func TestDiscoverLocalLANIP_TailscalePreference(t *testing.T) {
	prevIfaces := Interfaces
	prevAddrs := InterfaceAddrs
	t.Cleanup(func() {
		Interfaces = prevIfaces
		InterfaceAddrs = prevAddrs
	})

	Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Index: 1, Name: "tailscale0", Flags: net.FlagUp},
		}, nil
	}
	InterfaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
		// 100.96.5.42 is inside the 100.64.0.0/10 CGNAT range
		// Tailscale uses for its mesh.
		return []net.Addr{
			&net.IPNet{IP: net.IPv4(100, 96, 5, 42), Mask: net.CIDRMask(10, 32)},
		}, nil
	}

	if got := DiscoverLocalLANIP(); got != "100.96.5.42" {
		t.Fatalf("Tailscale CGNAT not picked: got %q, want 100.96.5.42", got)
	}
}

// TestDiscoverLocalLANIP_RFC1918BeatsTailscale verifies the
// preference order. When both an RFC1918 and a Tailscale CGNAT
// address are present, the LAN address wins — a user on a normal
// home network shouldn't see a Tailscale URL when their phone is
// already on the same Wi-Fi.
func TestDiscoverLocalLANIP_RFC1918BeatsTailscale(t *testing.T) {
	prevIfaces := Interfaces
	prevAddrs := InterfaceAddrs
	t.Cleanup(func() {
		Interfaces = prevIfaces
		InterfaceAddrs = prevAddrs
	})

	Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			// Tailscale on the lower index — without a preference
			// scan we'd return it first. With preference, the
			// 192.168 address must win even though it's on a
			// higher-index iface.
			{Index: 1, Name: "tailscale0", Flags: net.FlagUp},
			{Index: 2, Name: "en0", Flags: net.FlagUp},
		}, nil
	}
	InterfaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
		switch iface.Index {
		case 1:
			return []net.Addr{&net.IPNet{IP: net.IPv4(100, 96, 5, 42), Mask: net.CIDRMask(10, 32)}}, nil
		case 2:
			return []net.Addr{&net.IPNet{IP: net.IPv4(192, 168, 1, 10), Mask: net.CIDRMask(24, 32)}}, nil
		}
		return nil, nil
	}

	if got := DiscoverLocalLANIP(); got != "192.168.1.10" {
		t.Fatalf("RFC1918 preference broken: got %q, want 192.168.1.10", got)
	}
}

// TestDiscoverLocalLANIP_SkipsLoopbackAndDown locks the iface
// filter: loopback and down interfaces never contribute to the LAN
// URL.
func TestDiscoverLocalLANIP_SkipsLoopbackAndDown(t *testing.T) {
	prevIfaces := Interfaces
	prevAddrs := InterfaceAddrs
	t.Cleanup(func() {
		Interfaces = prevIfaces
		InterfaceAddrs = prevAddrs
	})

	Interfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
			{Index: 2, Name: "down0", Flags: 0}, // not up
		}, nil
	}
	InterfaceAddrs = func(iface net.Interface) ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)},
		}, nil
	}

	if got := DiscoverLocalLANIP(); got != "" {
		t.Fatalf("loopback/down iface should yield empty result, got %q", got)
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
