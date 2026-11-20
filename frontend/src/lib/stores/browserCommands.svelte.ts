// The browser pane's chrome commands: new tab, focus the address, reload,
// next and previous tab, reopen the last closed tab. Each acts on the browser pane's thread and
// is live only while that pane is the keyboard's (`browserPaneFocused`,
// panes.svelte#paneCommandTarget), so the
// chords keep their meaning elsewhere. A chord pressed in the native page
// arrives as a replay after the pane was focused (browserPageChords.ts
// replayBrowserAccelerator). Closing a tab is `pane.close`. AVAILABLE_WHEN
// says, per command, when it runs and when the palette offers it; its
// chords stay the pane's (browserPaneReservesChord), so a chord pressed
// while its command cannot run does nothing rather than what it means
// elsewhere.

import { registerCommand, type CommandContext } from './commandRegistry.svelte';
import { keyWouldDispatch } from './keybindings.svelte';
import { actInPane, focusedBrowser, keyboardTabTarget, openBrowserTabInPane, tabTakesAddress, type FocusedBrowser } from './browserActions';
import { browserAction, type PageId } from './browserActionWire';
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
  const available = { ...context.flags, browserPaneUnattached: false, browserAddressUnavailable: false, browserNoClosedTab: false };
  return isBrowserPaneFocused() && keyWouldDispatch(event, { ...context, flags: available });
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

// When each command runs (`when`) and when the palette offers it
// (`offered`, which the palette requires besides `when`). Every one needs
// the pane to be the keyboard's with nothing above it. The two differ:
// the palette shows what the window's state says the command would do for
// the tab shown; a command the user issued is decided for the target their
// gesture named, and never refused on the strength of a state that the
// commands already queued in the pane mount's order will change. A
// numbered command whose precondition an earlier queued one can create is
// sent, and the backend's answer at its turn decides.
const PANE = 'browserPaneFocused && !overlayOpen';
const AVAILABLE_WHEN = {
  // Sends from the pane's attachment.
  'browser.newTab': { when: `${PANE} && !browserPaneUnattached`, offered: '' },
  // Focuses the field, which takes focus only once the attach answered (the
  // chrome is inert until then) and while the shown tab takes an address:
  // nothing is sent, so the field's state now is the answer for both.
  'browser.focusAddress': { when: `${PANE} && !browserAddressUnavailable`, offered: '' },
  // Sends a reload of the tab the gesture named (keyboardTabTarget), which
  // runs only when that tab takes an address (reloadTarget); the palette
  // offers it for the shown tab.
  'browser.reload': { when: `${PANE} && !browserPaneUnattached`, offered: '!browserAddressUnavailable' },
  // Send a step from the pane's attachment.
  'browser.nextTab': { when: `${PANE} && !browserPaneUnattached`, offered: '' },
  'browser.prevTab': { when: `${PANE} && !browserPaneUnattached`, offered: '' },
  // Sends from the pane's attachment, numbered: a close queued before it
  // may make the closed tab it reopens, so only the palette reads the
  // closed tabs the state shows.
  'browser.reopenTab': { when: `${PANE} && !browserPaneUnattached`, offered: '!browserNoClosedTab' },
} as const;

/**
 * The tab a reload from the keyboard acts on: the tab the gesture named
 * (keyboardTabTarget), when it takes an address now. Null when it names
 * none or that tab cannot reload yet.
 */
function reloadTarget(focused: FocusedBrowser): PageId | null {
  const pageId = keyboardTabTarget(focused);
  const page = focused.state?.pages?.find((tab) => tab.id === pageId) ?? null;
  return pageId && tabTakesAddress(page) ? pageId : null;
}

function available(id: keyof typeof AVAILABLE_WHEN): { when: string; offeredWhen?: string } {
  const { when, offered } = AVAILABLE_WHEN[id];
  return offered ? { when, offeredWhen: offered } : { when };
}

export function registerBrowserCommands(): void {
  registerCommand({
    id: 'browser.newTab',
    label: 'Browser: New Tab',
    description: 'Open a new tab in the focused browser pane and put the cursor in its address bar.',
    icon: '+',
    ...available('browser.newTab'),
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
    ...available('browser.focusAddress'),
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
    ...available('browser.reload'),
    editableReachable: true,
    // Names the tab the window shows, or the page the chord was pressed
    // in, so a tab switch still on its way never reloads another tab.
    run: () => {
      const focused = focusedBrowser();
      const pageId = focused ? reloadTarget(focused) : null;
      if (focused?.pane && pageId) void actInPane(focused.pane, browserAction.onTab('reload', pageId));
    },
  });

  registerCommand({
    id: 'browser.nextTab',
    label: 'Browser: Next Tab',
    icon: '→',
    ...available('browser.nextTab'),
    editableReachable: true,
    run: () => stepTab(1),
  });

  registerCommand({
    id: 'browser.prevTab',
    label: 'Browser: Previous Tab',
    icon: '←',
    ...available('browser.prevTab'),
    editableReachable: true,
    run: () => stepTab(-1),
  });

  registerCommand({
    id: 'browser.reopenTab',
    label: 'Browser: Reopen Closed Tab',
    description: 'Reopen the tab most recently closed in the focused browser pane, where it was.',
    icon: '↺',
    ...available('browser.reopenTab'),
    editableReachable: true,
    run: () => {
      const pane = focusedBrowser()?.pane;
      if (pane) void actInPane(pane, browserAction.reopen());
    },
  });
}
