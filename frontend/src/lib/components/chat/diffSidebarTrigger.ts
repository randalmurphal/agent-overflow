// Helper to route a payload-id (and optional file path) into the
// per-tool diff sidebar. Centralized so DiffPreview, ToolResultCard
// chips, and any future inline trigger share one entry point — the
// pane setter takes care of mutex (closes Plan + checkpoint drawer).
//
// Cmd/Ctrl+click on the inline DiffPreview header also routes here,
// so the same path covers both the explicit icon button and the
// modifier-click affordance.

import type { ThreadPane } from '../../stores/thread.svelte';

export interface OpenDiffSidebarOpts {
  payloadId: string;
  filePath?: string;
}

export function openDiffSidebar(pane: ThreadPane, opts: OpenDiffSidebarOpts): void {
  pane.openDiffSidebar({ payloadId: opts.payloadId, filePath: opts.filePath });
}

/**
 * True when the click event carries a "promote to sidebar" modifier
 * (Cmd on macOS, Ctrl elsewhere). Used by inline diff headers so a
 * plain click expands inline and a modifier-click opens the sidebar.
 */
export function isPromoteModifier(event: MouseEvent | KeyboardEvent): boolean {
  return event.metaKey || event.ctrlKey;
}
