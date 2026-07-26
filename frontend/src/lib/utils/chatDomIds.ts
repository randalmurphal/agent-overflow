// Synthetic DOM ids for chat rows, scoped to the pane that renders them.
//
// Every such id is built from an item id, a payload id, or a group key. Those
// are unique within a THREAD — the store's primary key is
// `(thread_id, item_id)` — not within the document. Two panes can hold the
// same thread at once, and then both emit the same row: `aria-controls` and
// `aria-labelledby` resolve to whichever copy comes first in the document, so
// a screen reader following the disclosure in the right-hand pane is handed
// the left-hand pane's body. `document.getElementById` lookups have the same
// ambiguity.
//
// Call this for BOTH halves of a disclosure — the header's `controls` and the
// body's `id` — from ONE derived value per row. Writing the string twice is
// how the two drift apart, and a disclosure pointing at an id that no longer
// exists fails silently: it looks correct and announces nothing.

/**
 * `prefix-paneId-key` inside a pane, `prefix-key` outside one.
 *
 * A row rendered without a pane (a standalone diff preview, a companion body)
 * appears once per document and needs no scope. A row inside a pane always
 * has one, so pass the pane wherever the timeline renders it.
 */
export function chatRowDomId(
  pane: { readonly paneId: string } | undefined,
  prefix: string,
  key: string,
): string {
  return pane ? `${prefix}-${pane.paneId}-${key}` : `${prefix}-${key}`;
}
