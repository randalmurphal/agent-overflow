# frontend/

Svelte 5 with runes, TypeScript, Vite 8, and Tailwind CSS 4. The root
`package.json` forwards frontend commands here.

Use `pnpm run check:file <file.ts> ...` while editing plain TypeScript. It does
not check `.svelte` files or component props. Before completion run
`pnpm run check`, `pnpm run build`, and the relevant Vitest project. There is no
repository formatter; preserve the surrounding style.

## Navigation

Area guides own the contracts for
[`stores/`](src/lib/stores/AGENTS.md),
[`transport/`](src/lib/transport/AGENTS.md),
[`markdown/`](src/lib/markdown/AGENTS.md), and the guided component directories
under `src/lib/components/`.

`src/lib/native/` owns native capability implementations and bridges and is
documented by [`mobile/AGENTS.md`](../mobile/AGENTS.md). Components and transport
code may select behavior through those capability seams; keep platform API calls
inside `native/`. Layout is independent of the shell and responds to the viewport
through `stores/layoutMode.svelte.ts`.

## Component and state boundaries

- Reuse the primitives in `components/primitives/` for menus, popovers,
  dropdowns, drawers, and modals. `utils/popoverOwnership.ts` defines shared
  ownership and clipping behavior.
- `components/composer/ComposerInputSurface.svelte` is the shared editing core
  for the main composer and message editing.
- `components/panes/` is the only layer that mounts layout items as panes.
- Put state that outlives a component in the store for the entity it describes.
  Components derive from stores rather than maintaining consumer-keyed copies.
- Workspace actions use `pane.workspace` and the constructors in
  `utils/workspaceKey.ts`. Do not construct `WorkspaceRef` values at call sites.
- Keep heavy transcript payloads lazy and bounded to the visible window.
- Call generated bindings through `stores/bindings.ts`. Components do not
  subscribe directly to runtime events.

## Compact layout

Compact mode is selected by viewport and coarse pointer, not by device or run
mode. Every component must work in desktop and compact layouts.

- The sidebar and pane strip remain mounted as two screens. Do not set
  descendants to `visibility: visible`; clear an override so inherited hidden
  state remains effective.
- Compact navigation preserves pane and timeline state. Reveal through the
  layout store instead of remounting surfaces.
- Use the shared long-press context-menu bridge and shared menu primitives. Do
  not add component-specific long-press handlers.
- Popovers become sheets by default. Caret-owned composer lists and explicitly
  anchored controls may opt out.
- Android Back dismisses surfaces through `native/lifecycle.ts`. Only commands
  marked `dismissesSurface` may run from that path.

## Rendering and diagnostics

Raw backend content remains canonical; render Markdown, ANSI, paths, code, and
diagrams as viewport-local projections.

Wrap new top-level regions in `shared/RenderBoundary.svelte`. Report production
errors through `reportFrontendDiagnostic`; console-only errors are invisible in
normal builds. Clipboard failures go through
`utils/clipboard.ts#reportCopyFailure`. Diagnostic messages stay stable, and
details must exclude message payloads, copied content, credentials, and RPC
bodies.

Use semantic theme tokens registered in `lib/theme/tokenRegistry.ts`. Do not
introduce raw palette utilities, color literals outside the theme layer, or
per-token root style writes. Theme architecture is documented in
[`theme-system.md`](../docs/architecture/theme-system.md).

## Performance-sensitive rules

- Do not author conditional layer promotion or content transforms to repair
  scrolling. `.pane-scroll-surface` is the sole `will-change: scroll-position`
  exception.
- Standing indicators use `primitives/SteppedSpinner.svelte` or another stepped
  animation. Avoid continuously interpolated ambient motion.
- Use `utils/refreshScheduler.ts` for event-driven refresh so sustained event
  streams cannot defer work forever.
- Lazy feature surfaces must keep a stable dynamic-import promise. Verify eager
  entry chunks when changing a lazy boundary.
- Key global CSS selectors on a class, id, element, or attribute. Avoid
  featureless selector compounds that broaden style invalidation.
- Random identifiers come from `utils/randomId.ts`; plain HTTP remote clients
  cannot rely on `crypto.randomUUID`.
- Build sends with `utils/sendOptions.ts#buildSendOptions` so retries retain one
  idempotency identity.

Scroll and timeline motion are specified in
[`frontend-scroll.md`](../docs/architecture/frontend-scroll.md). Run-map motion
has its separate contract in
[`workflow-run-map.md`](../docs/architecture/workflow-run-map.md).

## Tests and dependencies

Add component tests for rendered behavior and focused unit tests for extracted
logic. Stateful APIs need transition coverage, including repeated attach and
detach. Browser-only geometry, focus, and scroll behavior belongs in the browser
project.

Preserve unlisted exports when mocking shared modules with `importOriginal`.
Prefer binding mocks over mocking through `.svelte.ts` import boundaries. Add
passive-load bindings to the shared app defaults when every rendered app needs
them.

Do not edit `node_modules`. Temporary fixes for active dependencies live in
versioned pnpm patches; adopted dormant code lives under `src/` with its license.
The first-party streaming Markdown pipeline is documented in
[`src/lib/markdown/AGENTS.md`](src/lib/markdown/AGENTS.md).
