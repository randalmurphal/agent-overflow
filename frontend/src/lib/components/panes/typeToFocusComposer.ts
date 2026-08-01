// Type-to-focus: when nothing else claims a bare printable keystroke, land it
// in the focused chat pane's composer so the user can just start typing.
//
// Every guard here is fail-closed: the redirect fires only in the one state
// where no other surface could want the key — no overlay/modal/picker, no
// focus trap, no interactive element focused, a plain thread pane with no
// pending prompt and a hydrated draft. A guard failing means "do nothing",
// i.e. exactly the pre-feature behavior, so a wrong answer can only cost the
// convenience, never an existing interaction.
//
// Dispatch order is part of the contract: App.svelte calls this only after
// dispatchKey() declined the event on the non-editable branch, so every
// keybinding — including future user-configured bare-key chords — wins ahead
// of the redirect.

import { getFocusedPaneId, getPane, iterPanes, revealPane } from '../../stores/panes.svelte';
import { getComposerDraftForPane } from '../../stores/composerDraftRegistry.svelte';
import { getTerminalFocused } from '../terminal/terminalStore.svelte';
import { focusTextareaAtEnd } from '../composer/composerKeyboard';
import { findPaneComposer } from './paneComposerFocus';
import { hasActiveFocusTrap } from '../../utils/focusTrap';

export interface TypeToFocusSurfaceFlags {
  workflowsOverlayOpen: boolean;
  anyModalOpen: boolean;
  anyPickerOpen: boolean;
}

/**
 * A key qualifies only when it would insert a single printable character on
 * its own: no ctrl/meta (chords), no alt (macOS option-glyphs are excluded
 * wholesale rather than distinguished from dead keys and other platforms'
 * accelerators). Shift stays allowed so capitals and shifted punctuation
 * redirect. Space is excluded: on a focused scroll container space means
 * page-down, and no message starts with one. IME preedit keystrokes
 * (isComposing / legacy keyCode 229) must never redirect — moving focus
 * mid-composition corrupts the composition buffer.
 */
export function isTypeToFocusKey(ev: KeyboardEvent): boolean {
  if (ev.metaKey || ev.ctrlKey || ev.altKey) return false;
  if (ev.isComposing || ev.keyCode === 229) return false;
  if (ev.key.length !== 1) return false; // 'Enter', 'F2', 'Dead', arrows…
  return ev.key !== ' ';
}

// Elements whose focus (own or ancestral) means the keyboard is already
// claimed: activatable controls (Space/Enter have meaning there), ARIA
// widgets with their own key model, and any modal/popover surface — typing
// must never reach a composer behind an open dialog. Plain containers (body,
// the timeline's tabindex="-1" scroll surface) intentionally do NOT match,
// so click-to-read-then-type keeps working.
const CLAIMED_FOCUS_SELECTOR = [
  'button', 'a[href]', 'input', 'textarea', 'select', 'summary', 'iframe',
  'audio[controls]', 'video[controls]',
  '[contenteditable]:not([contenteditable="false"])',
  '[role="button"]', '[role="link"]', '[role="checkbox"]', '[role="radio"]',
  '[role="switch"]', '[role="slider"]', '[role="textbox"]', '[role="combobox"]',
  '[role="searchbox"]', '[role="spinbutton"]',
  '[role="menu"]', '[role="menubar"]', '[role="menuitem"]',
  '[role="menuitemcheckbox"]', '[role="menuitemradio"]',
  '[role="listbox"]', '[role="option"]', '[role="tablist"]', '[role="tab"]',
  '[role="tree"]', '[role="treeitem"]', '[role="grid"]', '[role="row"]',
  '[role="toolbar"]', '[role="dialog"]', '[aria-modal="true"]',
  '[data-popover]', '[data-modal-backdrop]',
].join(', ');

export function isClaimedFocus(active: Element | null): boolean {
  if (!active || active === document.body || active === document.documentElement) return false;
  return active.closest(CLAIMED_FOCUS_SELECTOR) !== null;
}

/**
 * Redirect an unclaimed printable keydown into the focused chat pane's
 * composer. Returns true when focus moved. Deliberately does NOT
 * preventDefault: focus moves during keydown, so the browser inserts the
 * character into the textarea natively — no synthetic value writes (the
 * composer's value is $derived from the draft store, not bind:value).
 */
export function redirectTypingToFocusedComposer(
  ev: KeyboardEvent,
  surfaces: TypeToFocusSurfaceFlags,
): boolean {
  if (typeof document === 'undefined') return false;
  if (!isTypeToFocusKey(ev)) return false;
  // An open overlay/modal/picker owns the keyboard outright — never focus
  // something behind it. The focus-trap check catches modal surfaces that
  // don't flow through the command-context flags.
  if (surfaces.workflowsOverlayOpen || surfaces.anyModalOpen || surfaces.anyPickerOpen) return false;
  if (hasActiveFocusTrap()) return false;
  if (isClaimedFocus(document.activeElement)) return false;

  // ComposerPendingUserInputPanel answers bare digits at window level from
  // ANY pane, and registers its listener after this one — its preventDefault
  // isn't visible here. A pending user-input prompt anywhere yields the
  // whole feature so digit-answering stays intact.
  for (const pane of iterPanes()) {
    if (pane.pendingUserInputs.length > 0) return false;
  }

  // Raw focus id on purpose (same rule as Composer's initial-focus pass): a
  // focused companion/take-control pane isn't in the ThreadPane registry and
  // must not pull its source's composer — that would fire focusin on the
  // source section and demote the companion.
  const paneId = getFocusedPaneId();
  const pane = paneId ? getPane(paneId) : undefined;
  if (!paneId || !pane) return false;
  if (getTerminalFocused(paneId)) return false;
  // An approval prompt in this pane owns the keyboard: its action row takes
  // bare j/k/h/l once focused, and grabbing the textarea under a blocking
  // prompt isn't "just start typing".
  if (pane.pendingApprovals.length > 0) return false;

  // Same hydration gate as the initial-focus pass: caret-at-end is only
  // meaningful once the resumed draft's value is actually in the textarea.
  const draft = getComposerDraftForPane(paneId);
  if (!draft || draft.hydrating || draft.threadId !== pane.threadId) return false;

  const textarea = findPaneComposer(paneId);
  if (!textarea) return false;

  focusTextareaAtEnd(textarea);
  // Typing is explicit compose intent, so the focused pane may reveal —
  // focus alone never scrolls the strip (revealPane's contract), and without
  // this the first keystroke into an off-screen pane would be invisible.
  revealPane(paneId);
  return true;
}
