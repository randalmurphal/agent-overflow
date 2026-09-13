// Browser Back for the compact layout, so a phone-sized browser pointed at
// a remote host answers a swipe-back or the Back button the way the
// Android shell answers its hardware key (`native/lifecycle.ts`).
//
// The app has one URL, so the browser's history has nothing of the app's
// in it: Back leaves the page from every screen. This keeps exactly ONE
// sentinel entry pushed whenever there is somewhere to go back to inside
// the app: the thread screen, or any dismissable surface (the settings or
// workflows overlay, a companion, a modal, a popover, a context menu).
// A Back over the sentinel lands on the page's own entry and raises
// `popstate`; the handler runs the same `stepBack` ladder the shell runs,
// and the effect below re-pushes the sentinel if the app is still away
// from root. At root there is no sentinel, so Back leaves the page, which
// is what the shell does too.
//
// "Away from root" reads the sources the ladder itself consults: the
// layout store's screen and the airspace registry, which every overlay
// primitive registers its painted root with. There is no second list of
// surfaces to keep in sync. The URL scrubs elsewhere use `replaceState`
// on whatever entry is current and never touch the count, and
// `browserHistoryGuard` suppresses desktop history KEYS, which never
// raise `popstate`; neither is disturbed.
//
// Off compact nothing is pushed. A sentinel left behind by a resize out
// of compact is harmless: the handler ignores a pop off compact, and the
// effect drops the entry the next time compact returns at root.

import { stepBack } from './stepBack';
import { isNativeShell } from '../native/platform';
import { getCompactScreen, isCompactLayout } from '../stores/layoutMode.svelte';
import { airspaceSurfaces } from './paneAirspace.svelte';

const SENTINEL_KEY = 'aoBack';

export function isCompactBackSentinel(state: unknown): boolean {
  return (
    typeof state === 'object' &&
    state !== null &&
    (state as Record<string, unknown>)[SENTINEL_KEY] === true
  );
}

/** Whether one Back has somewhere to go inside the app. */
function awayFromRoot(): boolean {
  if (!isCompactLayout()) return false;
  return getCompactScreen() === 'thread' || airspaceSurfaces().length > 0;
}

/**
 * Install once from App.svelte. A no-op in the native shell, which has the
 * hardware key, and off the DOM. Answers the disposer.
 */
export function installCompactHistoryBack(): () => void {
  if (typeof window === 'undefined' || isNativeShell()) return () => {};

  // Whether the current history entry is our sentinel. Reactive so the
  // effect re-arms after a pop consumed it. Seeded from the entry a reload
  // landed on: a stale sentinel at root is dropped by the effect below.
  let armed = $state(isCompactBackSentinel(window.history.state));
  // Pops this module asked for (`history.back()` to drop a sentinel the
  // app no longer needs) and must not answer with the ladder.
  let ownPopsPending = 0;

  const onPopState = (event: PopStateEvent): void => {
    const wasArmed = armed;
    armed = isCompactBackSentinel(event.state);
    if (ownPopsPending > 0) {
      ownPopsPending -= 1;
      return;
    }
    // Only a Back over our sentinel is ours. Forward onto it re-arms and
    // nothing else happens.
    if (!wasArmed || armed || !isCompactLayout()) return;
    // One step. The effect re-pushes the sentinel if the app is still away
    // from root once the dismissed surface has left the registry; at root
    // it stays down and the next Back leaves the page.
    stepBack();
  };
  window.addEventListener('popstate', onPopState);

  const dispose = $effect.root(() => {
    $effect(() => {
      const away = awayFromRoot();
      if (away && !armed) {
        window.history.pushState({ [SENTINEL_KEY]: true }, '');
        armed = true;
      } else if (!away && armed && isCompactLayout()) {
        // The app returned to root by its own hand (the header's back
        // button, a tap outside a menu). Drop the sentinel so the next
        // Back leaves the page rather than needing a second press.
        armed = false;
        ownPopsPending += 1;
        window.history.back();
      }
    });
  });

  return () => {
    window.removeEventListener('popstate', onPopState);
    dispose();
  };
}
