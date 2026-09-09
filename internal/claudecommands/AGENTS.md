# Claude command catalog

This package stores the slash commands reported by Claude's zero-token account
probe. It never starts a provider process.

Each successful probe replaces the entry scoped by `provider.ProbeCacheKey`.
Do not merge or backfill commands. Store wire names without a leading slash and
preserve descriptions verbatim; consumers add presentation syntax.

`Store(key, nil, err)` keeps the previous entry, `Store(key, nil, nil)`
records a known empty list, and a nil `CommandsFor` result means unknown.
`initialize` may omit MCP prompt commands found in
`system/init.slash_commands`; combine observations only where both exist.
