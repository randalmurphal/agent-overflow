// Package tailnet joins this backend to the owner's tailnet as its own
// node, using tailscale.com/tsnet in userspace (docs/specs/remote-access.md
// §7, "Anywhere access"). Off-network reach IS the tailnet: there is no
// public listener and no tunnel, so every path a request can take is one
// the owner enrolled.
//
// The package owns the node's LIFECYCLE and its published STATUS, and
// nothing else. It never serves a request: the listeners it hands back go
// to internal/transport, which answers them with the same mux, the same
// credentials and the same per-call gate its main bind uses. It knows
// nothing about settings either — internal/app reconciles the user's
// preference onto it.
package tailnet
