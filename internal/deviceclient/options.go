package deviceclient

import (
	"context"
	"net"
	"net/http"
	"time"
)

// DialContextFunc selects the network path without changing TLS verification,
// the destination authority, credential scope, or request retry policy.
type DialContextFunc func(context.Context, string, string) (net.Conn, error)
type options struct {
	dial       DialContextFunc
	probe      time.Duration
	redemption func() (time.Duration, error)
	lifetime   context.Context
	// roundTrip wraps the credential transport (withRoundTrip).
	roundTrip func(http.RoundTripper) http.RoundTripper
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

// WithRedemptionBound replaces pinTimeout for the redemption alone (Redeem,
// Pair). bound is asked when the redemption starts; an error or a
// non-positive answer refuses the redemption before it is sent. The client
// the redemption leaves, and every client Open builds, keeps pinTimeout. Nil
// keeps pinTimeout. Only a test sets it (testutil.StepBudget): its backend
// runs in the test process, where a crowded machine can hold a redemption
// past pinTimeout.
func WithRedemptionBound(bound func() (time.Duration, error)) Option {
	return func(o *options) { o.redemption = bound }
}

// withRoundTrip wraps the credential transport, so this package's tests can
// read the deadline each exchange is sent with.
func withRoundTrip(wrap func(http.RoundTripper) http.RoundTripper) Option {
	return func(o *options) { o.roundTrip = wrap }
}

// WithLifetime is the owner's lifetime. When it ends, a rotation in flight
// stops at once unless a legacy exchange has been sent (Client.rotationWait).
// A client without one finishes every rotation, bounded by the HTTP timeout.
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
