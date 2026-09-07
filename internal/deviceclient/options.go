package deviceclient

import (
	"context"
	"net"
)

// DialContextFunc selects the network path without changing TLS verification,
// the destination authority, credential scope, or request retry policy.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)
type options struct{ dial DialContextFunc }
type Option func(*options)

// WithDialContext allows an application-owned tailnet node to carry requests.
// The function must also handle ordinary destinations through an OS dialer.
// A custom dialer bypasses environment proxies so a peer request cannot escape
// to an unrelated proxy before its intended network path is selected.
func WithDialContext(dial DialContextFunc) Option { return func(o *options) { o.dial = dial } }
func resolveOptions(values []Option) options {
	var result options
	for _, value := range values {
		value(&result)
	}
	return result
}
