// Package claudeconfig is the read/write adapter for ~/.claude.json.
//
// Two of Claude Code's own fields are shared with AO:
//
//   - MCP server configuration — the top-level `mcpServers` map
//     (user-scope library) and the per-workspace
//     `projects.<path>.disabledMcpServers[]` array. AO reads and writes
//     these so the same list a user sees in Claude Code's `/mcp`
//     command is in 1:1 sync with the composer popup in AO.
//   - `oauthAccount` — the cached identity of the logged-in account.
//     AO only ever CLEARS it (see identity.go); Claude Code remains the
//     sole writer, re-deriving it from whichever credential is
//     installed. Clearing it is the half of an account switch that does
//     not live in the credential file.
//
// Every other top-level key is preserved untouched because Claude
// writes constantly (metrics, session ids).
package claudeconfig
