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

// containedInterfaces reports whether ifaces are the namespace Command
// builds: loopback plus lanName carrying lanAddress and nothing routable
// besides. Linux brings a dummy interface up with an IPv6 link-local
// address, which reaches nothing beyond it either.
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
		for _, addr := range list {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || !(ipNet.IP.Equal(lanAddress) || (ipNet.IP.To4() == nil && ipNet.IP.IsLinkLocalUnicast())) {
				return false
			}
		}
		lan = true
	}
	return lan
}
