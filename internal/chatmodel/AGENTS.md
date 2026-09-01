# internal/chatmodel/

Pure provider+store-only helpers for chat-thread model profiles. The
package owns the fallback/seed/sanitize/sameness logic plus capability
detection and the context-window query/validation cluster. Every
function here is callable without a running App or SQLite handle.

The App-coupled wrappers that need persistence (`rememberChatModelProfile`,
`seedChatModelProfile`, plus the context-settings and thread-model bindings)
stay in `app_*.go` and compose this package's pieces with their store
reads/writes.

Every helper lives in `chatmodel.go` with a doc comment stating its
contract; read those before adding a variant.

## Responsibility boundary

- What BELONGS here:
  - Anything that can be expressed as a pure function over
    `(provider.*, store.ChatModelProfile, store.Thread)` arguments.
  - Sanitization that needs to agree across every callsite:
    fast-mode clamping, context-window clamping, runtime-mode
    normalization for sameness checks.
- What does NOT belong here:
  - SQLite reads or writes. `seedChatModelProfile` and
    `rememberChatModelProfile` stay in `app_chat_bar.go` because
    they coordinate the store dance around these pure pieces.
  - Frontend-facing wire types. The `ContextSettingsProfile`
    wrapper that the binding returns lives in
    `app_context_settings.go`.
  - Codex's live model-catalog cache belongs to its own package
    (`internal/codexmodels`). This package only consults the
    static `provider.*` registry.

## Anti-patterns

- Do NOT bypass `SanitizeProfile` when projecting a stored row into
  the UI. Older rows can carry context windows or fast-mode flags
  that the current registry has retired; the sanitizer is the single
  point where those get clamped.
- Do NOT replicate the Codex-permissive branch of
  `SupportsStoredFastMode` elsewhere. The live Codex catalog can
  advertise models the static registry doesn't know about; a stricter
  check would silently drop a remembered favorite when the live
  catalog hasn't loaded yet.
- Do NOT add a per-call `forceSanitize bool` knob to the profile
  helpers. Either a value is always sanitized at this boundary (the
  cache rule) or it never is (the wire rule). Mixed modes invite
  drift between callsites.
- Do NOT gate a user-facing surface on this package's static-only
  context-window lookups. A wire-only model (a slug the CLI ships
  before `provider.*` lists it) exists only in the App's merged
  catalogs, so a static lookup reports it unknown and a static
  default guesses the provider-wide standard window — a value that IS
  a supported option and silently displaces the family's flagged
  default (claude-fable-5-1 defaulting to 200k instead of 1M,
  2026-09-01). The catalog-aware pieces stay pure by taking the
  resolved options (`ValidateContextUpdate`, `DefaultContextWindowFor`)
  or the caller's resolver (`FallbackProfileWith`) as a PARAMETER; App
  code supplies `App.contextWindowOptionsForModel`, threadapp supplies
  `ModelPolicy.ContextWindowOptions`. The static functions here stay
  as the fallback layer, never the authority.
