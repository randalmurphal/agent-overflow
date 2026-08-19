// The document's light/dark class stamp — DOM writes only.
//
// There is exactly ONE resolver for "which mode are we in":
// `stores/themeMode.svelte.ts#getResolvedTheme`, which owns the single
// `matchMedia(prefers-color-scheme)` subscription for the whole app. This
// module used to carry a second, independent listener of its own, so a
// system-theme flip travelled two unrelated paths (this one repainted the
// html class; the store's one repainted the xterm/mermaid surfaces) with no
// ordering relationship between them. Keep this file a pure applier: if you
// need to know the mode, read the store.
//
// The type lives HERE rather than in the store so the dependency runs one
// way: a pure util must not import from `stores/`. The store re-exports it,
// so existing importers are unaffected.

export type ResolvedTheme = 'light' | 'dark';

/**
 * Stamps `html.light` / `html.dark` for the already-resolved mode.
 *
 * BOTH classes are written even though only `html.light` is read by our own
 * CSS. `.dark` is kept as a conventional root marker for the vendored
 * streamdown's MutationObserver; that observer's outputs are currently
 * short-circuited (`mermaidConfig.theme` is always set, and the shiki Code
 * path is replaced by our own host), but stamping both is cheap insurance
 * against a future consumer — ours or the vendor's — reading the
 * conventional marker.
 *
 * Idempotent, and deliberately reads the live DOM rather than caching the
 * last value: a no-op call must not touch the class attribute at all, or
 * every unrelated settings change would wake that observer.
 */
export function applyThemeClass(resolved: ResolvedTheme): void {
  const root = document.documentElement;
  const other: ResolvedTheme = resolved === 'dark' ? 'light' : 'dark';
  if (root.classList.contains(resolved) && !root.classList.contains(other)) return;
  root.classList.remove(other);
  root.classList.add(resolved);
}
