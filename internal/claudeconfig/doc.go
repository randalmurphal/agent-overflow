// Package claudeconfig is the read/write adapter for ~/.claude.json.
//
// Claude Code stores its own MCP server configuration in this file:
//   - top-level `mcpServers` map (user-scope library)
//   - per-workspace `projects.<path>.disabledMcpServers[]` array
//
// AO reads from and writes to these specific fields so the same UI a
// user sees in Claude Code's `/mcp` command is in 1:1 sync with the
// composer popup in AO. Every other top-level key is preserved
// untouched because Claude writes constantly (metrics, session ids).
package claudeconfig
