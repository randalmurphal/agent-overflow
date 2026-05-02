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
  search, and row-boundary markers anchor on. `SubagentGroup` is
  structural and does not carry `data-item-id`; response dividers
  therefore can only ever sit before a leaf, not before a subagent card.
  `shouldRenderResponseDividerBefore` in `MessageTimeline.svelte`
  enforces that contract by returning false for non-leaf nodes.
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
    `GenericToolCallRow`, `CommandOutput`, `DiffPreview`,
    `ToolResultCard`, `ThinkingBlock`, `LazyContentBlock`.
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

`stickyBottomController.svelte.test.ts` covers the controller's intent
state machine in isolation. `scroll.test.ts` covers the
MessageTimeline-level integration: snapshot save/restore, load-older
flow, scroll-to-item routing, and layout invariants (composer-height
variable, reserved-slot banners). Heavy reliance on real layout is
avoided — assertions are written so they hold under happy-dom's missing
geometry.
