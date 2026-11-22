// Composer DOM-focus helpers, serving two callers with one no-scroll rule:
//
// 1. Pane moves by keyboard (paneNavigation.ts, pane.close): when focus
//    shifts panes while the user was typing, the caret moves into the new
//    pane, its composer when it takes input, else its section, so later
//    keystrokes land where logical focus says. Without this, alt+h from
//    inside the right pane moves focusedPaneId but leaves the caret in the
//    right pane's textarea, where Enter still sends.
//
// 2. Picker-popup closes (`restorePickerFocus`): the one place composer
//    toolbar and workspace pickers decide where focus goes when their
//    popover closes, gated by the close reason.

import {
  popoverCloseRestoresFocus,
  type PopoverCloseReason,
} from '../../utils/popoverOwnership';
import { focusChosen } from '../../utils/focusIntent';

/** Whether an editing caret holds focus now, which a pane move carries. */
export function isEditingFocused(): boolean {
  const active = typeof document !== 'undefined' ? document.activeElement : null;
  if (!(active instanceof HTMLElement)) return false;
  const tag = active.tagName;
  if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return true;
  return active.isContentEditable;
}

/**
 * Whether DOM focus sits inside a pane's section, whatever element holds
 * it (a composer, a diff scroller, a button), which a pane move carries.
 */
export function isFocusInAPane(): boolean {
  const active = typeof document !== 'undefined' ? document.activeElement : null;
  return active instanceof HTMLElement && active.closest('[data-pane-id]') !== null;
}

/** Whether an editing caret holds focus inside `paneId`'s section. */
export function isEditingFocusedIn(paneId: string): boolean {
  if (!isEditingFocused()) return false;
  const section = paneSection(paneId);
  return section !== null && section.contains(document.activeElement);
}

function paneSection(paneId: string): HTMLElement | null {
  if (typeof document === 'undefined') return null;
  return document.querySelector<HTMLElement>(`[data-pane-id="${CSS.escape(paneId)}"]`);
}

/**
 * Puts the caret in `paneId`, so keys typed after a move never reach the
 * pane focus left: its composer when it takes input; else the pane's
 * primary scroller (`data-pane-scroller`, focusable by script), where the
 * browser starts a keyboard scroll, so PageDown and the arrows scroll it;
 * else (a side chat still preparing) PaneHost's section, focusable only
 * while it holds this focus, so a click never lands on it. It is a focus
 * choice: no deferred focus asked for before it lands after it.
 */
export function carryCaretIntoPane(paneId: string): void {
  focusChosen();
  if (focusPaneComposer(paneId)) return;
  const section = paneSection(paneId);
  if (!section) return;
  const scroller = section.querySelector<HTMLElement>('[data-pane-scroller]');
  if (scroller) {
    scroller.focus({ preventScroll: true });
    return;
  }
  focusSectionOnce(section);
}

function focusSectionOnce(section: HTMLElement): void {
  if (section.hasAttribute('tabindex')) {
    section.focus({ preventScroll: true });
    return;
  }
  section.tabIndex = -1;
  section.focus({ preventScroll: true });
  if (document.activeElement !== section) {
    section.removeAttribute('tabindex');
    return;
  }
  section.addEventListener('blur', () => section.removeAttribute('tabindex'), { once: true });
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
 * (terminal pane, companion pane) or disabled (blocking prompt,
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
