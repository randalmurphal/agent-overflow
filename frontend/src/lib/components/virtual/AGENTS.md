# components/virtual/

The DOM adapter for the bespoke windowing engine (`utils/virtual/`),
shared by every virtualized surface: the chat timeline
(`chat/MessageTimeline.svelte`), discussion channels, and the review
pane. It is surface-agnostic. Chat-specific behavior (priors, restore,
paging, row projection) lives with its surface in `components/chat/`.

- `TimelineVirtualizer.svelte` binds the engine to the DOM: one lazy
  ResizeObserver for scroller + mounted rows, scroll-event feed, spacer +
  absolute row positioning, scrollend synthesis, the imperative handle
  (`TimelineVirtualizerHandle` in `utils/virtual/types.ts`).
- `VirtualRow.svelte` is the per-row mount/measure wrapper.

The adapter also owns the **reading anchor**: the one DOM measurement the
engine cannot make for itself. Whole-row `[index, height]` cannot say
whether a straddling row's growth landed above or below the viewport top,
so the adapter hit-tests the element at the top and tracks its offset
within its own row (`utils/virtual/readingAnchor.ts`). Consumers opt in
per-surface with `trackReadingAnchor`; the default is always-track, and
chat turns it off while the controller holds bottom-follow intent because
the pin write already covers that case. Rationale and the bounding
argument: `docs/architecture/frontend-scroll.md` § "The row that spans the
viewport top".

Ownership contract (see `docs/architecture/frontend-scroll.md`): this
component NEVER writes scrollTop. Compensations surface as observations;
imperative scrolls go through the required `applyScrollTarget` prop, so
each consumer decides who owns the write. Chat passes the scroll
controller chokepoint, simpler surfaces may pass a direct writer they
own. Do not add chat imports here; the dependency runs one way only
(surfaces import the adapter, never the reverse).

The write flows back too: the engine's scroll offset is the base every
compensation target is computed from, and a scroll event reports an
authored write one frame late. Chat forwards the controller's
`onScrollTopWritten` readback to `noteScrollTopWritten`, so a row
re-measuring above the viewport mid-glide compensates from where the
spring actually is, not where it was last frame (regression:
`activityRunAutoCollapse.browser.test.ts` "never yanks the glide").
Windowing still keys off the scroll event; the note moves the offset
only.

A pending `scrollToIndex` converges across measurement passes but may
only continue a journey nobody else has redirected: each pass checks the
live position against where its own last write left it (compensation
writes and the browser's shrink-clamp count as its own side) and cancels
on any other motion. A stale absolute target re-fired over a reader
gesture or a spring glide is a visible yank. Do not weaken this guard
to "keep converging harder"; a navigation that lost the viewport has
nothing left to navigate.
