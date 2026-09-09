# Claude configuration

This package reads and writes the Claude configuration a session would load
from disk. It never starts the CLI. Callers inject the Claude home and workspace.

Preserve unknown top-level keys and key order in `~/.claude.json`. Resolve
project state with `ProjectKey` so worktrees share the CLI's project entry.
Only MCP server names may leave configuration rows; commands, arguments, and
environment values can contain credentials.

`ReadOAuthAccount` is an adoption-time read. Accept organization data only
when its email matches the probe identity. AO may clear `oauthAccount` during
account switching but never writes a replacement identity.

`ListSkills` merges user, project, and enabled-plugin roots. Project skills
win name collisions; plugin skills remain namespaced. Missing roots are empty,
malformed skill frontmatter skips that skill, and other errors are returned.
