// Layout mode: `full` (the desktop pane strip) or `compact` (the phone
// shell). Compact is a LAYOUT MODE OF THE ONE APP, not a second app: the
// same components render, and only the shell around them changes
// (docs/specs/remote-access.md § The phone client). It is chosen from
// the viewport, never from the run mode or the device class, so a
// resized browser window and the Android shell reach the same layout by
// the same door, and Playwright's compact project is the same app under
// a phone viewport.
//
// The query needs BOTH terms: a narrow window on a desktop keeps the
// pane strip (a user dragging a window small is not asking for a phone),
// and a coarse pointer on a wide tablet keeps it too (tablets are a
// deliberate non-goal).
//
// The mode is stamped on <html> as `layout-compact` so stylesheets key
// on it through the `compact:` Tailwind variant (app.css), the way the
// theme mode class works. Compact carries one more piece of state, the
// SCREEN: the phone shows the thread list or the open thread, never
// both, and `revealPane` (the one door every "show me this pane" path
// already passes through) flips it to the thread. The screen is stamped
// beside the mode (`data-compact-screen`) because the two screens stay
// MOUNTED and swap visibility in CSS: unmounting the pane strip on every
// trip back to the list would reload the thread each time.

const COMPACT_QUERY = '(max-width: 640px) and (pointer: coarse)';

export type CompactScreen = 'list' | 'thread';

let compact = $state(false);
let screen: CompactScreen = $state('list');

export function isCompactLayout(): boolean {
  return compact;
}

export function getCompactScreen(): CompactScreen {
  return screen;
}

export function showCompactList(): void {
  screen = 'list';
  stamp();
}

export function showCompactThread(): void {
  screen = 'thread';
  stamp();
}

function stamp(): void {
  if (typeof document === 'undefined') return;
  const root = document.documentElement;
  root.classList.toggle('layout-compact', compact);
  if (compact) root.dataset.compactScreen = screen;
  else delete root.dataset.compactScreen;
}

/**
 * Subscribe the mode to the viewport. Called once from App.svelte;
 * returns the disposer.
 */
export function installLayoutMode(): () => void {
  if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return () => {};
  const mql = window.matchMedia(COMPACT_QUERY);
  const apply = (): void => {
    compact = mql.matches;
    stamp();
  };
  apply();
  mql.addEventListener('change', apply);
  return () => {
    mql.removeEventListener('change', apply);
    compact = false;
    stamp();
  };
}

/** Tests set the mode directly; the media query is not theirs to drive. */
export function setCompactLayoutForTest(next: boolean): void {
  compact = next;
  screen = 'list';
  stamp();
}
