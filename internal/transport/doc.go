// Package transport implements the HTTP+WebSocket wire protocol that
// connects the Svelte frontend to the Go backend. The same protocol
// works for the Wails-embedded webview pointing at 127.0.0.1, for
// browsers on the LAN, and for desktop clients connecting to a
// remote-hosted backend.
//
// The wire is intentionally small: a JSON-RPC-shaped request/response
// pair plus a server-initiated event push frame. There's no batching,
// no negotiation, no version handshake — clients send requests and
// replay markers, the server sends responses and events.
//
// All wire messages are JSON text frames. Binary frames are rejected.
package transport
