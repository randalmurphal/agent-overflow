import type { ThreadPane } from '../../stores/thread.svelte';

export function preservePaneScrollAnchor(
  pane: ThreadPane | undefined,
  event: MouseEvent,
  action: () => void | Promise<void>,
): void | Promise<void> {
  const anchor = event.currentTarget instanceof HTMLElement ? event.currentTarget : null;
  const preserve = pane?.scrollController?.preserveScrollAnchor;
  if (!anchor || !preserve) {
    return action();
  }
  return preserve(anchor, action);
}
