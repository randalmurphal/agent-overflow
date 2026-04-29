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
  structural and intentionally has NO `data-item-id` — that attribute
  belongs on the inner row component (`TimelineLeaf` for leaves,
  `SubagentGroup` for subagent cards) so test queries and the
  CompletionDivider's "render before [data-item-id]" sibling check land
  on the right element.
- Is safe to remount when virtua scrolls a row out of and back into the
  rendered window. Snippets re-receive `pane`, `item`, `depth` on
  remount; nothing inside should depend on `onMount` running exactly
  once per item lifetime.
- Defers heavy work (Mermaid render, Shiki highlight, KaTeX typeset,
  attachment image load) to dynamic imports / IntersectionObserver
  triggered from the row itself. Module-level singletons in
  `markdownEnhance.ts` cache the underlying highlighter / mermaid
  instance so per-row remount is just DOM work.

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
