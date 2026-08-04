// Package claudeconfig is the read/write adapter for the slice of
// Claude Code's on-disk configuration that AO shares with the CLI.
//
// Writable surface (~/.claude.json):
//
//   - MCP server configuration — the top-level `mcpServers` map
//     (user scope), the per-project `projects.<key>.mcpServers` map
//     (local scope, `claude mcp add --scope local`), and the
//     per-project `projects.<key>.disabledMcpServers[]` array. The
//     project key is ProjectKey(workspacePath): the canonical git
//     root, so every worktree of a repo reads and writes the main
//     checkout's entry — exactly how the CLI keys it.
//   - `oauthAccount` — the cached identity of the logged-in account.
//     AO only ever CLEARS it (see identity.go); Claude Code remains the
//     sole writer, re-deriving it from whichever credential is
//     installed. Clearing it is the half of an account switch that does
//     not live in the credential file.
//
// Read-only surface (so ListServers can enumerate the full membership
// a session would load, without spawning anything):
//
//   - Plugin servers — `enabledPlugins` from the merged settings files
//     (user, project, local), installation records from
//     ~/.claude/plugins/installed_plugins.json, and server names from
//     each plugin's .mcp.json / plugin.json manifest.
//   - Project-scope servers — `.mcp.json` files from the workspace and
//     its ancestor directories, gated by `disabledMcpjsonServers` (the
//     non-interactive sessions AO spawns auto-approve everything not
//     explicitly rejected).
//
// Only server NAMES from those sources ever leave this package's
// plugin/project rows — command/args/env can hold live tokens and
// must not be rendered or persisted.
//
// Every other top-level key in ~/.claude.json is preserved untouched
// because Claude writes constantly (metrics, session ids).
package claudeconfig
