# Prompt override matching and rendering

This package is the pure part of system-prompt overrides. Application code
gathers spawn-time facts, performs filesystem changes, and reconciles live
sessions. See `docs/specs/prompt-tool-overrides.md`.

## Contracts

- `Match` evaluates entries in user order and returns the first enabled,
  nonblank prompt whose model matches after `provider.NormalizeModelSlug`.
  Model-marker policy stays in the provider package.
- `Render` recognizes only the declared `Token*` constants in one left-to-right
  pass. Unknown placeholders remain verbatim, missing facts become empty text,
  and substituted values are never rescanned.
- Adding a placeholder requires updating `Facts`, `Render`, the application fact
  gatherer, render tests, and the frontend token list.
  `TestPlaceholderTokensMatchTheFrontendMirror` enforces the shared vocabulary.
- Keep matching and rendering free of subprocesses, host probes, and writes so
  they remain safe on live reconciliation paths.
- `ClaudeMemoryDir` delegates workspace encoding to
  `sessionfork.WorkspaceProjectDir`. Do not create another Claude project-slug
  encoder. Directory creation belongs to the application path that renders the
  override.
