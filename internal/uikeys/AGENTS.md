# internal/uikeys/

WebviewWindow keybindings shared across every window Agent Overflow
opens. Centralised so a new shortcut lands in one place instead of
silently drifting between three call sites.

## Layout

- `keys.go` — `Browser()` returns the standard zoom / reload /
  fullscreen accelerators. Used by `main.go` (desktop window +
  `runClient` `--connect` window) and `cmd/agent-overflow-windows/main.go`
  (WSL launcher window).

## Responsibility boundary

- What BELONGS here: keybinding factories used by ≥2 WebviewWindow
  surfaces.
- What does NOT belong here: per-window-instance handlers,
  app-feature-specific shortcuts (composer, palette, etc.) that live
  inside the SPA, anything requiring non-Wails imports.

## Anti-patterns

- Do NOT inline a new browser-style binding in a call site. Add it
  here so every window picks it up.
- Do NOT pull in non-Wails dependencies. The launcher binary embeds
  this package; an unrelated transitive dep would bloat the `.exe`.
