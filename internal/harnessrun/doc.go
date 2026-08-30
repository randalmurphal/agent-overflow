// Package harnessrun owns the durable lifecycle of a harness workload.
//
// A run is deliberately separate from the command which drives it. The
// supervisor creates an atomic manifest before the caller mutates a data root,
// holds an exclusive lease for that root, and records every terminal outcome.
// A failed fresh root is quarantined. An explicitly borrowed root is never
// removed or quarantined by this package.
package harnessrun
