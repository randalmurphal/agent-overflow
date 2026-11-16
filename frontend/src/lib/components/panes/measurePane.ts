// Svelte action that publishes a pane section's measured width to the
// shared layoutMetrics store and notifies the caller of offsetLeft changes.
// Used by PaneHost to keep both the global per-pane width registry and the
// host-local offset map in sync as panes mount, swap paneId, or unmount.
// A promoted pane's section is laid over the strip while its slot keeps the
// strip's layout, so while `promoted` the section's own size is not its
// slot width and is not published; the next update after demote publishes
// the slot again.

import { clearPaneWidth, setPaneWidth } from '../../stores/layoutMetrics.svelte';

export interface MeasurePaneOptions {
  paneId: string;
  // Receives the latest offsetLeft on mount, on resize, and on paneId swap.
  // null is fired when the pane unmounts or its paneId moves away so the
  // caller can drop the stale entry from any offset cache it keeps.
  onOffsetChange(paneId: string, offsetLeft: number | null): void;
  promoted?: boolean;
}

export function measurePane(node: HTMLElement, options: MeasurePaneOptions) {
  let currentPaneId = options.paneId;
  let onOffsetChange = options.onOffsetChange;
  let promoted = options.promoted ?? false;

  function publish(): void {
    if (promoted) return;
    setPaneWidth(currentPaneId, node.getBoundingClientRect().width);
    onOffsetChange(currentPaneId, node.offsetLeft);
  }

  const obs = new ResizeObserver((entries) => {
    const entry = entries[0];
    if (!entry || promoted) return;
    setPaneWidth(currentPaneId, entry.contentRect.width);
    onOffsetChange(currentPaneId, node.offsetLeft);
  });
  obs.observe(node);
  publish();
  return {
    update(next: MeasurePaneOptions) {
      onOffsetChange = next.onOffsetChange;
      const wasPromoted = promoted;
      promoted = next.promoted ?? false;
      if (next.paneId === currentPaneId) {
        if (wasPromoted && !promoted) publish();
        return;
      }
      clearPaneWidth(currentPaneId);
      onOffsetChange(currentPaneId, null);
      currentPaneId = next.paneId;
      publish();
    },
    destroy() {
      obs.disconnect();
      clearPaneWidth(currentPaneId);
      onOffsetChange(currentPaneId, null);
    },
  };
}
