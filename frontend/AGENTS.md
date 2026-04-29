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
- `src/lib/transport/` — WebSocket client + the `@wailsio/runtime` shim
  the production build aliases the Wails generator's import to. Bindings
  end up calling `wsClient.ts` over WS in production. Don't import from
  here directly in feature code; go through `stores/bindings.ts`.
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

## Chat scroll architecture

The MessageTimeline scroll surface is built on **`virtua/svelte`**'s
`<VList>`. Virtua owns geometry and per-row anchor preservation
(ResizeObserver + binary-searched jump-correction). The frontend layers
on top:

- **`stickyBottomController.svelte.ts`** — single owner of intent
  (`'stick' | 'free'`) and the only writer to scroll position outside
  virtua's internals. Programmatic scrolls go through
  `forceStick()` / `notifyContentMaybeGrew()` / `pauseAutoScroll()` or
  directly via `vlist.scrollToIndex(...)`. Never write `scrollTop`.
- **`scrollIntentCore.svelte.ts`** — shared state machine (intent
  transitions, gesture interpretation, pause-lease, down-gesture window)
  used by both `stickyBottomController` (virtua) and `stickToBottom`
  (DOM container, ChannelView). A change to "what counts as a down
  gesture" or "how long the restick window is" lands in one file, not
  both controllers.
- **`pane.scrollController`** — registration slot. MessageTimeline
  publishes its sticky controller on mount; external surfaces (sidebar
  resizers, resizable drawers) acquire `pauseAutoScroll()` during their
  drag to keep auto-follow from yanking the user mid-gesture. The lease
  is depth-counted and idempotent. The pane only knows the minimal
  `PaneScrollController` interface (`pauseAutoScroll(): () => void`).
- **`threadScrollSnapshots.ts`** — per-thread LRU of
  `{kind:'bottom'} | {kind:'anchor', itemId, offsetTop}`. Snapshots are
  semantic (item id + offset), not virtua's internal cache shape, so
  they survive virtua version bumps.
- **Layout decoupling** — `ChatView.svelte` positions the composer +
  below-bar as an absolute overlay inside the timeline's relative
  container. A `--composer-height` CSS variable, written by a
  ResizeObserver on the overlay, drives the timeline's bottom padding
  so composer growth (textarea autosize, attachment tray, approval
  panel) never alters the scroll surface's `clientHeight`.
- **Reserved-slot banners** — `ProviderStatusBanner.svelte` and
  `TransportStatusBanner.svelte` both use `min-h-N` wrappers +
  `transition:fade` so banner mount/unmount does not animate adjacent
  height. Cost: ~100px of always-reserved chrome across the two
  surfaces; banners appear in a stable location and never push the
  scroll viewport.
- **Row state survives virtua remount via pane-level registries.**
  Expansion state for tool-call payloads, attachment-blob URLs, and
  subagent-group expanded flag all live on the `ThreadPane` keyed by
  `item.id` / `payloadId`. Row components read the handle out of the
  pane on each mount (using `untrack` so reads don't bind the row to
  its initial value). This means scrolling a row past the
  `bufferSize=900` window and back preserves "show full output"
  toggles, loaded payload chunks, and any image blobs. Registries are
  cleared on `switchThread` to bound memory.

`ChannelView.svelte` (Discussion mode) uses a different controller —
`stickToBottom.svelte.ts` — because it scrolls a plain DOM container,
not a virtua list. The two controllers serve different surfaces.

What NOT to add:
- Manual `scrollTop` writes outside the controller.
- A row-height signature cache. virtua re-measures via ResizeObserver.
- A scroll-anchor compensation pass on top of virtua's jump algorithm.
- A second virtualizer over the same data.
- `transition:slide` adjacent to the scroll area — animated height
  shifts visible content under the user's cursor.

## Search

- Full-thread message search uses the in-app `MessageSearch` palette
  (Ctrl/Cmd+F, see `palette/MessageSearch.svelte`). The query goes
  through the `SearchThreadMessages` Wails binding which reads SQLite
  directly — coverage is independent of which rows are currently
  mounted in virtua. This is the canonical search surface, not the
  browser-native find which only sees mounted rows.
- A search hit calls `pane.requestScrollToItem(itemId)`;
  `MessageTimeline` reacts by paging older items in via
  `pane.loadUntilItem(id)` and then `vlist.scrollToIndex(idx, { align:
  'center', smooth: true })`. The two-step (load-then-scroll) is
  necessary because virtua only knows about items present in
  `pane.items`.

## Accepted scroll-surface tradeoffs

- **SubagentGroup inner overflow.** The expanded subagent body uses an
  internal `max-h-[20rem] overflow-y-auto` instead of nesting a
  virtualizer. Children are rendered eagerly when expanded, so a
  subagent with 200 children pays full DOM cost on expand. In
  practice subagents top out around 50 children (~100 KB DOM); the
  dense overview UX wins over micro-optimizing a worst case we don't
  see. Revisit only if a real thread shows DOM cost from a
  200+-child subagent.
- **Focus survival across virtua remount.** When the focused element
  belongs to a row that scrolls past `bufferSize=900` and unmounts,
  focus jumps to `<body>`. Tab through a long virtualized timeline
  is therefore fragile. Industry chat surfaces (Slack, Discord, VS
  Code chat) accept the same tradeoff. Revisit only if user
  feedback surfaces real keyboard-navigation pain.
- **`shiki` is still a dependency.** The Go-side SSR plan moved diff
  highlighting off the client, but `markdownEnhance.ts` still
  dynamically imports `shiki` for code blocks inside assistant
  markdown (and a small set of payload expansions). Module-level
  caches keep per-row remount cheap.

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
