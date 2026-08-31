package loopback

import "testing"

// TestHostHeader is the rebind-defence decision function, moved here
// from internal/clientmode where it was exercising transport's copy.
// The integration tests on either server cannot reach the empty-Host
// branch — Go's http.Client substitutes the URL's authority for an empty
// Request.Host — but a hand-built raw-TCP client can, so the predicate
// is tested directly.
func TestHostHeader(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", false},
		{"127.0.0.1", true},
		{"127.0.0.1:54321", true},
		{"localhost", true},
		{"localhost:8080", true},
		{"LocalHost", true},
		{"[::1]", true},
		{"[::1]:8080", true},
		{"::1", false}, // unbracketed IPv6 is malformed for an HTTP Host
		{"foreign.test", false},
		{"foreign.test:80", false},
		{"192.168.1.5", false},
		{"127.0.0.1.foreign.test", false}, // a string prefix is not a match
		{"localhost.foreign.test", false},
		{"127.0.0.2", false},         // the rest of 127/8 is refused too
		{"[fe80::1234]:8080", false}, // non-loopback IPv6 with a port
		// A DNS name resolving to loopback is the whole mechanism: the
		// packets arrive locally, and the Host is still refused.
		{"rebound.test", false},
		{"rebound.test:31337", false},
	}
	for _, c := range cases {
		if got := HostHeader(c.host); got != c.want {
			t.Errorf("HostHeader(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// TestPeerAddress walks the forms RemoteAddr takes, moved here from
// internal/transport (which had the netip implementation) and
// internal/provider/claudetui (which had one of the two net.ParseIP
// copies). The cases are the union of both suites.
func TestPeerAddress(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:54321", true},
		{"127.0.0.1:0", true},
		{"[::1]:54321", true},
		{"[::1]:8080", true},
		// The rest of 127/8 counts here, unlike HostHeader: this is a
		// real address the kernel reported, not a spelling somebody chose.
		{"127.0.0.5:1", true},
		// IPv4-mapped IPv6 loopback — netip's IsLoopback recognises it.
		{"[::ffff:127.0.0.1]:1234", true},
		{"10.0.0.1:80", false},
		{"10.0.0.5:54321", false},
		{"192.168.1.5:8080", false},
		{"192.168.1.10:443", false},
		{"8.8.8.8:53", false},
		{"[fe80::1234]:1234", false},
		{"", false},
		// Malformed fails closed, so a synthetic request cannot reach a
		// privileged method by being unparseable.
		{"not an address", false},
		// A name is not an address. The kernel never reports one, and
		// accepting it would make a Host-shaped string pass a check that
		// is supposed to be about the peer.
		{"localhost:1234", false},
	}
	for _, c := range cases {
		if got := PeerAddress(c.addr); got != c.want {
			t.Errorf("PeerAddress(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestEndpointHostname(t *testing.T) {
	cases := []struct {
		hostname string
		want     bool
	}{
		{"localhost", true},
		{"LOCALHOST", true},
		{"127.0.0.1", true},
		{"127.0.0.5", true},
		{"::1", true}, // url.URL.Hostname() delivers IPv6 unbracketed
		{"::ffff:127.0.0.1", true},
		{"", false},
		{"192.168.1.5", false},
		{"fe80::1234", false},
		{"foreign.test", false},
		{"127.0.0.1.foreign.test", false},
		// Brackets never survive url.URL.Hostname(), and this predicate
		// takes its input from there.
		{"[::1]", false},
	}
	for _, c := range cases {
		if got := EndpointHostname(c.hostname); got != c.want {
			t.Errorf("EndpointHostname(%q) = %v, want %v", c.hostname, got, c.want)
		}
	}
}

func TestEndpointAuthority(t *testing.T) {
	cases := []struct {
		authority string
		want      bool
	}{
		{"localhost", true},
		{"localhost:8080", true},
		{"127.0.0.1", true},
		{"127.0.0.1:8080", true},
		{"127.0.0.5:1", true},
		{"[::1]", true},
		{"[::1]:8080", true},
		{"", false},
		{"192.168.1.5:8080", false},
		{"foreign.test:443", false},
		{"[fe80::1234]:8080", false},
	}
	for _, c := range cases {
		if got := EndpointAuthority(c.authority); got != c.want {
			t.Errorf("EndpointAuthority(%q) = %v, want %v", c.authority, got, c.want)
		}
	}
}

// TestTheTwoEndpointPredicatesDivergeWhereDocumented pins the reason
// EndpointAuthority is a separate function rather than a wrapper anyone
// could inline. A bare unbracketed "::1" is a valid Hostname() result
// and a malformed authority, so the two must disagree about it — and if
// they ever stop disagreeing, one of the two callers has silently
// changed meaning.
func TestTheTwoEndpointPredicatesDivergeWhereDocumented(t *testing.T) {
	if !EndpointHostname("::1") {
		t.Error("EndpointHostname(\"::1\") = false; url.URL.Hostname() delivers IPv6 unbracketed, so this has to be true")
	}
	if EndpointAuthority("::1") {
		t.Error("EndpointAuthority(\"::1\") = true; an authority spells IPv6 bracketed, so a bare one is malformed")
	}
}

// TestHostHeaderIsStrictlyNarrowerThanEndpointAuthority holds the
// relationship the package doc claims. Both take an authority, so they
// are directly comparable: HostHeader is a refusal policy over a set
// EndpointAuthority classifies, and every Host it admits must be
// loopback by the wider reading too. A spelling that passed the policy
// without being loopback at all would be the worst possible bug here.
func TestHostHeaderIsStrictlyNarrowerThanEndpointAuthority(t *testing.T) {
	for _, authority := range []string{"127.0.0.1", "localhost", "[::1]", "127.0.0.1:8080", "[::1]:8080"} {
		if !HostHeader(authority) {
			t.Errorf("HostHeader(%q) = false; it is an accepted spelling", authority)
		}
		if !EndpointAuthority(authority) {
			t.Errorf("EndpointAuthority(%q) = false; anything HostHeader admits must classify as loopback", authority)
		}
	}
	// Loopback by classification, refused by the policy.
	for _, authority := range []string{"127.0.0.2", "127.0.0.5:1", "[::ffff:127.0.0.1]"} {
		if !EndpointAuthority(authority) {
			t.Errorf("EndpointAuthority(%q) = false; it is a loopback literal", authority)
		}
		if HostHeader(authority) {
			t.Errorf("HostHeader(%q) = true; the policy admits three spellings and this is not one", authority)
		}
	}
}
