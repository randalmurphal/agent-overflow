# components/virtual/

This directory adapts the surface-agnostic windowing engine in `utils/virtual/`
to the DOM. Chat, discussion channels, and review surfaces share it.

`TimelineVirtualizer.svelte` owns scroller and row observation, spacer layout,
absolute row positioning, scroll-event input, scroll-end synthesis, and the
`TimelineVirtualizerHandle`. `VirtualRow.svelte` mounts and measures one row.

- Keep this layer surface-agnostic. Surface-specific paging, restore, grouping,
  and scroll intent stay with the consumer. Imports flow from surfaces to this
  adapter, never back into chat or review.
- The adapter never writes `scrollTop` directly. Return targets through the
  required `applyScrollTarget` callback so the surface's scroll owner performs
  every write.
- Forward authored write readbacks through `noteScrollTopWritten`. Compensation
  must start from the current authored offset, including during animation.
- The reading anchor measures the element spanning the viewport top. Keep it
  enabled unless the consumer already owns bottom-follow positioning.
- A pending `scrollToIndex` may converge through later measurement passes only
  while the viewport remains where that journey last placed it. Cancel after
  unrelated motion rather than overriding reader input or another controller.

Read [`frontend-scroll.md`](../../../../../docs/architecture/frontend-scroll.md)
before changing measurement, compensation, anchoring, or write ownership.
