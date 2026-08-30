# internal/codexmodels/

TTL'd, single-flighted cache around Codex `model/list`. The catalog
fetch spawns a Codex CLI subprocess. Without coalescing, every settings
render and every model picker open would fire one. The Cache keys by
binary path, TTLs successful results for `DefaultTTL` (5 min), and
collapses concurrent calls against the same binary into one CLI run. `Peek`
is the nonblocking read for paint paths: it returns only a fresh completed
entry and never starts or joins a subprocess.

The App owns a single Cache instance (lazy-init through
`(*App).codexModels()` so tests that build an `&App{...}` directly stay
NPE-safe) and reaches into it from `GetModelsForProvider("codex")` and
`refreshCodexModelCatalog`.

## Layout

- `codexmodels.go`: `Cache` type, `New` / `NewWith` constructors,
  `Get` / `Peek` / `Reset` methods, `Lister` test seam, and the
  shared `provider.CloneModels` defensive-copy helper. `DefaultTTL` and
  `DefaultErrorTTL` are the exported cache lifetimes.

## Responsibility boundary

- What BELONGS here:
  - The TTL + single-flight bookkeeping (entries + inflight maps under
    one mutex).
  - Defensive cloning so callers can't mutate the cached slice.
  - The default `Lister` wiring to `codex.ListModels`.
- What does NOT belong here:
  - The provider-list of supported models (lives in
    `internal/provider/codex/models.go`).
  - Reacting to settings changes. The App calls `Reset()` after a
    Codex binary path patch lands.
  - `*App` state or wire-format types.

## Anti-patterns

- Do NOT cache failed lookups for the full success TTL. A transient CLI
  failure (binary missing during a settings edit, permissions) is retained
  only for `DefaultErrorTTL`, so paired capability reads share the failure
  without masking recovery for five minutes.
- Do NOT add a per-key TTL knob to `Get`. The cache's coherence
  assumption is "one TTL for everything"; introducing per-call
  overrides invites drift in which call sites get fresh data and
  which don't.
- Do NOT skip the defensive clone in `Get`. Callers consume the
  result, may stash it inside structs they mutate, and a shared
  slice would silently corrupt subsequent reads.
