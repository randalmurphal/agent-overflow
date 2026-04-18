# frontend/

Svelte 5 + Vite 8 (Rolldown) + Tailwind 4 + TypeScript.

## Commands

- `npm run check` — Svelte + TypeScript type check. Must pass.
- `npm run build` — production build. Must pass.
- `npm test` — Vitest unit tests.

## Layout

- `src/lib/components/` — UI components grouped by feature.
- `src/lib/components/primitives/` — reusable Menu / Popover / Modal /
  dropdown shells. Every picker in the composer toolbar and sidebar
  (ModeCycleButton, EffortMenu, ModelProviderMenu, UnifiedThreadPicker,
  ThreadFromPRDialog, etc.) composes these rather than rolling its
  own positioning / focus-trap / keyboard handling.
- `src/lib/stores/` — reactive stores (runes). See subarea guide.
- `src/lib/types/` — shared TypeScript types.
- `src/lib/utils/` — pure helpers.
- `bindings/` — Wails-generated TypeScript. Never edit by hand.

## Rules

- **Runes only.** `$state`, `$derived`, `$effect`, `$props`. No legacy
  stores, no `export let`, no reactive `$:` syntax.
- **Tailwind v4 is CSS-native.** Theme tokens go in `@theme` inside
  `app.css`. No `tailwind.config.js`.
- **Components stay small.** Aim for < 300 lines per `.svelte` file.
  When you hit the limit, extract — don't stretch.
- **No business logic in templates.** Derive in `<script>`, render in
  the template.
- **Bindings are typed.** Import from `src/lib/stores/bindings.ts`.
  Don't call `window.runtime` directly.
- **Heavy content is on-demand.** Diffs, command output, thinking —
  fetch via bindings when the user expands, don't preload.

## State Shape

- `ThreadPane` factory (in `stores/thread.svelte.ts`) owns all
  per-thread reactive state — items, payload meta, streaming,
  approvals, design artifacts, channel messages, token usage.
- Panes live in a registry; v1 runs a single main pane but the
  factory shape leaves room for tiling / multi-pane without a rewrite.
- The sidebar thread list is its own store — it doesn't hold pane
  state.

## Events In

- `app.Event.On('provider-event', ...)` — fan out to active panes.
- `app.Event.On('error', ...)` — toast + status bar.
- Custom event names per feature are defined in `stores/events.ts`.

## Testing

- Store logic: unit-test with Vitest under `src/lib/stores/*.test.ts`.
- Component rendering: the current coverage is thin. When you add or
  change behavior, add a component test that would fail without the
  change.
- A failing `npm run check` is a blocker, not a warning.

## When Unsure

- If a UI decision is ambiguous, look at `forge/apps/web/src/` for the
  UX reference.
- If behavior against a Wails binding is unclear, spike-test in a
  throwaway app — see `/docs/references/spike-policy.md`.
