# components/virtual/

The DOM adapter for the bespoke windowing engine (`utils/virtual/`),
shared by every virtualized surface: the chat timeline
(`chat/MessageTimeline.svelte`), discussion channels, and the review
pane. It is surface-agnostic — chat-specific behavior (priors, restore,
paging, row projection) lives with its surface in `components/chat/`.

- `TimelineVirtualizer.svelte` — binds the engine to the DOM: one lazy
  ResizeObserver for scroller + mounted rows, scroll-event feed, spacer +
  absolute row positioning, scrollend synthesis, the imperative handle
  (`TimelineVirtualizerHandle` in `utils/virtual/types.ts`).
- `VirtualRow.svelte` — the per-row mount/measure wrapper.

Ownership contract (see `docs/architecture/frontend-scroll.md`): this
component NEVER writes scrollTop. Compensations surface as observations;
imperative scrolls go through the required `applyScrollTarget` prop, so
each consumer decides who owns the write — chat passes the scroll
controller chokepoint, simpler surfaces may pass a direct writer they
own. Do not add chat imports here; the dependency runs one way only
(surfaces import the adapter, never the reverse).
