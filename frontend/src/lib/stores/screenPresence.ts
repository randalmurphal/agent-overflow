// Tells every attached backend whether THIS screen is being looked at, and
// which threads it is showing (transport/frames.ts ClientPresenceFrame).
//
// ONE CONSUMER, ONE DECISION. The backend reads it only to decide whether to
// RAISE an OS notification it was about to raise — "no toast about a turn you
// watched finish" (internal/app/app_notifications.go screenIsAlreadyLooking).
// It never changes what this client is sent, what any surface renders, or
// what work the backend does. Off-view work shedding is a rejected design in
// this codebase; the alternative to a toast is no toast, not a stale pane.
// Nothing else in the app may read this module, and nothing here may be
// wired into rendering, subscription, or fetching.
//
// THE OPPOSITE RULE TO ./watchedThreads.ts, deliberately, and they must not
// be confused. That one is EXISTENCE and never visibility, because a pane
// that stopped receiving renders wrongly the moment it is looked at. This one
// IS visibility, because the question it answers is literally "is somebody
// looking". Neither may borrow the other's inputs.
//
// THE APPROXIMATION, stated rather than hidden: a desktop pane scrolled off
// the horizontal strip still counts as on screen. Resolving that needs an
// IntersectionObserver per pane, which is per-frame work on every scroll for
// a notification the person will see the moment they scroll back — a bad
// trade in both directions. Compact is exact, because the strip is one pane
// wide there: the revealed pane is the focused one.
//
// Rune-free and a leaf, like ./watchedThreads.ts: it reads the pane and
// layout accessors and hands the answer to transport/backends.ts, which owns
// "every attached backend, and every one attached afterwards".
import { setPresenceEverywhere } from '../transport/backends';
import { getCompactScreen, isCompactLayout } from './layoutMode.svelte';
import { getFocusedPaneOrNull, openThreadIds } from './panes.svelte';

interface ScreenPresence {
  focused: boolean;
  threads: string[];
}

function threadsOnScreen(): string[] {
  if (isCompactLayout()) {
    // The phone shows the list or one thread, never both, and the revealed
    // pane is the focused one — so this branch is exact where the desktop's
    // is an approximation.
    if (getCompactScreen() !== 'thread') return [];
    const threadId = getFocusedPaneOrNull()?.threadId;
    return threadId ? [threadId] : [];
  }
  return [...openThreadIds()];
}

// The pane read comes FIRST and unconditionally, and that ordering is
// load-bearing rather than stylistic: App.svelte drives this from a `$effect`,
// which re-runs only on the reactive state a run actually read. Short-circuiting
// on `document.hidden` before reading the panes would leave a run that
// collected no dependencies, and the effect would then never fire again for a
// pane opened after the tab came back.
function composePresence(): ScreenPresence {
  if (typeof document === 'undefined') return { focused: false, threads: [] };
  const threads = threadsOnScreen();
  // A hidden document is a screen showing nothing at all — another tab, or a
  // minimised window. Both halves go with it, so a backgrounded tab does not
  // keep claiming its panes are on screen.
  if (document.hidden) return { focused: false, threads: [] };
  // Focus is the app being in front, which is what "quiet while I am already
  // looking" means. `hasFocus` answers false for a visible window sitting
  // behind another app, which is exactly the case the second rule is for.
  return { focused: document.hasFocus(), threads };
}

/**
 * Recompute this screen's presence and state it.
 *
 * The transport dedups, so calling this after any composition change costs a
 * small array build and nothing on the wire. App.svelte drives it from the
 * pane and layout state it already tracks; this module drives it from the
 * document's own focus and visibility events.
 */
export function refreshScreenPresence(): void {
  const { focused, threads } = composePresence();
  setPresenceEverywhere(focused, threads);
}

/**
 * Attach the document-level half of the signal and state it once. Installed
 * exactly once, from App.svelte, which owns the disposer.
 *
 * Three listeners and no polling: focus and blur are the alt-tab edges, and
 * visibilitychange is the tab-switch and minimise edge. None of them fires
 * per frame, and each one ends in the transport's dedup.
 */
export function installScreenPresence(): () => void {
  if (typeof window === 'undefined' || typeof document === 'undefined') {
    return () => {};
  }
  const onChange = (): void => refreshScreenPresence();
  window.addEventListener('focus', onChange);
  window.addEventListener('blur', onChange);
  document.addEventListener('visibilitychange', onChange);
  refreshScreenPresence();
  return () => {
    window.removeEventListener('focus', onChange);
    window.removeEventListener('blur', onChange);
    document.removeEventListener('visibilitychange', onChange);
  };
}
