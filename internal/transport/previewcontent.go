package transport

import (
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
)

// NewContentPreview serves a caller-owned, confined file handler at a separate
// origin, with the same tickets, browser grants and revocation as dev previews.
// Each instance has its own grant book: recycling a port for another directory
// must not make an old directory's cookie authorize the new one.
//
// local is for an on-host caller only. It replaces all configured sources with
// a literal loopback bind and never exposes cleartext to a network interface.
// Browsers need no certificate installation to view their own computer's files.
// Remote callers use the configured TLS sources, exactly like dev previews.
func NewContentPreview(cfg PreviewGatewayConfig, content http.Handler, local bool) (*PreviewGateway, int, error) {
	if content == nil {
		return nil, 0, errors.New("a file preview needs a content handler")
	}
	scheme := "https"
	if local {
		cfg.Sources = []PreviewListenerSource{contentLoopbackSource{}}
		scheme = "http"
	}
	gateway := newPreviewGateway(cfg, content, scheme)
	// Netstack sources need a concrete port. Bounded collision retries avoid
	// assuming they implement the host kernel's special port-zero allocation.
	// The port is an address, not a secret; the ticket book owns authentication.
	for range 8 {
		port := 49152 + rand.IntN(16384)
		gateway.bind(PreviewTarget{Port: port, Scheme: "http"})
		gateway.mu.Lock()
		served := gateway.ports[port] != nil
		gateway.mu.Unlock()
		if served {
			return gateway, port, nil
		}
	}
	gateway.Close()
	return nil, 0, errors.New("no preview address is available; check this computer's access and sharing settings")
}

type contentLoopbackSource struct{}

func (contentLoopbackSource) PreviewHost() string { return "127.0.0.1" }

func (contentLoopbackSource) ListenPreview(port int) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}
