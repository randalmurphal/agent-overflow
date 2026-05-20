// Package codexconfig is the read/write adapter for ~/.codex/config.toml.
//
// Codex stores its own MCP server configuration under
// `[mcp_servers.<name>]` table headers with first-class `enabled =
// false` opt-out. AO reads from and writes to those specific sections
// so the same servers a user configures via `codex mcp add` are in
// 1:1 sync with the composer popup in AO. Every other top-level
// section ([features], [tui], [projects.*], [notice], etc.) is
// preserved by byte-splicing — only the bytes inside the affected
// `[mcp_servers.<name>]` block are rewritten on update.
package codexconfig
