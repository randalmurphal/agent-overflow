// The shell's lifecycle, wired to the two doors the SPA already has.
//
// Two signals, and neither of them is new behaviour invented here:
//
//   - **Pause / resume** is the ONLY visibility signal this client ever
//     sends, and it goes through `transport/lease.ts` — the door wave
//     6f-a shipped ahead of its caller precisely so the capability could
//     not be wired by reaching past the seam. It is emphatically not
//     `document.visibilityState`: a hidden tab, a minimised window and an
//     off-screen pane all stay `active`, because off-view work shedding
//     is a rejected design in this codebase and the one case the frame
//     exists for is the platform having stopped running the app.
//   - **The hardware back button** is Android's spelling of "one step
//     back", and `answerBackPress` is the whole stack, in order: an open
//     overlay or sheet closes first; a terminal drawer stacked over the
//     chat closes; a companion screen (review, browser, plan) closes and
//     the thread it was opened from comes back; the thread screen goes
//     back to the list (`stores/layoutMode`'s `showCompactList`); and the
//     list screen is the root, where the platform's own answer is to
//     leave the app. The first real-phone session (2026-09-04) had only
//     the last two rungs, so back from the review pane skipped the thread
//     it came from and landed on the list.
//
// **The overlay case dispatches Escape rather than reaching into
// components.** Every overlay, sheet, popover and dialog in this app
// already closes on Escape through the keybinding path, and there are
// enough of them that a registry here would be a second list to keep in
// sync — one that is wrong the first time somebody adds a sheet. So the
// button synthesises the key the platform's own keyboard would have sent,
// and whether anything consumed it is read off `defaultPrevented`, which
// is the same answer the browser gives any other key handler. The one
// target it avoids is a focused terminal: xterm turns Escape into an ESC
// byte for the shell and reports the key consumed, which is a keystroke
// nobody pressed. From there the key goes to the pane instead, where the
// same window-level handlers still see it.

import { closeCompanion, getCompanionPane } from '../stores/companionPanes.svelte';
import { getCompactScreen, isCompactLayout, showCompactList } from '../stores/layoutMode.svelte';
import { getFocusedPaneOrNull, revealPane } from '../stores/panes.svelte';
import { setClientLease } from '../transport/lease';
import { runTerminalToggle } from '../components/terminal/terminalToggle';
import { appPlugin } from './plugins';
import { isNativeShell } from './platform';

/**
 * Ask the page to close whatever is on top, and report whether anything
 * did. A synthetic Escape at the active element, so it walks exactly the
 * path a real key press walks.
 */
function dismissTopSurface(): boolean {
  if (typeof document === 'undefined') return false;
  const active = document.activeElement;
  const target = active?.closest('.xterm')
    ? (active.closest('[data-pane-id]') ?? document.body)
    : (active ?? document.body);
  if (!target) return false;
  const event = new KeyboardEvent('keydown', {
    key: 'Escape',
    code: 'Escape',
    bubbles: true,
    cancelable: true,
  });
  target.dispatchEvent(event);
  return event.defaultPrevented;
}

/**
 * The companion pane the thread screen is showing, if that is what is on
 * screen. Compact lays every pane out at the strip's full width and glides
 * between them, so "which pane" is the one under the strip's centre —
 * read from geometry rather than focus, because opening a companion
 * deliberately leaves focus on the thread it was opened from.
 */
function onScreenCompanion(): { paneId: string; sourcePaneId: string } | null {
  if (typeof document === 'undefined') return null;
  const strip = document.querySelector('.compact-screen-thread');
  if (!strip) return null;
  const stripRect = strip.getBoundingClientRect();
  const centre = stripRect.left + stripRect.width / 2;
  for (const section of strip.querySelectorAll<HTMLElement>('[data-pane-id]')) {
    const rect = section.getBoundingClientRect();
    if (rect.left > centre || rect.right <= centre) continue;
    if (section.dataset.paneKind === 'thread') return null;
    const paneId = section.dataset.paneId ?? '';
    const companion = getCompanionPane(paneId);
    return companion ? { paneId, sourcePaneId: companion.sourcePaneId } : null;
  }
  return null;
}

/**
 * One step back. Answers whether the press was absorbed by the page; a
 * `false` means the list screen is showing and the platform should leave
 * the app. Each rung answers for itself, so a press never does two things.
 */
export function answerBackPress(): boolean {
  if (dismissTopSurface()) return true;
  if (!isCompactLayout() || getCompactScreen() !== 'thread') return false;
  const focused = getFocusedPaneOrNull();
  if (focused?.showTerminal) {
    runTerminalToggle(focused);
    return true;
  }
  const companion = onScreenCompanion();
  if (companion) {
    closeCompanion(companion.paneId);
    revealPane(companion.sourcePaneId);
    return true;
  }
  showCompactList();
  return true;
}

/**
 * Subscribe the shell's lifecycle. No-op off the shell, which is what
 * makes this safe to call unconditionally from `main.ts`. Answers a
 * disposer; nothing in the app calls it today, and it exists so a test
 * can install and remove the subscription without leaking a listener into
 * the next case.
 */
export async function installNativeLifecycle(): Promise<() => void> {
  if (!isNativeShell()) return () => {};
  const app = await appPlugin();
  if (!app) return () => {};

  const handles = await Promise.all([
    app.addListener('pause', () => setClientLease('background')),
    app.addListener('resume', () => setClientLease('active')),
    app.addListener('backButton', () => {
      if (!answerBackPress()) void app.exitApp();
    }),
  ]);

  return () => {
    for (const handle of handles) void handle.remove();
  };
}
