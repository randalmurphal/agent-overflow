# internal/codexskills/

The caller-facing shape of a Codex skill, and a TTL'd, single-flighted
cache in front of the `skills/list` read that produces it.

Skills are Codex's user-invokable prompt units and the **replacement for
custom prompts**, which upstream removed in 0.118 — there is no legacy
method to fall back to. The read is never free: it either rides a live
session's connection or costs a whole short-lived `codex app-server`
process, and either way it re-scans the filesystem for every requested
directory. Without coalescing, every composer-menu render would pay that.

## Layout

- `codexskills.go` — the `Skill` / `CwdSkills` / `LoadError` types, the
  `Key` constructor, the `Fetch` callback type, and the `Cache`
  (`New` / `NewWith`, `Get` / `Refresh` / `Invalidate` / `Reset`).
  `DefaultTTL` (5 min), `DefaultErrorTTL` (30 s) and the `Scope*` wire
  values are the only other exported names.

## Responsibility boundary

- What BELONGS here:
  - The caller-facing skill shape. `internal/provider/codex` imports this
    package and projects onto it, the same way it projects onto
    `internal/mcpstatus.ServerStatus` — which is what keeps the raw wire
    types inside the provider package without inventing a second
    near-identical struct.
  - TTL + single-flight bookkeeping and defensive cloning.
  - The cache key shape (`Key`).
- What does NOT belong here:
  - Deciding HOW to read. The App supplies a `Fetch` closure because only
    it knows whether a live session's connection can be ridden
    (`app_codex_skills.go#readCodexSkills`).
  - The wire shape and its parsing — `internal/provider/codex/skills.go`.
  - Importing `internal/provider/codex`. That direction is what would make
    the import cycle; the App is the seam.

## Three rules that are not stylistic

- **The key carries the cwd, not just the binary.** Skills are
  directory-scoped: the `repo` tier comes from the workspace itself, so
  two workspaces genuinely have different answers and a cwd-less key would
  serve one project's skills to another. The binary is the other dimension
  for the same reason `internal/codexmodels` keys on it — a different
  codex build has a different bundled set.
- **The active account is deliberately NOT a dimension.** This is the
  difference from `internal/codexusage`. Skills resolve from the canonical
  `CODEX_HOME` (every AO spawn unsets the override) plus the cwd's repo;
  switching logins replaces `auth.json` and nothing a skill scan reads. If
  AO ever points skill reads at a per-account home, `Key` has to grow with
  it in the same commit.
- **Errors are cached, and shared.** A failed lookup populates an entry for
  `DefaultErrorTTL` and every concurrent caller receives the same error.
  "This workspace has no skills" is a legitimate successful answer here,
  which is exactly why a failure must never be allowed to look like one.

## Anti-patterns

- Do NOT narrow a `skills/changed` invalidation to a key. The notification
  is an empty struct — no cwd, no scope, no skill name — so there is
  nothing to narrow to, and a skill file that moved between two watched
  roots would leave a stale entry behind under the key it left. `Reset` is
  the only correct response.
- Do NOT drop the generation counter from the load path. It is what keeps
  a `Reset` that lands mid-read from being undone by the read it raced; a
  read that started before the change cannot be known to have observed it.
- Do NOT skip the `Clone` in `Get`. Callers stash the slices in structs
  they mutate, and a shared backing array would silently corrupt every
  later read.
