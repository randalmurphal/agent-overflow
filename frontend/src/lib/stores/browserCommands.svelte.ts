// The browser pane's chrome commands: new tab, focus the address, reload,
// next and previous tab, reopen the last closed tab. Each acts on the browser pane's thread and
// is live only while that pane is the keyboard's (`browserPaneFocused`,
// panes.svelte#paneCommandTarget), so the
// chords keep their meaning elsewhere. A chord pressed in the native page
// arrives as a replay after the pane was focused (browserCompanion.svelte.ts
// replayBrowserAccelerator). Closing a tab is `pane.close`. A pane whose
// attach has not answered has nothing to send: its chords do nothing.

import { registerCommand } from './commandRegistry.svelte';
import { actInPane, focusedBrowser, keyboardTabTarget, openBrowserTabInPane } from './browserActions';
import { browserAction } from './browserActionWire';
import { focusBrowserAddressBar } from './browserAddressFocus.svelte';

export function isBrowserPaneFocused(): boolean {
  return focusedBrowser() !== null;
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

const WHEN = 'browserPaneFocused && !overlayOpen';

export function registerBrowserCommands(): void {
  registerCommand({
    id: 'browser.newTab',
    label: 'Browser: New Tab',
    description: 'Open a new tab in the focused browser pane and put the cursor in its address bar.',
    icon: '+',
    when: WHEN,
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
    when: WHEN,
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
    when: WHEN,
    editableReachable: true,
    run: () => stepTab(1),
  });

  registerCommand({
    id: 'browser.prevTab',
    label: 'Browser: Previous Tab',
    icon: '←',
    when: WHEN,
    editableReachable: true,
    run: () => stepTab(-1),
  });

  registerCommand({
    id: 'browser.reopenTab',
    label: 'Browser: Reopen Closed Tab',
    description: 'Reopen the tab most recently closed in the focused browser pane, where it was.',
    icon: '↺',
    when: WHEN,
    editableReachable: true,
    run: () => {
      const pane = focusedBrowser()?.pane;
      if (pane) void actInPane(pane, browserAction.reopen());
    },
  });
}
