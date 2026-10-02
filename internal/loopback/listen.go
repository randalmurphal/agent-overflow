package loopback

import "net"

// Listen binds an ephemeral port on ::1 for a server whose clients run on
// this machine on the same side of any WSL boundary: provider CLIs, MCP
// clients, hook commands and the backend's own dialers.
//
// IPv6 rather than 127.0.0.1 because WSL's virtioproxy (consomme)
// networking routes IPv4 loopback inside the distro through a Windows-side
// relay. That relay refuses connects past about ten at once and adds
// milliseconds to every round trip; ::1 stays in the Linux kernel. A
// listener a Windows process must reach stays on 127.0.0.1 instead, because
// WSL forwards only IPv4 loopback to Windows.
func Listen() (net.Listener, error) {
	return net.Listen("tcp6", "[::1]:0")
}
