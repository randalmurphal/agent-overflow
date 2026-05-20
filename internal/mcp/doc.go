// Package mcp owns the pure projection logic between a stored
// store.MCPServer row and the provider-specific config shapes the
// Claude CLI's --mcp-config flag and the Codex app-server's
// thread/start configOverrides["mcp_servers"] consume. It also owns
// the design-MCP / user-MCP merge precedence and per-provider name
// validation. Nothing here imports SQLite, performs IO, or talks to a
// running session.
package mcp
