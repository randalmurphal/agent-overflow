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
//     back", and `utils/stepBack.ts` is the whole stack (it also answers
//     a browser's Back in a phone-sized browser through
//     `utils/compactHistoryBack.svelte.ts`). `answerBackPress` wraps the
//     stack for the shell: while the app lock is up the press is
//     absorbed, because `inert` on the app root stops pointers and focus
//     but not store calls, and a Back that navigated behind the cover
//     would leak the next screen when it came down.

import { setClientLease } from '../transport/lease';
import { appPlugin } from './plugins';
import { isNativeShell } from './platform';
import { isAppLocked } from './lock';
import { stepBack } from '../utils/stepBack';

/**
 * The shell's hardware back press. Answers whether the press was absorbed
 * by the page; a `false` means the platform should leave the app. While
 * the lock cover is up every press is absorbed and nothing moves.
 */
export function answerBackPress(): boolean {
  if (isAppLocked()) return true;
  return stepBack();
}

/**
 * Subscribe the shell's lifecycle. No-op off the shell, which is what
 * makes this safe to call unconditionally; `native/boot.ts` awaits it once
 * per document. Answers a disposer that the app itself never calls — it
 * exists so a test can install and remove the subscription without leaking
 * a listener into the next case.
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
