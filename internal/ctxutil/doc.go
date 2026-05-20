// Package ctxutil holds stdlib-only context-aware primitives shared
// across the codebase. Currently the canonical cancellable sleep used
// by every poll loop that needs to honor context cancellation.
package ctxutil
