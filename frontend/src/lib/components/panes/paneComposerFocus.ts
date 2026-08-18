// Composer DOM-focus helpers, serving two callers with one no-scroll rule:
//
// 1. Pane-nav keybindings (pane.focusLeft, pane.focusRight, pane.close,
//    thread.newPane): when focus shifts panes while the user was typing,
//    move DOM focus to the new pane's composer textarea so subsequent
//    keystrokes land where the user expects. Without this, pressing alt+h
//    from inside the right pane updates focusedPaneId but leaves the caret
//    in the right pane's textarea — feeling like alt+h "did nothing".
//
// 2. Picker-popup closes (`restorePickerFocus`): the one place composer
//    toolbar and workspace pickers decide where focus goes when their
//    popover closes, gated by the close reason.

import {
  popoverCloseRestoresFocus,
  type PopoverCloseReason,
} from '../../utils/popoverOwnership';

function isActiveElementEditable(): boolean {
  const active = typeof document !== 'undefined' ? document.activeElement : null;
  if (!active) return false;
  const tag = active.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return (active as HTMLElement).isContentEditable === true;
}

export function focusPaneComposerIfEditableActive(paneId: string): void {
  if (typeof document === 'undefined') return;
  if (!isActiveElementEditable()) return;
  focusPaneComposer(paneId);
}

/**
 * Move DOM focus to the named pane's composer textarea. Returns false
 * when the textarea is missing or disabled, so callers can fall back:
 * composer-toolbar pickers refocus their trigger button, and the
 * terminal-close path uses it to keep the caret off <body> after the
 * drawer unmounts.
 */
export function focusPaneComposer(paneId: string): boolean {
  const textarea = findPaneComposer(paneId);
  if (!textarea) return false;
  // preventScroll is load-bearing: a bare focus() instantly scrolls every
  // scrollable ancestor (the horizontal pane strip included) to the
  // textarea, and it runs synchronously while PaneHost's smooth reveal is
  // still one rAF away — turning pane-nav onto an off-screen pane into a
  // snap. Strip reveal belongs to revealPane/PaneHost alone; DOM focus
  // must never scroll. (xterm's Terminal.focus() already does the same.)
  textarea.focus({ preventScroll: true });
  return true;
}

/**
 * The one focus-restore path for a picker popup closing. Explicit
 * dismissals ({@link popoverCloseRestoresFocus}) put the caret back: in
 * the pane's composer when `paneId` is given and its textarea is
 * available, else on the trigger. Composer-first because the pickers sit
 * just under the textarea — after picking an effort or a branch the user
 * is almost always going to keep typing. Closes the user caused by engaging
 * something ELSE — 'outside-click', or 'anchor-gone' when the trigger
 * scrolled away — restore nothing: focus belongs where the user just put
 * it, and a restore here re-focuses the popup's pane, flipping logical
 * pane focus away from the one the user clicked. Leaving focus on <body>
 * after such a dismissal is covered: type-to-focus routes the next
 * printable keystroke into the focused pane's composer. (Escape/Tab can
 * never race that path — a popover claiming Tab closes before the caret
 * could move, so the restore and the user's own focus never disagree.)
 *
 * The trigger fallback is preventScroll'd for the same reason
 * `focusPaneComposer` is: a bare `.focus()` on a trigger scrolled out of
 * the pane strip synchronously snaps the strip back to it — observed as
 * a thread-click reveal gliding from the popup's pane instead of from
 * where the user actually was. DOM focus must never scroll; strip reveal
 * belongs to revealPane/PaneHost alone.
 *
 * The target union requires naming at least one destination — a bare
 * `{}` is a compile error — while still accepting a possibly-undefined
 * trigger binding from callers that only hold `$state` refs.
 */
export function restorePickerFocus(
  reason: PopoverCloseReason | undefined,
  target:
    | { paneId: string; triggerEl?: HTMLElement }
    | { paneId?: undefined; triggerEl: HTMLElement | undefined },
): void {
  if (!popoverCloseRestoresFocus(reason)) return;
  if (target.paneId !== undefined && focusPaneComposer(target.paneId)) return;
  target.triggerEl?.focus({ preventScroll: true });
}

/**
 * Locate the named pane's composer textarea, or null when it is missing
 * (discussion-mode pane, companion pane) or disabled (blocking prompt,
 * !canCompose). Exported for type-to-focus, which needs the node itself
 * to pair focus with caret-at-end.
 */
export function findPaneComposer(paneId: string): HTMLTextAreaElement | null {
  if (typeof document === 'undefined') return null;
  const root = document.querySelector(`[data-pane-id="${CSS.escape(paneId)}"]`);
  if (!root) return null;
  const textarea = root.querySelector<HTMLTextAreaElement>(
    'textarea[aria-label="Message Input"]',
  );
  if (!textarea || textarea.disabled) return null;
  return textarea;
}
