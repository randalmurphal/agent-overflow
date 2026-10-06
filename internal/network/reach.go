package network

import (
	"net"

	"agent-overflow/internal/netisolate"
)

// Reach is the network a LAN bind uses: where the listener goes and which
// address pairing links, pairing addresses and computer routes name. The
// zero value is the host's network.
//
// A loopback Reach keeps an instance on this machine. A LAN bind stays on
// 127.0.0.1, every published LAN address is 127.0.0.1, and nothing is
// advertised or discovered by multicast. Isolated instances on a shared
// host network use it, so a test run puts nothing on the developer's LAN.
type Reach struct {
	loopback bool
	// discover replaces DiscoverLocalLANIP for a host Reach; tests set it
	// so they never depend on the machine's interfaces.
	discover func() string
}

// LoopbackReach confines an instance to this machine.
func LoopbackReach() Reach { return Reach{loopback: true} }

// IsolatedReach is the Reach for an isolated instance: the host's network
// only inside the test network namespace, whose LAN reaches nothing
// (netisolate), and loopback everywhere else.
func IsolatedReach() Reach {
	if netisolate.Contained() {
		return Reach{}
	}
	return LoopbackReach()
}

// LoopbackOnly reports whether this Reach keeps the instance on this
// machine. Callers skip multicast discovery and advertisement, LAN preview
// listeners and the Windows launcher's LAN relay when it does.
func (r Reach) LoopbackOnly() bool { return r.loopback }

// BindHost returns the listener host for the LAN toggle. A host Reach
// listens on every interface when LAN access is on; a loopback Reach never
// leaves 127.0.0.1.
func (r Reach) BindHost(bindAll bool) string {
	if bindAll && !r.loopback {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// LANIP returns the address to publish for LAN access, or "" when none is
// usable.
func (r Reach) LANIP() string {
	switch {
	case r.loopback:
		return "127.0.0.1"
	case r.discover != nil:
		return r.discover()
	default:
		return DiscoverLocalLANIP()
	}
}

// pairable reports whether ip may be published as this Reach's LAN
// address for pairing.
func (r Reach) pairable(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if r.loopback {
		return ip.IsLoopback()
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
}
