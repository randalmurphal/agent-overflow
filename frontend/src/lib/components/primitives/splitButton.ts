// Shared chrome for the chat-header split buttons (Git commit/push and
// Open-in-editor). Both render a raw two-segment `<button>` pair rather
// than the `Button` primitive because that primitive has no split/caret
// shape; keeping their base class here is what locks them to the SAME
// 24px height, border, font size, and focus ring so the header cluster
// reads as one row.
//
// The `h-6` is load-bearing: if the two halves of a split button drift
// apart, or either drifts off 24px, the control's async git-status /
// editor-catalog mount grows the chat header and reflows the timeline.
// Per-segment bits (padding, which corners round, the shared middle
// border) are appended at each use site.
export const SPLIT_BTN_BASE =
  'inline-flex items-center h-6 text-[0.6875rem] border border-border text-text-secondary ' +
  'hover:text-text-primary hover:border-text-secondary cursor-pointer ' +
  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';
