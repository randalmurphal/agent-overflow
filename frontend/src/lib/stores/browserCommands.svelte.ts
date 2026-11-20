// The browser pane's chrome commands: new tab, focus the address, reload,
// next and previous tab, reopen the last closed tab. Each acts on the browser pane's thread and
// is live only while that pane is the keyboard's (`browserPaneFocused`,
// panes.svelte#paneCommandTarget), so the
// chords keep their meaning elsewhere. A chord pressed in the native page
// arrives as a replay after the pane was focused (browserCompanion.svelte.ts
// replayBrowserAccelerator). Closing a tab is `pane.close`. A pane whose
// attach has not answered has nothing to send, so its commands that send
// are not available (`browserPaneUnattached`): the palette does not offer
// them and running one reports that nothing ran. Its chords stay the pane's
// (browserPaneReservesChord), so a chord pressed before the attach answers
// does nothing rather than what it means elsewhere.

import { registerCommand, type CommandContext } from './commandRegistry.svelte';
import { keyWouldDispatch } from './keybindings.svelte';
import { actInPane, focusedBrowser, keyboardTabTarget, openBrowserTabInPane } from './browserActions';
import { browserAction } from './browserActionWire';
import { focusBrowserAddressBar } from './browserAddressFocus.svelte';

export function isBrowserPaneFocused(): boolean {
  return focusedBrowser() !== null;
}

/**
 * Whether the keyboard's browser pane owns `event`'s chord in `context`: a
 * command runs for it there once the pane is attached, whether or not it is
 * yet. The chord is then the pane's to run or to do nothing with, never one
 * to hand on (the native shell's reload, shellChords.svelte.ts). Reactive.
 */
export function browserPaneReservesChord(event: KeyboardEvent, context: CommandContext): boolean {
  return isBrowserPaneFocused() && keyWouldDispatch(event, { ...context, flags: { ...context.flags, browserPaneUnattached: false } });
}

/**
 * Shows the tab `step` places from the active one, wrapping. The backend
 * steps from its own order and selection, so presses sent before the
 * previous one's state arrives each move one tab.
 */
function stepTab(step: 1 | -1): void {
  const pane = focusedBrowser()?.pane;
  if (pane) void actInPane(pane, browserAction.step(step));
}

// Focusing the address sends nothing, so it is offered attached or not; the
// rest send from the pane's attachment and need one.
const WHEN = 'browserPaneFocused && !overlayOpen';
const SEND_WHEN = 'browserPaneFocused && !browserPaneUnattached && !overlayOpen';

export function registerBrowserCommands(): void {
  registerCommand({
    id: 'browser.newTab',
    label: 'Browser: New Tab',
    description: 'Open a new tab in the focused browser pane and put the cursor in its address bar.',
    icon: '+',
    when: SEND_WHEN,
    editableReachable: true,
    run: () => {
      const pane = focusedBrowser()?.pane;
      if (pane) void openBrowserTabInPane(pane, { focusAddress: true });
    },
  });

  registerCommand({
    id: 'browser.focusAddress',
    label: 'Browser: Focus Address Bar',
    icon: '⌖',
    when: WHEN,
    editableReachable: true,
    run: () => {
      const focused = focusedBrowser();
      if (focused) focusBrowserAddressBar(focused.threadId);
    },
  });

  registerCommand({
    id: 'browser.reload',
    label: 'Browser: Reload Page',
    icon: '↻',
    when: SEND_WHEN,
    editableReachable: true,
    // Names the tab the window shows, or the page the chord was pressed
    // in, so a tab switch still on its way never reloads another tab.
    run: () => {
      const focused = focusedBrowser();
      const pageId = focused ? keyboardTabTarget(focused) : null;
      if (focused?.pane && pageId) void actInPane(focused.pane, browserAction.onTab('reload', pageId));
    },
  });

  registerCommand({
    id: 'browser.nextTab',
    label: 'Browser: Next Tab',
    icon: '→',
    when: SEND_WHEN,
    editableReachable: true,
    run: () => stepTab(1),
  });

  registerCommand({
    id: 'browser.prevTab',
    label: 'Browser: Previous Tab',
    icon: '←',
    when: SEND_WHEN,
    editableReachable: true,
    run: () => stepTab(-1),
  });

  registerCommand({
    id: 'browser.reopenTab',
    label: 'Browser: Reopen Closed Tab',
    description: 'Reopen the tab most recently closed in the focused browser pane, where it was.',
    icon: '↺',
    when: SEND_WHEN,
    editableReachable: true,
    run: () => {
      const pane = focusedBrowser()?.pane;
      if (pane) void actInPane(pane, browserAction.reopen());
    },
  });
}
