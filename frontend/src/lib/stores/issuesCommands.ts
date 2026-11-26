// The Issues pane's commands: open or reveal the pane, or close it when it
// is focused, while this window may use the pane (`trackerRead`), with no
// default chord; and Escape (and the phone's Back) out of an open ticket,
// in the Issues pane or in a ticket's own pane, which the pane, loaded on
// demand, hands its step to.

import { registerCommand } from './commandRegistry.svelte';
import { paneLayoutKindOf } from './paneLayout.svelte';
import { getFocusedPaneId } from './panes.svelte';
import { focusedToolKind, toggleTool } from './toolPanes.svelte';

// Each pane's step back, by layout pane id.
const backHandlers = new Map<string, () => void>();

/**
 * The step back out of the ticket the pane `paneId` shows. The returned
 * disposer restores the handler it replaced, so a body that remounts while
 * the old one is still mounted leaves that one's handler in place.
 */
export function registerIssuesBackHandler(paneId: string, handler: () => void): () => void {
  const previous = backHandlers.get(paneId);
  backHandlers.set(paneId, handler);
  return () => {
    if (backHandlers.get(paneId) !== handler) return;
    if (previous) backHandlers.set(paneId, previous);
    else backHandlers.delete(paneId);
  };
}

/** The focused pane is the Issues pane or a ticket's own pane. */
export function isIssuesPaneFocused(): boolean {
  if (focusedToolKind() === 'issues') return true;
  const paneId = getFocusedPaneId();
  return paneId !== null && paneLayoutKindOf(paneId) === 'ticket';
}

export function registerIssuesCommands(): void {
  registerCommand({
    id: 'issues.toggle',
    label: 'Issues: Open',
    description: 'Open or reveal the Issues pane. Running it while the pane is focused closes it.',
    icon: '▦',
    when: 'trackerRead',
    run: () => toggleTool('issues'),
  });

  // Gated on `toolDetailOpen` as well as focus, as pulls.escape is: in the
  // Issues pane a ticket is open, so Back leaves the pane once none is; in
  // a ticket's own pane its Details rail or Back trail is, so Back never
  // closes that pane.
  registerCommand({
    id: 'issues.escape',
    dismissesSurface: true,
    label: 'Issues: Back',
    description: 'Close the details over a narrow ticket, else go back to the ticket this one was opened from; in the Issues pane, else close the ticket and show the board.',
    when: 'issuesPaneFocused && toolDetailOpen && !overlayOpen && !paneFindOpen',
    run: () => {
      const paneId = getFocusedPaneId();
      if (paneId) backHandlers.get(paneId)?.();
    },
  });
}

export function resetIssuesCommandsForTest(): void {
  backHandlers.clear();
}
