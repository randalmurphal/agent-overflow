# frontend/

Svelte 5 + Vite 8 (Rolldown) + Tailwind 4 + TypeScript.

## Commands

- `npm run check` — Svelte + TypeScript type check. Must pass.
- `npm run build` — production build. Must pass.
- `npm test` — Vitest unit tests.

## Layout

- `src/lib/stores/` — runes-based reactive stores. `thread.svelte.ts`
  owns the per-thread `ThreadPane` factory (items, payload meta,
  streaming, approvals, design artifacts, channel messages, token
  usage). `events.ts` declares custom event names. `bindings.ts` wraps
  the auto-generated Wails bindings.
- `src/lib/components/chat/` — timeline rendering. Kind-based
  discrimination; no role/content matching.
- `src/lib/components/composer/` — message composer, mode / effort /
  model pickers.
- `src/lib/components/sidebar/` — projects + thread list.
- `src/lib/components/primitives/` — reusable Menu / Popover / Modal /
  dropdown shells. Every picker in the composer toolbar and sidebar
  composes these rather than rolling its own positioning / focus-trap /
  keyboard handling.
- `src/lib/components/{design,discussion,git,palette,settings,terminal,shared}/` —
  per-feature component groups.
- `src/lib/types/` — shared TypeScript types.
- `src/lib/utils/` — pure helpers.
- `bindings/` — Wails-generated TypeScript. Never edit by hand.

## Responsibility boundary

- What BELONGS here:
  - UI rendering, routing between panes, user input capture.
  - Reactive state for the visible thread (items, approvals, streaming
    flags, token usage).
  - On-demand fetching of heavy payloads via bindings.
- What does NOT belong here:
  - Business decisions about turns, forks, approvals — those are
    decided in Go and surfaced via events or bindings.
  - Direct `window.runtime` calls — always go through the typed
    wrappers in `stores/bindings.ts`.
  - Parallel state slices for streaming. `ThreadPane` is the sole
    owner.

## State shape

- `ThreadPane` factory (in `stores/thread.svelte.ts`) owns all
  per-thread reactive state — items, payload meta, streaming,
  approvals, design artifacts, channel messages, token usage.
- Panes live in a registry; v1 runs a single main pane but the factory
  shape leaves room for tiling / multi-pane without a rewrite.
- The sidebar thread list is its own store — it doesn't hold pane
  state.

## Events in

- `app.Event.On('provider-event', ...)` — fan out to active panes.
- `app.Event.On('error', ...)` — toast + status bar.
- Custom event names per feature are defined in `stores/events.ts`.

## Raw-content rendering

Raw content is canonical. Go sends raw item summaries, channel message
content, and payload data; the frontend owns rendering as a viewport-local
projection.

Assistant text, discussion messages, and proposed plans render through
`ChatMarkdown.svelte`. The cheap markdown pass is synchronous; heavy
enrichments such as Shiki, Mermaid, and KaTeX are dynamically imported
from the mounted row and discarded if the source changes. ANSI-like
payloads render through `AnsiText.svelte` from raw bytes.

Do not add a server-rendered chat HTML field or a global DOM observer.
Copy/download paths read raw `summary` / `content` / `data`.

## Extension points

- To add a new event kind rendered by chat: add a kind constant in
  `stores/events.ts`, a renderer in `components/chat/`, and a
  `ThreadPane` reducer branch. See
  `docs/architecture/how-to.md#add-a-new-event-kind`.
- To add a composer mode / picker: compose the primitives under
  `components/primitives/`; don't roll custom positioning.
- To regenerate Wails bindings: run `wails3 task common:generate:bindings`,
  which passes `-ts`. Never edit files in `bindings/` by hand.

## Anti-patterns

- Do NOT create legacy stores. Runes only — `$state`, `$derived`,
  `$effect`, `$props`. No `export let`, no `$:`.
- Do NOT maintain a parallel state slice for streaming next to the
  persisted timeline. One owner per pane.
- Do NOT discriminate timeline items by role or content substring.
  Discriminate via `kind`.
- Do NOT re-order items per render. Upsert by `(turnIndex, itemIndex)`
  and let the store stay sorted.
- Do NOT implement count-based slicing for virtualization (forge's
  `useDeferredValue` ping-pong, count-window approaches). Heavy
  content is on-demand — expand-to-load, not preload.
- Do NOT stretch a `.svelte` file past ~300 lines. Extract instead.
- Do NOT add business logic to templates. Derive in `<script>`, render
  in the template.
- Do NOT call `window.runtime` directly. Use `stores/bindings.ts`.
- Do NOT preload heavy content. Diffs, command output, thinking —
  fetch via bindings when the user expands.

## Testing

- Store logic: unit-test with Vitest under `src/lib/stores/*.test.ts`.
- Component rendering: coverage is thin; when you add or change
  behavior, add a component test that would fail without the change.
- A failing `npm run check` is a blocker, not a warning.

## References

- Forge web app: `/Users/randy/repos/forge/apps/web/src/` — UX
  reference for ambiguous decisions.
- `docs/references/spike-policy.md` — when Wails binding behavior is
  unclear.
- Root `CLAUDE.md` principle 4 ("Frontend memory is bounded by the
  visible thread").
