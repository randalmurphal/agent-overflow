# frontend/

Svelte 5 + Vite 8 (Rolldown) + Tailwind 4 + TypeScript.

## Commands

- `pnpm run check` — Svelte + TypeScript type check. Must pass.
- `pnpm run build` — production build. Must pass.
- `pnpm test` — Vitest unit tests.

## Layout

- `src/lib/stores/` — runes-based reactive stores. `thread.svelte.ts`
  owns the per-thread `ThreadPane` factory; `events.ts` fans backend
  events into active panes; `bindings.ts` wraps generated Wails calls.
- `src/lib/components/panes/` — pane host/layout surfaces. This is the
  only place that should translate layout items into mounted chat panes.
- `src/lib/components/chat/` — timeline rendering. Kind-based
  discrimination; no role/content matching. See its local guide before
  editing rows, virtualized scrolling, markdown, or RHS panels.
- `src/lib/components/composer/` — message composer, mode / effort /
  model pickers.
- `src/lib/components/sidebar/` — projects + thread list.
- `src/lib/components/primitives/` — reusable Menu / Popover / Modal /
  dropdown shells. Pickers compose these rather than rolling their own
  positioning, focus-trap, or keyboard behavior.
- `src/lib/components/{design,discussion,git,palette,settings,terminal,shared}/` —
  per-feature component groups.
- `src/lib/types/` — shared TypeScript types.
- `src/lib/utils/` — pure helpers.
- `src/lib/transport/` — WebSocket client + `@wailsio/runtime` shim.
  Feature code should go through `stores/bindings.ts`, not this package.
- `bindings/` — Wails-generated TypeScript. Never edit by hand.

## State Boundaries

`ThreadPane` is the sole owner of per-thread runtime UI state: visible
items, streaming flags, approvals, design artifacts, channel messages,
token usage, right-side-panel state, terminal placement, and scroll
controller registration. Do not add a parallel streaming or timeline
state slice next to it.

Pane layout and pane runtime state are separate. Layout stores own
placement/order/min-size metadata; `ThreadPane` owns the chat/discussion
runtime state mounted into that slot. Command palette actions must resolve
against an explicit target pane because enablement can change while the
palette is open.

The visible-thread memory budget is load-bearing. Heavy payloads (diffs,
command output, thinking, attachments) load on demand through bindings.
Thread switch is a bounded-window load or cache restore, not a full-history
hydrate. Settled subagent children are evicted from pane memory into a
per-anchor fold (`utils/subagentFold.ts`) and re-hydrate on card
expansion; see "Live Window Bounds" in
[`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md).

## Thread Switch And Scroll

The durable contracts for cache restore, tail-only initial load, lazy
older paging, scroll intent, virtua integration, and scroll-regression
diagnostics live in
[`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md).
Read that before touching:

- `src/lib/stores/thread.svelte.ts`
- `src/lib/stores/threadItemCache.ts`
- `src/lib/stores/threadScrollSnapshots.ts`
- `src/lib/components/chat/MessageTimeline.svelte`
- `src/lib/components/discussion/ChannelView.svelte`
- `src/lib/utils/useStickToBottom.svelte.ts`

Short version: `MessageTimeline` owns the scroll container, `virtua`
owns row geometry, and `useStickToBottom` owns scroll intent and every
allowed `scrollTop` write outside virtua internals. Programmatic virtua
scrolls must be wrapped in `stick.runExternalScroll(...)`; never pass
`smooth: true`.

## Rendering

Raw content is canonical. Go sends raw item summaries, channel message
content, and payload data; the frontend renders them as viewport-local
projections. Do not add server-rendered chat HTML or a global DOM
observer.

Assistant text, discussion messages, and proposed plans render through
`ChatMarkdown.svelte` and `svelte-streamdown`. Path linkification happens
inside marked parsing using server-validated `PathRef[]` metadata and a
per-page-load nonce; click/copy behavior is delegated by
`markdownEnhance.ts`.

ANSI-like payloads render through `AnsiText.svelte`, which diffs into a
stable `<pre>` with Idiomorph so selection survives streaming updates.

## Anti-Patterns

- Do NOT create legacy stores. Runes only: `$state`, `$derived`,
  `$effect`, `$props`. No `export let`, no `$:`.
- Do NOT discriminate timeline items by role or content substring.
  Discriminate via `kind`.
- Do NOT re-order items during render. Upsert by `(turnIndex, itemIndex)`
  and let the store stay sorted.
- Do NOT implement count-based slicing for virtualization. Heavy content
  is expand-to-load, not preload.
- Do NOT stretch a `.svelte` file past roughly 300 lines when a clear
  component split exists.
- Do NOT put business logic in templates. Derive in `<script>`, render in
  the template.
- Do NOT call `window.runtime` directly. Use `stores/bindings.ts`.
- Do NOT preload heavy payloads.
- Do NOT add visible in-app explanatory text for internal mechanics,
  shortcuts, or implementation details.

## Testing

Store logic: unit-test with Vitest under `src/lib/stores/*.test.ts`.
Component behavior: add a component test when changing rendering or
interaction. Scroll behavior has dedicated coverage in
`src/lib/utils/useStickToBottom.svelte.test.ts` and
`src/lib/components/chat/scroll.test.ts`.

`pnpm run check` and `pnpm run build` are blockers. `pnpm test` is the
frontend unit-test gate.

## References

- [`docs/architecture/frontend-scroll.md`](../docs/architecture/frontend-scroll.md) —
  chat/discussion scroll architecture and diagnostics.
- [`docs/architecture/data-flow.md`](../docs/architecture/data-flow.md) —
  provider → triage → store → frontend pipeline.
- [`docs/architecture/how-to.md`](../docs/architecture/how-to.md) —
  extension playbooks.
- [`docs/references/spike-policy.md`](../docs/references/spike-policy.md) —
  when Wails or provider behavior is unclear.
- `/Users/randy/repos/forge/apps/web/src/` — UX reference for ambiguous
  decisions.
