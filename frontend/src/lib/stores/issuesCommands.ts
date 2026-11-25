// The Issues pane's commands: open or reveal the pane, or close it when it
// is focused, while this window may use the pane (`trackerRead`), with no
// default chord; and Escape (and the phone's Back) out of an open ticket,
// which the pane, loaded on demand, hands its step to.

import { registerCommand } from './commandRegistry.svelte';
import { focusedToolKind, toggleTool } from './toolPanes.svelte';

let backHandler: (() => boolean) | null = null;

/**
 * The pane's step back out of its open ticket. Returns whether it stepped.
 * The returned disposer restores the previous handler, so a pane that goes
 * while another is mounted leaves that one's handler in place.
 */
export function registerIssuesBackHandler(handler: () => boolean): () => void {
  const previous = backHandler;
  backHandler = handler;
  return () => {
    if (backHandler === handler) backHandler = previous;
  };
}

export function isIssuesPaneFocused(): boolean {
  return focusedToolKind() === 'issues';
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

  // Gated on `toolDetailOpen` (a ticket is open) as well as focus, as
  // pulls.escape is, so Back leaves the pane once no ticket is open.
  registerCommand({
    id: 'issues.escape',
    dismissesSurface: true,
    label: 'Issues: Back',
    description: 'Close the details over a narrow ticket, else go back to the ticket this one was opened from, else close it and show the board.',
    when: 'issuesPaneFocused && toolDetailOpen && !overlayOpen && !paneFindOpen',
    run: () => {
      backHandler?.();
    },
  });
}

export function resetIssuesCommandsForTest(): void {
  backHandler = null;
}
