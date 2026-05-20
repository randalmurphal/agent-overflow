// Package mcpprobe runs the MCP `initialize` JSON-RPC against a
// configured server and classifies the response as Ready, NeedsAuth,
// or Failed. It owns no protocol parsing beyond what's needed to map
// a server's reachability to one of those states — full MCP semantics
// stay inside the provider subprocesses.
//
// A process-global TTL cache fronts the probes so the composer popup
// can render status synchronously after the first slow handshake.
// Cache invalidation is explicit (the App calls Invalidate on every
// library edit or successful OAuth completion) — TTL is the
// background safety net, not the freshness contract.
package mcpprobe
