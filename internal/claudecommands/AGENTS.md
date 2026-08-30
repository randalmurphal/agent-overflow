# internal/claudecommands/

Holds the list of provider-executed slash commands the Claude CLI reports,
keyed by probe identity.

The rows arrive free: the zero-token account probe's `initialize`
control_response carries a `commands` array alongside `models` and `account`,
and `claude.ProbeConfig.OnCommands` hands it here. **Nothing in this package
spawns a process.**

## Why there is no merge policy

`internal/claudemodels` merges because AO ships a hand-maintained model catalog
the wire enriches. There is no AO command catalog and there cannot be one. The
list is whatever the running binary, that login, that working directory, and
those plugins produce (52 entries on a real 2.1.219 install: built-ins, skills,
user and project commands, plugin commands). So the wire list is the whole
answer and every probe **replaces** the entry wholesale.

That is also the wire's own contract on the other two surfaces:
`system/commands_changed` explicitly means "replace your cached list", and
`system/init.slash_commands` restates the whole set per session.

## What the wire is (and is not)

- `name` carries **no leading slash** (the CLI's zod: "Skill name (without the
  leading slash)"). Every consumer adds the `/` itself.
- `description` carries provenance suffixes the CLI renders in its own picker
  ("… (user)", "… (project)"). Passed through as data, because parsing
  provenance out of prose would be a guess.
- The `initialize` list is the RICH one but is **not complete**: MCP prompt
  commands (`mcp__server__prompt`) appear only on `system/init.slash_commands`,
  which is names-only. Neither surface subsumes the other, which is why triage
  emits what each wire frame actually carried and the overlay happens at the
  edge that has both.

## Identity

`Cache` is keyed by `provider.ProbeCacheKey`, the same key the account probe
memoizes under. Project-scoped commands live in the workdir and plugin commands
under the credentialed home, so one identity's list is not another's.

It deliberately does NOT share the probe cache's TTL or its invalidations: a
command list has no correctness deadline, and dropping it while identity is
rechecked would empty an open menu for the seconds a re-probe takes. Every
probe replaces the entry; the map is capped instead.

## Absence rules

Three different "nothing" values, and they are not interchangeable:

| Value | Means | Consumer must |
|---|---|---|
| `Store(key, nil, err)` | The array was unreadable. | Keep the previous entry. `Store` no-ops. |
| `Store(key, nil, nil)` | The binary reports no commands. | Replace with empty. |
| `CommandsFor(key) == nil` | No probe has reported for this identity. | Say "unknown", never "none". |

## Anti-patterns

- Do NOT merge, enrich, or backfill. A command that fell off the list is gone.
- Do NOT spawn anything from here. If a caller wants a fresher list, it probes.
- Do NOT strip or add the leading slash in storage. Store what the wire said.
