# frontend/

Svelte 5 + Vite 8 (Rolldown) + Tailwind 4 + TypeScript.

## Commands

- `pnpm run check` — Svelte + TypeScript type check. Must pass.
- `pnpm run build` — production build. Must pass.
- `pnpm test` — Vitest unit tests.

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

## Scroll architecture

The MessageTimeline scroll surface is built on **`virtua/svelte`**'s
`<Virtualizer scrollRef={ourScrollEl}>`. Virtua owns geometry and per-row
anchor preservation (ResizeObserver + binary-searched jump-correction);
the frontend owns the scroll container itself, which is the outer
`<div class="overflow-y-auto">` in `MessageTimeline.svelte`. Owning the
container is what lets the spring controller observe content growth
**before paint** (single content-element ResizeObserver) and write
`scrollTop` synchronously in the same paint cycle — eliminating the
rAF gap between content layout and scroll correction that was the
flicker source. The frontend layers on top:

- **`useStickToBottom.svelte.ts`** — Svelte-5 port of stackblitz-labs'
  `use-stick-to-bottom`. Single owner of intent (`isAtBottom` flag) and
  the only writer to `scrollTop` outside virtua's internals. Spring
  driver (`damping=0.7`, `stiffness=0.05`, `mass=1.25`) chases the moving
  bottom while content keeps growing for ~350ms after the last grow
  event. Programmatic scrolls go through `forceStick({animation})` /
  `notifyContentMaybeGrew()` / `pauseAutoScroll()` / `stopScroll()`; the
  one place virtua writes scrollTop is `listRef.scrollToIndex(...)`,
  which MUST be preceded by `stick.stopScroll()` so the spring stops
  fighting the jump. Never write `scrollTop` directly.
- **`pane.scrollController`** — registration slot. Both
  `MessageTimeline.svelte` (chat) and `ChannelView.svelte` (Discussion)
  publish their `useStickToBottom` controller on mount; external
  surfaces (sidebar resizers, resizable drawers) acquire
  `pauseAutoScroll()` during their drag to keep auto-follow from
  yanking the user mid-gesture. The lease is depth-counted and
  idempotent. The pane only knows the minimal `PaneScrollController`
  interface (`pauseAutoScroll(): () => void` +
  `notifyContentMaybeGrew(): void`), so a single set of resizer/drawer
  hooks works on both surfaces.
- **`threadScrollSnapshots.ts`** — per-thread LRU of
  `{kind:'bottom'} | {kind:'anchor', itemId, offsetTop}`. Snapshots are
  semantic (item id + offset), not virtua's internal cache shape, so
  they survive virtua version bumps.
- **Layout decoupling** — `ChatView.svelte` positions the composer +
  live-turn UI + below-bar as an absolute overlay inside the timeline's
  relative container. A `--composer-height` CSS variable, written by a
  ResizeObserver on the overlay, drives the timeline's bottom padding
  so composer growth, working/todo panels, attachment trays, and
  approval panels never alter the scroll surface's `clientHeight`.
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
- **Expansion-state memory tradeoff.** The expansion registry keeps
  loaded payload chunks until the user collapses a row or switches
  thread. Open transcript rows are user-owned UI state; collapsing one
  from an unrelated row's load changes timeline height outside the
  user's interaction path and fights virtua.
- **Stable transcript rows.** Anything rendered inside `<Virtualizer>`
  is a stable history record. A row may update text/content in place, but it
  should not change its outer shell after first render: no static
  div-to-button swaps, no late chevron insertion, no completion-time
  summary cards appended inside history, and no live working/todo UI in
  the virtualized data. New transcript structures must decide their
  shell from provider metadata available at first render and keep later
  details inside reserved slots. Disclosure-style rows should compose
  `TranscriptDisclosureHeader.svelte` so toggle chrome and trailing
  actions keep the same DOM shape across loading/completion updates.
- **Shiki diff token cache.** `tokenCache.ts` partitions cached lines
  by `${theme}:${threadId}:${lang}:…` so a thread switch can drop
  every line tokenized under the outgoing thread without disturbing
  any other thread's tokens. `pane.switchThread` calls
  `clearTokensForThread(prevThreadId)` exactly once per switch; the
  partition + clear-on-switch is what bounds long-session memory.
  The fixed-cap LRU (5000 entries, ~5 MB worst case) only exists to
  absorb repeat-visit pressure within a single thread; it's
  deliberately large enough that a multi-thousand-line diff doesn't
  self-evict during initial render.

`ChannelView.svelte` (Discussion mode) shares the same
`useStickToBottom` controller. It scrolls a plain DOM container with no
virtualizer, but the controller's content-element ResizeObserver is
agnostic to what's inside contentEl — so the spring chases the bottom
the same way as the chat surface. Discussion's contentEl wraps the
`{#each}` over channel messages; the scroll element is the surrounding
`overflow-y-auto` div. The intervening
`<div bind:this={contentEl} class="space-y-3">` is intentional — it
gives the content-RO a target whose height tracks message-list growth
without including the scroll container's padding, mirroring chat's
`<div bind:this={contentEl}>` wrapper around the `<Virtualizer>`.

What NOT to add:
- Manual `scrollTop` writes outside the controller.
- A row-height signature cache. virtua re-measures via ResizeObserver.
- A scroll-anchor compensation pass on top of virtua's jump algorithm.
- A second virtualizer over the same data.
- A length-watching auto-follow `$effect` that calls
  `listRef.scrollToIndex(last, 'end')` on streaming. The content-RO inside
  `useStickToBottom` reproduces this synchronously before paint; a
  duplicate effect re-introduces the rAF gap that this architecture
  eliminated.
- `smooth: true` on any `listRef.scrollToIndex(...)` call. Virtua's
  smooth path uses the native `scrollTo({behavior:'smooth'})` which
  fights the spring driver. Always pair `scrollToIndex` with a preceding
  `stick.stopScroll()`.
- `transition:slide` adjacent to the scroll area — animated height
  shifts visible content under the user's cursor.
- Late transcript adornments on completion. If the UI needs a marker,
  attach it to the row boundary when that row first appears; don't add
  a separate end-of-turn row after the virtualizer has measured the
  previous bottom.

## Search

- Full-thread message search uses the in-app `MessageSearch` palette
  (Ctrl/Cmd+F, see `palette/MessageSearch.svelte`). The query goes
  through the `SearchThreadMessages` Wails binding which reads SQLite
  directly — coverage is independent of which rows are currently
  mounted in virtua. This is the canonical search surface, not the
  browser-native find which only sees mounted rows.
- A search hit calls `pane.requestScrollToItem(itemId)`;
  `MessageTimeline` reacts by paging older items in via
  `pane.loadUntilItem(id)` and then, after `stick.stopScroll()` +
  `stick.setEscapedFromLock(true)`, `listRef.scrollToIndex(idx, { align:
  'center' })`. The two-step (load-then-scroll) is necessary because
  virtua only knows about items present in `pane.items`. Never pass
  `smooth: true` — virtua's smooth path uses
  `scrollEl.scrollTo({behavior:'smooth'})` which races the spring; if
  smooth-to-hit is wanted later, route through a controller-owned
  `springTo(target)` API.

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
  highlighting off the client, but `svelte-streamdown` ships a
  `HighlighterManager` that dynamically loads shiki for code blocks
  inside assistant markdown (and a small set of payload expansions).
  Module-level caches inside the library keep per-row remount cheap.
- **Click-anchor preservation and pointerdown-defers-forceStick are
  deliberately NOT implemented in `useStickToBottom`.** The legacy
  Discussion controller had both: clicking a `<details>` / `<button>`
  inside a message would adjust `scrollTop` to keep the clicked element
  fixed in viewport, and a `forceStick` while the user was mid-drag of
  the scrollbar would defer until pointerup. Neither is reproduced in
  the unified controller: chat's transcript rows don't expand-collapse
  in place (every disclosure has a stable header that's part of the
  row's first-paint shell), and Discussion message bodies are plain
  Markdown without expandable affordances. The pointerdown-defer is a
  rare-input-mode case (mouse-drag of scrollbar + concurrent post)
  with no recorded user impact in the chat surface; treat as an
  accepted simplification for the unification.

## Raw-content rendering

Raw content is canonical. Go sends raw item summaries, channel message
content, and payload data; the frontend owns rendering as a viewport-local
projection.

Assistant text, discussion messages, and proposed plans render through
`ChatMarkdown.svelte`, which mounts a `<Streamdown>` (`svelte-streamdown`)
with our own thin host wrappers (`StreamdownCodeHost`, `StreamdownMermaidHost`,
`StreamdownMathHost`) that re-stamp the original source on `data-code-source`,
`data-mermaid-source`, and `data-math-source` so `markdownSerialize.ts`'s
copy-as-markdown round-trip keeps working. Streamdown owns markdown
parsing (via `marked`), shiki highlighting, KaTeX typesetting, mermaid
rendering, link/image URL prefix safety, and graceful incomplete-token
auto-close while streaming (`parseIncompleteMarkdown={streaming}`).
The library uses a token-keyed `{#each}` over marked blocks under the
hood, so DOM identity is preserved across content updates — text
selection, scroll-within-code, and previously-rendered shiki/mermaid
nodes all survive streaming chunks. Two custom post-process passes
remain in `markdownEnhance.ts`: project-relative path linkification
(`enhancePathLinks`) and the document-level markdown-aware copy
delegate (`ensureMarkdownCopyDelegate`).

ANSI-like payloads render through `AnsiText.svelte`, which builds an
HTML string from raw bytes and applies it to a stable `<pre>` via
`Idiomorph.morph(...)`. Idiomorph diffs the live DOM against the new
HTML and patches only changed nodes — text selection survives streaming
chunks, no per-line re-tokenization on each update.

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
- A failing `pnpm run check` is a blocker, not a warning.

## References

- Forge web app: `/Users/randy/repos/forge/apps/web/src/` — UX
  reference for ambiguous decisions.
- `docs/references/spike-policy.md` — when Wails binding behavior is
  unclear.
- Root `CLAUDE.md` principle 4 ("Frontend memory is bounded by the
  visible thread").
