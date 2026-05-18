// Svelte action that publishes a pane section's measured width to the
// shared layoutMetrics store and notifies the caller of offsetLeft changes.
// Used by PaneHost to keep both the global per-pane width registry and the
// host-local offset map in sync as panes mount, swap paneId, or unmount.

import { clearPaneWidth, setPaneWidth } from '../../stores/layoutMetrics.svelte';

export interface MeasurePaneOptions {
  paneId: string;
  // Receives the latest offsetLeft on mount, on resize, and on paneId swap.
  // null is fired when the pane unmounts or its paneId moves away so the
  // caller can drop the stale entry from any offset cache it keeps.
  onOffsetChange(paneId: string, offsetLeft: number | null): void;
}

export function measurePane(node: HTMLElement, options: MeasurePaneOptions) {
  let currentPaneId = options.paneId;
  let onOffsetChange = options.onOffsetChange;

  function publish(): void {
    setPaneWidth(currentPaneId, node.getBoundingClientRect().width);
    onOffsetChange(currentPaneId, node.offsetLeft);
  }

  const obs = new ResizeObserver((entries) => {
    const entry = entries[0];
    if (!entry) return;
    setPaneWidth(currentPaneId, entry.contentRect.width);
    onOffsetChange(currentPaneId, node.offsetLeft);
  });
  obs.observe(node);
  publish();
  return {
    update(next: MeasurePaneOptions) {
      onOffsetChange = next.onOffsetChange;
      if (next.paneId === currentPaneId) return;
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
