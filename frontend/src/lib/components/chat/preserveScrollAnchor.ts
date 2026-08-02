import type { ThreadPane } from '../../stores/thread.svelte';

/**
 * Run `action` while holding `anchor`'s viewport position, so a row that
 * changes height keeps the thing the reader is looking at where it was.
 *
 * The anchor is the element whose top edge must not move — everything below
 * it absorbs the delta. For a disclosure header that is the control itself
 * (the body grows underneath it), which is what `preservePaneScrollAnchor`
 * assumes; a control that sits BELOW the region it grows must pass the region
 * instead, or holding the button still would push the text off the top.
 */
export function preservePaneScrollAnchorAt(
  pane: ThreadPane | undefined,
  anchor: HTMLElement | null | undefined,
  action: () => void | Promise<void>,
): void | Promise<void> {
  const preserve = pane?.scrollController?.preserveScrollAnchor;
  if (!anchor || !preserve) {
    return action();
  }
  return preserve(anchor, action);
}

export function preservePaneScrollAnchor(
  pane: ThreadPane | undefined,
  event: MouseEvent,
  action: () => void | Promise<void>,
): void | Promise<void> {
  const anchor = event.currentTarget instanceof HTMLElement ? event.currentTarget : null;
  return preservePaneScrollAnchorAt(pane, anchor, action);
}
