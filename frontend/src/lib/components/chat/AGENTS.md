# components/chat/

Chat-surface components. The owning module is `MessageTimeline.svelte`.

## Scroll contract

See `frontend/AGENTS.md` § Chat scroll architecture for the high-level
shape. Operational rules for code in this directory:

- Use `listRef.findItemIndex(offset)` / `listRef.getItemOffset(index)` /
  `listRef.scrollToIndex(...)` for anything that needs to know "where am
  I in the timeline" or "go to row X". `listRef` is now a
  `VirtualizerHandle` (we use `<Virtualizer>` with our own `scrollRef`
  rather than `<VList>` which would own the scroller). Don't query the
  DOM for first-visible-item or write `scrollTop` directly.
- Programmatic scrolls go through `useStickToBottom` (`forceStick`,
  `notifyContentMaybeGrew`, `pauseAutoScroll`, `stopScroll`) or directly
  via `listRef.scrollToIndex(...)`. **Always call `stick.stopScroll()`
  BEFORE any `listRef.scrollToIndex(...)` and never pass `smooth: true`**
  — virtua's smooth path uses `scrollEl.scrollTo({behavior:'smooth'})`
  natively, which would fight the spring. Never `el.scrollIntoView()`
  on a row that lives inside the virtualizer; virtua won't see it and
  will fight the scroll.
- Don't add a parallel virtualizer over `pane.items` or `groupedNodes`.
- The auto-follow `$effect` is gone. Streaming flow is: text rewrites in
  the streaming row → row's height changes → virtua's per-row RO bumps
  `totalSize` → `contentEl.scrollHeight` changes → our content-RO fires
  before paint → spring slams to new target in the same paint. Don't
  reintroduce a length-watching effect that calls `scrollToIndex(last)`.
- Scroll-behavior tests live in `scroll.test.ts`. Component-shape tests
  for individual rows (TimelineLeaf, SubagentGroup, CommandOutput, etc.)
  stay in their own `*.test.ts` files.

## Row contract

Every row rendered inside `<Virtualizer>`'s children snippet:

- Lives inside a `[data-row-index]` outer wrapper. The wrapper is
  structural and intentionally has NO `data-item-id`. Only `TimelineLeaf`
  emits `data-item-id` on its root — that's what test queries, message
  search, and row-boundary markers anchor on. `SubagentGroup` is
  structural and does not carry `data-item-id`; response dividers
  therefore can only ever sit before a leaf, not before a subagent card.
  `shouldRenderTurnBoundaryBefore` in `MessageTimeline.svelte`
  enforces that contract by returning false for non-leaf nodes. The
  divider has two visual modes — labeled (`line | gap | pill | gap |
  line`) when the leaf is the final assistant_text of a settled turn,
  unlabeled (one continuous full-width line) otherwise. Both modes
  share a fixed wrapper height (`h-[1.625rem]`); the pill uses
  `leading-tight` to keep its content inside that wrapper across
  font-loading variance. Promoting an intermediate divider to "final"
  on turn settle therefore swaps the inner branch without changing row
  geometry — satisfies the "no late transcript adornments on
  completion" rule in `frontend/CLAUDE.md`. Tests discriminate the two
  modes via `data-final-response` on the wrapper plus presence/absence
  of "Response" in `divider.textContent`.
- Keeps its outer shell stable after first render. If a tool row might
  eventually have payload, render the header affordance from the start
  and disable the action until the body exists. Do not swap static rows
  into buttons, insert chevrons late, animate body height inside the
  scroll surface, or append completion-only history rows.
- Uses `TranscriptDisclosureHeader.svelte` for transcript disclosure
  headers unless there is a specific reason not to. The primitive keeps
  the chevron/button shell stable, uses `aria-disabled` for temporarily
  inert disclosures, and renders trailing actions as siblings so editor
  links / side-panel buttons are never nested inside another button.
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
    `GenericToolCallRow`, `CommandOutput`, `DiffFileStack`,
    `ThinkingBlock`, `LazyContentBlock`. Diff rows render
    always-inline through `DiffFileBlock`, so the body of the
    expansion handle here is just the lazy fetch — there is no
    user-facing expand/collapse for diffs.
  - `pane.attachmentCacheFor(itemId)` — image-attachment blob URL
    cache. `UserMessage` threads this into `createAttachmentPreviews`
    so a user-message row doesn't re-fetch `GetAttachmentData` on
    every scroll-back.
  - `pane.isSubagentGroupExpanded(groupKey)` /
    `toggleSubagentGroupExpanded(groupKey)` — collapse state for
    subagent cards. Use `SubagentGroupNode.groupKey`, not a raw parent
    item id.
  Read pattern: `const handle = $derived(pane.expansionStateFor(item))`,
  with any local fallback wrapped in `untrack(() => createPayloadExpansion(...))`
  so the fallback doesn't bind to initial prop values.
- Defers heavy work (Mermaid render, Shiki highlight, KaTeX typeset,
  attachment image load) to dynamic imports / IntersectionObserver
  triggered from the row itself. Module-level singletons in
  `markdownEnhance.ts` cache the underlying highlighter / mermaid
  instance so per-row remount is just DOM work.

## Markdown rendering pipeline

`ChatMarkdown.svelte` mounts `<Streamdown>` (svelte-streamdown), which
owns marked-based parsing, shiki highlighting, KaTeX typesetting,
mermaid rendering, and `parseIncompleteMarkdown` auto-close for
streaming sources. Three thin Svelte hosts under
`components/chat/markdown/` wrap the library's built-in Code, Mermaid,
and Math components and stamp the original source onto a wrapping
element (`data-code-source` / `data-mermaid-source` / `data-math-source`
+ legacy `math-inline` / `math-display` / `mermaid` classes) so
`markdownSerialize.ts`'s copy-as-markdown round-trip and
`DiagramInteractionHost`'s right-click "copy source" still work.

`markdownEnhance.ts` is now a thin file that re-exports the markdown-
aware copy delegate (`ensureMarkdownCopyDelegate`) and ships
`enhancePathLinks(container, workspacePath)` for the project-relative
path linkifier — the only post-Streamdown enhancement we still own.
The path linkifier walks text nodes for `src/lib/foo.ts:L:C` style
patterns, replacing them with `<a class="editor-link">` anchors that a
document-level click delegate routes to the `OpenInEditor` binding.
ChatMarkdown calls it from a `$effect` once `streaming` flips false.

All other module-level caches (shiki highlighter, mermaid SVG promise
cache, language extension map) live inside `svelte-streamdown` itself.
Per-row remount is still cheap because Streamdown's caches survive
component remounts at the library level.

## Test environment notes

happy-dom returns 0 for `clientHeight` / `clientWidth`, which would make
virtua mount zero rows. `MessageTimeline.svelte` switches virtua into
`ssrCount` mode under `import.meta.env.MODE === 'test'` so component
tests can assert on rendered DOM. Production (`vite dev` / `vite build`)
sees the default `undefined`, leaving virtua free to virtualize.

`useStickToBottom.svelte.test.ts` covers the spring controller's full
state machine in isolation: spring tick + arrival, content-RO
positive/negative deltas, wheel/scroll/keydown/touchmove gesture
handlers, programmatic-write tagging (`ignoreScrollToTop`), pause-lease
depth-counting, and lifecycle (re-attach detaches old listeners; detach
clears all timers). Geometry is stubbed per-test via
`Object.defineProperty` on `scrollHeight`/`clientHeight`/`scrollTop`,
and `performance.now` is mocked to advance 16.67ms per `nextFrame()` so
spring physics are deterministic.

`scroll.test.ts` covers the MessageTimeline-level integration: snapshot
save/restore, load-older flow, scroll-to-item routing, and layout
invariants (composer-height variable, reserved-slot banners). Heavy
reliance on real layout is avoided — assertions are written so they
hold under happy-dom's missing geometry.
