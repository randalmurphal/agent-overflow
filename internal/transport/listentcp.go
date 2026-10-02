package transport

import (
	"fmt"
	"net"

	"agent-overflow/internal/platform"
)

// explicitPortAttempts bounds how often ListenTCP re-probes when another
// process takes the probed port between the probe and the explicit bind.
const explicitPortAttempts = 4

// ListenTCP binds a listener that a Windows process may reach through WSL's
// localhost forwarding. Inside WSL a port-0 request binds an explicit port
// the kernel has just reported free instead: WSL 2.7.x under virtioproxy
// forwards a listener to Windows only when it tracks the bind, and it misses
// port-0 binds made from a thread other than the process leader, which a Go
// goroutine usually is (microsoft/WSL#41039, fixed in WSL 2.9.9). Elsewhere,
// and for an explicit port, it is net.Listen.
func ListenTCP(network, addr string) (net.Listener, error) {
	return listenTCP(network, addr, platform.IsWSL(), net.Listen)
}

func listenTCP(network, addr string, wsl bool, listen func(network, addr string) (net.Listener, error)) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if !wsl || err != nil || port != "0" {
		return listen(network, addr)
	}
	for range explicitPortAttempts {
		probe, err := listen(network, addr)
		if err != nil {
			return nil, err
		}
		_, chosen, err := net.SplitHostPort(probe.Addr().String())
		if closeErr := probe.Close(); closeErr != nil {
			return nil, fmt.Errorf("release probed port %s: %w", probe.Addr(), closeErr)
		}
		if err != nil {
			return nil, fmt.Errorf("read probed port %s: %w", probe.Addr(), err)
		}
		ln, err := listen(network, net.JoinHostPort(host, chosen))
		if err == nil || !addrInUse(err) {
			return ln, err
		}
	}
	return nil, fmt.Errorf("bind %s: %d probed ports were taken before they could be bound", addr, explicitPortAttempts)
}
