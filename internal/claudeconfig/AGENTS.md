# internal/claudeconfig/

Read/write adapter for the slice of Claude Code's on-disk configuration
AO shares with the CLI. **Nothing in this package spawns a process** —
that is the point: every answer here is what a session *would* load,
read straight from disk.

## Surfaces

| File | Surface |
|---|---|
| `store.go` | `Store` (root: the Claude home, default `~/.claude`) + `~/.claude.json` load/save. Every unknown top-level key round-trips untouched — Claude writes constantly (metrics, session ids). |
| `mcpjson.go` | MCP server membership: user scope (`mcpServers`), project scope (`projects.<key>.mcpServers` + `disabledMcpServers`), workspace `.mcp.json` ancestry gated by `disabledMcpjsonServers`. |
| `plugins.go` | Plugin enablement (`enabledPlugins` across merged settings files), installation records (`installed_plugins.json`), per-plugin `.mcp.json` / `plugin.json` server names. |
| `projectkey.go` | `ProjectKey(workspacePath)` — the canonical git root, so every worktree of a repo shares the main checkout's entry, exactly how the CLI keys it. |
| `identity.go` | `oauthAccount` clearing (the config half of an account switch). AO never writes it — the CLI re-derives it. |
| `identity_read.go` | `ReadOAuthAccount` — the one sanctioned READ of `oauthAccount` (org uuid + name), used at account-adoption time only and accepted only when its email matches the probe answer. Never live state: absence is a non-answer. |
| `skills.go` | `ListSkills(workspacePath)` — filesystem enumeration of the skills a session would load: user tier (`<home>/skills`), project tier (`<workspace>/.claude/skills`, wins name collisions), enabled plugins' `skills/` dirs (namespaced `<plugin>:<skill>`). |
| `orderedjson.go` | Key-order-preserving JSON object, so saves don't churn diffs of a file another program owns. |

## Why skills are read here at all

The zero-token account probe runs `--safe-mode`, whose `initialize`
response deliberately reports no skills — so a cold thread's composer
menu would show none until a session's `system/init` frame arrives.
`ListSkills` fills exactly that window. Once a live frame exists its
name set is authoritative; the frontend merge
(`mergeStaticClaudeCommands`) only enriches, never overrides it.

Per-skill parse problems (frontmatter fence that never closes,
unparseable YAML) skip that skill — mirroring the CLI not loading it —
while a SKILL.md with no frontmatter at all is still a skill named
after its directory. Missing directories are empty answers; any other
filesystem error is surfaced, not swallowed.

## Secrets rule

Only server NAMES from the plugin/project MCP sources ever leave this
package's rows — command/args/env can hold live tokens and must not be
rendered or persisted anywhere downstream.

## Anti-patterns

- Do NOT spawn the CLI to answer a config question. If disk can't
  answer it, the caller needs a session, not this package.
- Do NOT drop or reorder unknown keys in `~/.claude.json` on save.
- Do NOT leak plugin/project MCP `command`/`args`/`env` values out of
  this package.
- Do NOT key project state by the literal workspace path — use
  `ProjectKey`, or worktrees stop sharing their repo's entry.
