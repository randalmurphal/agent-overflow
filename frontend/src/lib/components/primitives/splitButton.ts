// Chrome for the chat header's split button (Git commit/push). It renders
// a raw two-segment `<button>` pair rather than the `Button` primitive
// because that primitive has no split/caret shape; the base class here is
// what locks both halves to the 24px height, border, font size, and focus
// ring of the cluster's other controls.
//
// The `h-6` is load-bearing: if the two halves drift apart, or either
// drifts off 24px, the control's async git-status mount grows the chat
// header and reflows the timeline. Per-segment bits (padding, which
// corners round, the shared middle border) are appended at each use site.
export const SPLIT_BTN_BASE =
  'inline-flex items-center h-6 text-[0.6875rem] border border-border text-text-secondary ' +
  'hover:text-text-primary hover:border-text-secondary cursor-pointer ' +
  'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40';
