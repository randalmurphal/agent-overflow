// The browser pane's chrome commands: new tab, focus the address, reload,
// next and previous tab. Each acts on the focused browser pane's thread and
// is live only while that pane is focused (`browserPaneFocused`), so the
// chords keep their meaning elsewhere. A chord pressed in the native page
// arrives as a replay after the pane was focused (browserCompanion.svelte.ts
// replayBrowserAccelerator). Closing a tab is `pane.close`.

import { registerCommand } from './commandRegistry.svelte';
import {
  browserCompanionAct,
  focusBrowserAddressBar,
  focusedBrowser,
  openBrowserTab,
} from './browserCompanion.svelte';

export function isBrowserPaneFocused(): boolean {
  return focusedBrowser() !== null;
}

/** Shows the tab `step` places from the active one, wrapping. */
function stepTab(step: number): void {
  const focused = focusedBrowser();
  const pages = focused?.state?.pages ?? [];
  if (!focused || pages.length < 2) return;
  const at = Math.max(0, pages.findIndex((page) => page.id === focused.state?.activePageId));
  const next = pages[(at + step + pages.length) % pages.length];
  void browserCompanionAct(focused.threadId, { kind: 'activate', pageId: next.id });
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
      const focused = focusedBrowser();
      if (focused) void openBrowserTab(focused.threadId, { focusAddress: true });
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
    run: () => {
      const focused = focusedBrowser();
      const pageId = focused?.state?.activePageId;
      if (focused && pageId) void browserCompanionAct(focused.threadId, { kind: 'reload', pageId });
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
}
