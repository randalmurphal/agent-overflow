# components/chat/

Chat-surface components. The owning module is `MessageTimeline.svelte`.

## Scroll contract

See `frontend/AGENTS.md` § Chat scroll architecture for the high-level
shape. Operational rules for code in this directory:

- Use `listRef.findItemIndex(offset)` / `listRef.getItemOffset(index)` /
  `listRef.scrollToIndex(...)` for anything that needs to know "where am
  I in the timeline" or "go to row X". Don't query the DOM for
  first-visible-item or write `scrollTop` directly.
- Programmatic scrolls go through `stickyBottomController` (`forceStick`,
  `notifyContentMaybeGrew`, `pauseAutoScroll`) or directly via
  `vlist.scrollToIndex(...)`. Never `el.scrollIntoView()` on a row that
  lives inside the virtualizer; virtua won't see it and will fight the
  scroll.
- Don't add a parallel virtualizer over `pane.items` or `groupedNodes`.
- Scroll-behavior tests live in `scroll.test.ts`. Component-shape tests
  for individual rows (TimelineLeaf, SubagentGroup, CommandOutput, etc.)
  stay in their own `*.test.ts` files.

## Row contract

Every row rendered inside `<VList>`'s children snippet:

- Lives inside a `[data-row-index]` outer wrapper. The wrapper is
  structural and intentionally has NO `data-item-id`. Only `TimelineLeaf`
  emits `data-item-id` on its root — that's what test queries, message
  search, and the CompletionDivider's "render before [data-item-id]"
  sibling check anchor on. `SubagentGroup` is structural and does not
  carry `data-item-id`; the divider therefore can only ever sit before
  a leaf, not before a subagent card. `shouldRenderDividerBefore` in
  `MessageTimeline.svelte` enforces that contract by returning false
  for non-leaf nodes.
- Is safe to remount when virtua scrolls a row out of and back into the
  rendered window. Snippets re-receive `pane`, `item`, `depth` on
  remount; nothing inside should depend on `onMount` running exactly
  once per item lifetime.
- Reads any "remembered" state (expansion toggles, loaded payload
  chunks, attachment blob URLs) out of a per-pane registry on the
  `ThreadPane`, NOT from local `let foo = $state(false)`. Local row
  state is wiped when virtua remounts the row; the registries are
  keyed on `item.id` / `payloadId` and survive remount. See:
  - `pane.expansionStateFor(item)` — payload expansion handle
    (preview/full toggle, loaded chunks). Used by
    `GenericToolCallRow`, `CommandOutput`, `DiffPreview`,
    `ToolResultCard`, `ThinkingBlock`, `LazyContentBlock`.
  - `pane.attachmentCacheFor(itemId)` — image-attachment blob URL
    cache. `UserMessage` threads this into `createAttachmentPreviews`
    so a user-message row doesn't re-fetch `GetAttachmentData` on
    every scroll-back.
  - `pane.isSubagentGroupExpanded(parentId)` /
    `toggleSubagentGroupExpanded(parentId)` — collapse state for
    subagent cards.
  Read pattern: `const handle = $derived(pane.expansionStateFor(item))`,
  with any local fallback wrapped in `untrack(() => createPayloadExpansion(...))`
  so the fallback doesn't bind to initial prop values.
- Defers heavy work (Mermaid render, Shiki highlight, KaTeX typeset,
  attachment image load) to dynamic imports / IntersectionObserver
  triggered from the row itself. Module-level singletons in
  `markdownEnhance.ts` cache the underlying highlighter / mermaid
  instance so per-row remount is just DOM work.

## Markdown enhancement caches

`markdownEnhance.ts` carries module-level caches that make per-row
remount cheap and bound the cost of repeated content:

- **Shiki highlighter** — single instance constructed lazily on first
  use, reused across every code block in every row. Languages and
  themes are loaded once.
- **Mermaid SVG cache** — `Map<sourceHash, Promise<string>>` keyed by
  a fast hash of the diagram source. Bounded LRU (50 entries). The
  cache holds **promises**, not strings, so a remount that races a
  first render reuses the same in-flight render rather than starting
  a parallel one. Module exposes `__resetMermaidSvgCacheForTest()`
  for unit tests; production code never calls it.
- **Mermaid source data attribute** — `enhanceMarkdown` writes the
  raw diagram source to `pre.dataset.mermaidSource` after rendering
  the SVG. Both the copy-as-markdown serializer and
  `DiagramInteractionHost`'s `copy-source` action read from there;
  a data attribute survives `Range.cloneContents`, where a WeakMap
  lookup keyed on the original element wouldn't.

## Test environment notes

happy-dom returns 0 for `clientHeight` / `clientWidth`, which would make
virtua mount zero rows. `MessageTimeline.svelte` switches virtua into
`ssrCount` mode under `import.meta.env.MODE === 'test'` so component
tests can assert on rendered DOM. Production (`vite dev` / `vite build`)
sees the default `undefined`, leaving virtua free to virtualize.

`stickyBottomController.svelte.test.ts` covers the controller's intent
state machine in isolation. `scroll.test.ts` covers the
MessageTimeline-level integration: snapshot save/restore, load-older
flow, scroll-to-item routing, and layout invariants (composer-height
variable, reserved-slot banners). Heavy reliance on real layout is
avoided — assertions are written so they hold under happy-dom's missing
geometry.
