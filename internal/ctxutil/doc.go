// Package ctxutil holds stdlib-only context-aware primitives shared
// across the codebase: the cancellable sleep every poll loop uses, and
// Detach for work that outlives its caller but not its owner.
package ctxutil
