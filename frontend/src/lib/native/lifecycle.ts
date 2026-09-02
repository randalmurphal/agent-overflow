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
//   - **The hardware back button** is Android's spelling of the gesture
//     the compact layout already answers. On the thread screen it means
//     "back to the list" (`stores/layoutMode`'s `showCompactList`), on
//     the list screen it means "leave the app", and an open overlay or
//     sheet closes first.
//
// **The overlay case dispatches Escape rather than reaching into
// components.** Every overlay, sheet, popover and dialog in this app
// already closes on Escape through the keybinding path, and there are
// enough of them that a registry here would be a second list to keep in
// sync — one that is wrong the first time somebody adds a sheet. So the
// button synthesises the key the platform's own keyboard would have sent,
// and whether anything consumed it is read off `defaultPrevented`, which
// is the same answer the browser gives any other key handler.

import { getCompactScreen, isCompactLayout, showCompactList } from '../stores/layoutMode.svelte';
import { setClientLease } from '../transport/lease';
import { appPlugin } from './plugins';
import { isNativeShell } from './platform';

/**
 * Ask the page to close whatever is on top, and report whether anything
 * did. A synthetic Escape at the active element, so it walks exactly the
 * path a real key press walks.
 */
function dismissTopSurface(): boolean {
  if (typeof document === 'undefined') return false;
  const target = document.activeElement ?? document.body;
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
      // In order, and each step answers for itself: a sheet that was open
      // is what the press was about; a thread screen goes back to the
      // list; a list screen is the root, and the platform's own answer
      // for "back from the root" is to leave.
      if (dismissTopSurface()) return;
      if (isCompactLayout() && getCompactScreen() === 'thread') {
        showCompactList();
        return;
      }
      void app.exitApp();
    }),
  ]);

  return () => {
    for (const handle of handles) void handle.remove();
  };
}

/** Exported for the unit test: the Escape dispatch, without a plugin. */
export const __dismissTopSurfaceForTest = dismissTopSurface;
