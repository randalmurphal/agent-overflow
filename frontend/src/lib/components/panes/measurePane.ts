// Svelte action that publishes a pane section's measured width to the
// shared layoutMetrics store and notifies the caller when the pane mounts,
// resizes, swaps paneId, or unmounts. Used by PaneHost to keep both the
// global per-pane width registry and the host-local offset map in sync.
// A promoted pane's section is laid over the strip while its slot keeps the
// strip's layout, so while `promoted` the section's own size is not its
// slot width and is not published; the next update after demote publishes
// the slot again.

import { clearPaneWidth, setPaneWidth } from '../../stores/layoutMetrics.svelte';

export interface MeasurePaneOptions {
  paneId: string;
  // Notifies the host when this pane is present (mount, resize, paneId swap)
  // or gone (unmount or paneId replaced) so it can republish strip offsets or
  // drop stale entries without forcing synchronous offsetLeft reads here.
  onPresenceChange(paneId: string, present: boolean): void;
  promoted?: boolean;
}

export function measurePane(node: HTMLElement, options: MeasurePaneOptions) {
  let currentPaneId = options.paneId;
  let onPresenceChange = options.onPresenceChange;
  let promoted = options.promoted ?? false;

  function publish(): void {
    if (promoted) return;
    setPaneWidth(currentPaneId, node.getBoundingClientRect().width);
    onPresenceChange(currentPaneId, true);
  }

  const obs = new ResizeObserver((entries) => {
    const entry = entries[0];
    if (!entry || promoted) return;
    setPaneWidth(currentPaneId, entry.contentRect.width);
    onPresenceChange(currentPaneId, true);
  });
  obs.observe(node);
  publish();
  return {
    update(next: MeasurePaneOptions) {
      onPresenceChange = next.onPresenceChange;
      const wasPromoted = promoted;
      promoted = next.promoted ?? false;
      if (next.paneId === currentPaneId) {
        if (wasPromoted && !promoted) publish();
        return;
      }
      clearPaneWidth(currentPaneId);
      onPresenceChange(currentPaneId, false);
      currentPaneId = next.paneId;
      publish();
    },
    destroy() {
      obs.disconnect();
      clearPaneWidth(currentPaneId);
      onPresenceChange(currentPaneId, false);
    },
  };
}
