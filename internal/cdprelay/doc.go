// Package cdprelay is the WSL backend's half of the embedded browser
// pane's CDP relay.
//
// The Windows launcher DIALS this backend (internal/webview2host's
// CDPTunnel) and multiplexes TCP streams to the pane environment's
// loopback debugging port. This package is the other end of that wire: it
// speaks the same cdpframe protocol, and exposes the far side as an
// ordinary loopback listener inside the distro so chromedp can attach to
// it with nothing but a URL.
//
// Nothing here dials outward and nothing listens off loopback.
package cdprelay
