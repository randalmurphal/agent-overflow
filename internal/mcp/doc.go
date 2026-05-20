// Package mcp holds the small set of provider-agnostic types that
// flow between the claudeconfig / codexconfig adapters and the
// mcpprobe handshake. It carries the Status enum the frontend reads,
// the transport names the probe selects on, and a Spec struct each
// adapter can produce so the probe layer never has to know which
// provider the entry came from. No IO, no rendering — those live in
// the adapter packages.
package mcp
