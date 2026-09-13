// One step back on the phone, shared by the two doors that ask for it:
// the Android shell's hardware key (`native/lifecycle.ts`) and a browser's
// Back in a phone-sized browser (`utils/compactHistoryBack.svelte.ts`).
// Kept out of `native/` so the browser door does not pull the shell's
// lazily loaded chunk into the entry bundle.
//
// The stack, in order: an open overlay or sheet closes first; a companion
// screen (review, plan, agent) on screen closes and the thread it was
// opened from comes back; a terminal drawer stacked over the chat closes;
// the thread screen goes back to the list; and the list screen is the
// root, where the caller decides what leaving means. The companion rung
// runs before the terminal rung because opening a companion leaves focus
// on its source thread: with a terminal open there and the review pane on
// screen, the terminal rung would toggle a drawer nobody can see.
//
// **The overlay case dispatches Escape rather than reaching into
// components.** Every overlay, sheet, popover and dialog in this app
// already closes on Escape through the keybinding path, and there are
// enough of them that a registry here would be a second list to keep in
// sync. So the step sends a marked surface-dismissal Escape, not a
// keyboard shortcut, and whether anything consumed it is read off
// `defaultPrevented`. The one target it avoids is a focused terminal:
// xterm turns Escape into an ESC byte for the shell and reports the key
// consumed, which is a keystroke nobody pressed. From there the key goes
// to the pane instead, where the same window-level handlers still see it.

import { closeCompanion, getCompanionPane } from '../stores/companionPanes.svelte';
import {
  getCompactScreen,
  isCompactLayout,
  onScreenCompactPaneId,
  showCompactList,
} from '../stores/layoutMode.svelte';
import { getFocusedPaneOrNull } from '../stores/panes.svelte';
import { runTerminalToggle } from '../components/terminal/terminalToggle';
import { createSurfaceDismissalEvent } from './surfaceDismissal';

/**
 * Ask the page to close whatever is on top, and report whether anything
 * did. A synthetic Escape at the active element, so it walks exactly the
 * component dismissal path a real key press walks. Global dispatch admits
 * only surface dismissal commands; Back must never become Interrupt Turn.
 */
function dismissTopSurface(): boolean {
  if (typeof document === 'undefined') return false;
  const active = document.activeElement;
  const target = active?.closest('.xterm')
    ? (active.closest('[data-pane-id]') ?? document.body)
    : (active ?? document.body);
  if (!target) return false;
  const event = createSurfaceDismissalEvent();
  target.dispatchEvent(event);
  return event.defaultPrevented;
}

/**
 * One step back. Answers whether the step did anything; a `false` means
 * the list screen is showing, which is the root, and the caller decides
 * what leaving means (the shell exits, a browser goes back a page). Each
 * rung answers for itself, so a press never does two things.
 */
export function stepBack(): boolean {
  if (dismissTopSurface()) return true;
  if (!isCompactLayout() || getCompactScreen() !== 'thread') return false;
  // The companion on screen closes and reveals its source itself
  // (`closeCompanion` reads the strip's geometry under compact).
  const onScreenPaneId = onScreenCompactPaneId();
  if (onScreenPaneId && getCompanionPane(onScreenPaneId)) {
    closeCompanion(onScreenPaneId);
    return true;
  }
  const focused = getFocusedPaneOrNull();
  if (focused?.showTerminal) {
    runTerminalToggle(focused);
    return true;
  }
  showCompactList();
  return true;
}
