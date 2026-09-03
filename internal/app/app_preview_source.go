package app

import (
	"errors"
	"net"

	"agent-overflow/internal/network"
	"agent-overflow/internal/transport"
)

// Where a preview is served FROM, on this machine. Two answers, tried in
// this order, and the order is the whole policy: the tailnet name works
// from wherever the owner's devices are, and the LAN address only works
// on this network.
//
// Both are TLS, with no cleartext fallback. The preview cookie is
// `Secure` and a browser will not store one from a cleartext origin that
// is not localhost — so a leg that cannot serve HTTPS cannot serve a
// preview at all, and saying so is a better answer than a link that
// silently fails to hold its cookie.

// tailnetPreviewSource serves previews at the node's MagicDNS name,
// under the certificate tsnet resolves for it.
type tailnetPreviewSource struct{ app *App }

// PreviewHost is the node's name, and only while it is up AND the
// tailnet has HTTPS turned on. Certificate domains are the tailnet admin
// panel's answer, not ours to substitute for: without them tsnet has no
// certificate to present, so there is no preview address here at all and
// the LAN source gets its turn.
func (s tailnetPreviewSource) PreviewHost() string {
	status, ok := s.app.tailnetNodeStatus()
	if !ok || !status.Running() || status.DNSName == "" || len(status.CertDomains) == 0 {
		return ""
	}
	return status.DNSName
}

// ListenPreview opens the node's own HTTPS listener on port.
func (s tailnetPreviewSource) ListenPreview(port int) (net.Listener, error) {
	s.app.tailnet.mu.Lock()
	node := s.app.tailnet.node
	s.app.tailnet.mu.Unlock()
	if node == nil {
		return nil, errors.New("the tailnet node is not running, so there is nothing to serve a preview on")
	}
	return node.ListenTLSOn(port)
}

// previewLANIP is the LAN address previews are served on, or "" when
// this backend is bound to loopback only. Read per bind rather than
// captured, because the setting is a toggle and the address moves with
// the network.
func (a *App) previewLANIP() string {
	if !a.currentSettings().Network.BindAll {
		return ""
	}
	return network.DiscoverLocalLANIP()
}

// previewSources builds the ordered source list for the gateway. Called
// once, when the gateway is built: both sources read their own state on
// every bind, so neither goes stale.
func (a *App) previewSources(srv *transport.Server) []transport.PreviewListenerSource {
	return []transport.PreviewListenerSource{
		tailnetPreviewSource{app: a},
		srv.PreviewLANSource(a.previewLANIP),
	}
}
