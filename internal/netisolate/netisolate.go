// Package netisolate runs a command inside new user and network namespaces
// that hold only loopback and a private LAN interface (LANName at LANAddress)
// with no route off it. Test suites run there so a test that binds every
// interface, dials out or probes the LAN reaches nothing beyond the
// namespace, whatever the host's networking mode.
//
// An executable that isolates commands must dispatch HelperArg before
// anything else in main (and in TestMain for tests):
//
//	if len(os.Args) > 1 && os.Args[1] == netisolate.HelperArg {
//		os.Exit(netisolate.RunHelper(os.Args[2:], os.Stderr))
//	}
package netisolate

import "net"

// HelperArg is argv[1] when an executable re-runs itself as the first
// process inside the namespaces.
const HelperArg = "__ao-netisolate-helper"

// EnvNamespace names the environment variable the helper sets to its network
// namespace's identity (the /proc/self/ns/net link) before it execs the
// target, so the target and its descendants can prove they run in the
// namespace Command built rather than on a host that merely looks like one.
const EnvNamespace = "AO_NETISOLATE_NS"

// containedInterfaces reports whether ifaces are the namespace Command
// builds: loopback plus lanName carrying lanAddress and nothing routable
// besides. lanName without lanAddress is not the namespace's interface.
// Linux brings a dummy interface up with an IPv6 link-local address, which
// reaches nothing beyond it either.
func containedInterfaces(ifaces []net.Interface, addrs func(net.Interface) ([]net.Addr, error), lanName string, lanAddress net.IP) bool {
	lan := false
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Name != lanName {
			return false
		}
		list, err := addrs(iface)
		if err != nil {
			return false
		}
		addressed := false
		for _, addr := range list {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				return false
			}
			switch {
			case ipNet.IP.Equal(lanAddress):
				addressed = true
			case ipNet.IP.To4() == nil && ipNet.IP.IsLinkLocalUnicast():
			default:
				return false
			}
		}
		if !addressed {
			return false
		}
		lan = true
	}
	return lan
}
