// The Pull requests pane's commands (docs/specs/pull-requests.md, "The
// page"): the chord that opens or closes the pane, and the surface
// dismissal that steps from an open MR or pipeline back to the list, which
// is what Escape and the phone's Back run while the pane is focused with a
// detail open. The pane registers what "back" does for its active tab.

import { registerCommand } from './commandRegistry.svelte';
import { focusedToolKind, toggleTool } from './toolPanes.svelte';

let backHandler: (() => boolean) | null = null;

/**
 * The pane's "step back to the list". Returns whether it stepped. The
 * returned disposer restores the previous handler, so a pane that goes
 * while another is mounted leaves that one's handler in place.
 */
export function registerPullsBackHandler(handler: () => boolean): () => void {
  const previous = backHandler;
  backHandler = handler;
  return () => {
    if (backHandler === handler) backHandler = previous;
  };
}

export function isPullsPaneFocused(): boolean {
  return focusedToolKind() === 'pulls';
}

/** Runs the pane's back step; false when no pane is mounted or nothing was open. */
export function stepPullsBack(): boolean {
  return backHandler?.() ?? false;
}

export function registerPullsCommands(): void {
  registerCommand({
    id: 'pulls.toggle',
    label: 'Pull requests: Open',
    description: 'Open or reveal the Pull requests pane. Pressing the same chord while it is focused closes it.',
    icon: '⇄',
    run: () => toggleTool('pulls'),
  });

  // Gated on `toolDetailOpen` as well as focus, like workflows.escape: a
  // surface dismissal that passed at the list would consume the phone's
  // Back without doing anything, and Back could never leave the pane.
  registerCommand({
    id: 'pulls.escape',
    dismissesSurface: true,
    label: 'Pull requests: Back to the list',
    description: 'Close the open merge request or pipeline and return to the list.',
    when: 'pullsPaneFocused && toolDetailOpen',
    run: () => {
      stepPullsBack();
    },
  });
}

export function resetPullsCommandsForTest(): void {
  backHandler = null;
}
