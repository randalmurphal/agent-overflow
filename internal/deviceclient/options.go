package deviceclient

import (
	"context"
	"net"
	"time"
)

// DialContextFunc selects the network path without changing TLS verification,
// the destination authority, credential scope, or request retry policy.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)
type options struct {
	dial     DialContextFunc
	probe    time.Duration
	lifetime context.Context
}
type Option func(*options)

// WithDialContext allows an application-owned tailnet node to carry requests.
// The function must also handle ordinary destinations through an OS dialer.
// A custom dialer bypasses environment proxies so a peer request cannot escape
// to an unrelated proxy before its intended network path is selected.
func WithDialContext(dial DialContextFunc) Option { return func(o *options) { o.dial = dial } }

// WithProbeInterval sets how often AwaitActivation asks whether the owner
// confirmed the pairing. Zero keeps probeInterval. Only an isolated test boot
// shortens it: every probe is a request to the backend's credential routes.
func WithProbeInterval(interval time.Duration) Option {
	return func(o *options) { o.probe = interval }
}

// WithLifetime is the owner's lifetime. When it ends, a recoverable renewal
// exchange in flight stops at once (Client.exchangeContext). A client without
// one finishes every exchange it sent, bounded by the HTTP timeout.
func WithLifetime(lifetime context.Context) Option {
	return func(o *options) { o.lifetime = lifetime }
}
func resolveOptions(values []Option) options {
	var result options
	for _, value := range values {
		value(&result)
	}
	return result
}
