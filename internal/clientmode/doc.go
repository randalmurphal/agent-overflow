// Package clientmode owns the Phase F `agent-overflow --connect <url>`
// remote-client mode: parsing the URL+token, and booting a tiny static
// asset server whose index.html has the bootstrap manifest injected so
// the SPA points its WebSocket client at the remote backend instead of
// a locally-bound transport.
package clientmode
