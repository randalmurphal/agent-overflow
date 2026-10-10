// Package forgeapi owns the forge API transport described in
// docs/architecture/forge-transport.md.
package forgeapi

import "context"

type interactiveKey struct{}

// WithInteractive marks ctx as carrying a user's own action (opening a PR
// pane, Refresh, Save, Send, Submit, Reply, Resolve) as opposed to a
// poller's. Such a
// request may spend the forge quota reserve that background polling
// leaves untouched.
func WithInteractive(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveKey{}, true)
}

// IsInteractive reports whether ctx, or a context it derives from, was
// marked by WithInteractive.
func IsInteractive(ctx context.Context) bool {
	marked, _ := ctx.Value(interactiveKey{}).(bool)
	return marked
}
