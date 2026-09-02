// Pure keyboard dispatch helpers for the Composer textarea.
//
// The real keyboard handler lives in Composer.svelte because it needs to
// call into the mention / send flows, but the popover navigation logic
// (ArrowUp / ArrowDown / Tab / Enter / Escape) is mechanical enough that
// it can be expressed as a reducer-style helper. Keeping it here means
// Composer.svelte stops carrying ~60 lines of branching.
//
// Contract: each helper returns an action token the caller can dispatch,
// plus next-state for the active index. The caller is responsible for
// preventing the default event, moving focus, firing insert functions,
// etc.

import type { ComposerMentionsHandle } from './composerMentions.svelte';
import type { ComposerSlashHandle } from './composerSlash.svelte';
import { isImeComposingEvent } from '../../utils/imeComposition';

export type PopoverAction =
  | { kind: 'move'; nextIndex: number }
  | { kind: 'insert' }
  | { kind: 'close' }
  | { kind: 'none' };

export interface PopoverNavArgs {
  key: string;
  activeIndex: number;
  itemCount: number;
}

/**
 * Decide what a single keydown should do in the context of an open
 * mention popover. Returns 'none' for any key we don't care about, so
 * the caller can fall through to its default handling.
 */
export function popoverNav({ key, activeIndex, itemCount }: PopoverNavArgs): PopoverAction {
  if (itemCount === 0) {
    // Empty popover: Escape still closes it, everything else bubbles.
    if (key === 'Escape') return { kind: 'close' };
    return { kind: 'none' };
  }

  switch (key) {
    case 'ArrowDown':
      return { kind: 'move', nextIndex: Math.min(activeIndex + 1, itemCount - 1) };
    case 'ArrowUp':
      return { kind: 'move', nextIndex: Math.max(activeIndex - 1, 0) };
    case 'Enter':
    case 'Tab':
      return { kind: 'insert' };
    case 'Escape':
      return { kind: 'close' };
    default:
      return { kind: 'none' };
  }
}

/**
 * Keystrokes an open popover must never claim, whichever menu is open.
 * `popoverNav` reduces over key names alone, so both facts — the modifier
 * and the IME state — have to be settled before it is consulted.
 */
function popoverYieldsKey(e: KeyboardEvent): boolean {
  // Shift+Tab is reserved for the global `mode.cycle` chord even when
  // a popover is open. `popoverNav` collapses Tab and Enter into a
  // single `insert` action without inspecting the shift modifier, so
  // without this guard Shift+Tab while typing `@foo` would insert the
  // active item instead of cycling chat ↔ plan.
  if (e.key === 'Tab' && e.shiftKey) return true;
  // Mid-IME-composition, both insert keys belong to the IME: Enter confirms
  // the candidate and Tab walks the candidate list. Inserting the highlighted
  // completion here would replace the still-composing text.
  if ((e.key === 'Enter' || e.key === 'Tab') && isImeComposingEvent(e)) return true;
  return false;
}

/**
 * Handle a keydown against an open mention popover. Returns `true` when
 * the keystroke was consumed (caller must not fall through), `false`
 * when the caller should continue its own logic (e.g. Enter to send).
 */
export function handleMentionPopoverKeydown(
  e: KeyboardEvent,
  mentions: ComposerMentionsHandle,
): boolean {
  if (popoverYieldsKey(e)) return false;
  if (mentions.mentionTrigger) {
    const action = popoverNav({
      key: e.key,
      activeIndex: mentions.mentionActiveIndex,
      itemCount: mentions.mentionResults.length,
    });
    if (action.kind === 'move') {
      e.preventDefault();
      mentions.setMentionActiveIndex(action.nextIndex);
      return true;
    }
    if (action.kind === 'insert') {
      const target = mentions.mentionResults[mentions.mentionActiveIndex];
      if (target) {
        e.preventDefault();
        mentions.insertMention(target);
        return true;
      }
    }
    if (action.kind === 'close') {
      e.preventDefault();
      mentions.closeMention();
      return true;
    }
  }

  return false;
}

/**
 * First selectable row at or past `from`, walking in `step` direction.
 *
 * Disabled rows exist (a Codex skill turned off in config, a "loading branches"
 * placeholder) and must not be landable: arrowing onto one would strand the
 * user on a row Enter refuses. Falls back to the starting index when every
 * candidate in that direction is disabled, so the highlight never leaves the
 * list.
 */
function nextSelectableIndex(
  entries: readonly { disabled?: boolean }[],
  from: number,
  step: 1 | -1,
): number {
  for (let i = from; i >= 0 && i < entries.length; i += step) {
    if (!entries[i]?.disabled) return i;
  }
  return -1;
}

/**
 * Handle a keydown against an open command popover. Same contract as
 * `handleMentionPopoverKeydown` — `true` means consumed — and the same
 * navigation reducer, so the two menus cannot drift on what Enter or Escape
 * does. The two are mutually exclusive in practice (`@` only triggers after
 * whitespace and the `/` menu's provider rows only at position 0), but the
 * caller still dispatches them in a fixed order rather than relying on that.
 */
export function handleSlashPopoverKeydown(
  e: KeyboardEvent,
  slash: ComposerSlashHandle,
): boolean {
  if (popoverYieldsKey(e)) return false;
  if (!slash.slashOpen) return false;

  const results = slash.slashResults;
  const action = popoverNav({
    key: e.key,
    activeIndex: slash.slashActiveIndex,
    itemCount: results.length,
  });
  if (action.kind === 'move') {
    e.preventDefault();
    const step = action.nextIndex >= slash.slashActiveIndex ? 1 : -1;
    const target = nextSelectableIndex(results, action.nextIndex, step);
    if (target >= 0) slash.setSlashActiveIndex(target);
    return true;
  }
  if (action.kind === 'insert') {
    const target = results[slash.slashActiveIndex];
    if (target && !target.disabled) {
      e.preventDefault();
      slash.insertCommand(target);
      return true;
    }
  }
  if (action.kind === 'close') {
    e.preventDefault();
    slash.closeSlash();
    return true;
  }
  return false;
}

export interface ComposerInputKeydownDeps {
  mentions: ComposerMentionsHandle;
  slash: ComposerSlashHandle;
  /**
   * The host's first look at the keystroke, before either popover. Return
   * true when it was consumed. The composer uses it to walk ArrowUp into a
   * pending user-input request's options, which are rendered above the
   * input surface and so are not the surface's to find.
   */
  claimKey?: (event: KeyboardEvent) => boolean;
  /**
   * The host's second look, after both completion menus have declined
   * the keystroke. History recall lives here rather than in `claimKey`:
   * an open `@` or `/` menu owns ArrowUp/ArrowDown, and a pre-popover
   * claim would steal its navigation. Return true when consumed — the
   * claimer owns preventDefault, same contract as `claimKey`.
   */
  claimAfterPopovers?: (event: KeyboardEvent) => boolean;
  /** Atomic image-placeholder deletion. Returns true when consumed. */
  placeholderKeydown: (event: KeyboardEvent) => boolean;
  /** A plain Enter that nothing above claimed, i.e. "submit this". */
  submitEnter: () => void;
  /**
   * False under the compact layout: a phone's Return key inserts a
   * newline and the Send button is the one way to send. Defaults to
   * sending.
   */
  enterSends?: boolean;
}

/**
 * The composer textarea's whole keydown contract, in dispatch order. Lives
 * here rather than in the component because the order IS the contract:
 * every branch below exists to stop a later one from firing, and reading
 * them together is the only way to see that.
 */
export function dispatchComposerInputKeydown(
  e: KeyboardEvent,
  deps: ComposerInputKeydownDeps,
): void {
  if (deps.claimKey?.(e)) return;

  const popoverOpen = deps.mentions.mentionTrigger !== null || deps.slash.slashOpen;

  // Shift+Tab is owned by the global keydown handler (`mode.cycle`).
  // Yield without preventDefault — the global handler bails on
  // `defaultPrevented`, so consuming the chord here would cancel
  // the dispatch; the global handler preventDefaults on successful
  // dispatch to suppress the browser's focus-shift. The popover
  // guard skips this branch when a menu is open, but
  // `handleMentionPopoverKeydown` / `handleSlashPopoverKeydown` below
  // have their own Shift+Tab bail-out so the chord still reaches the
  // global dispatcher.
  if (e.key === 'Tab' && e.shiftKey && !popoverOpen) return;

  // Plain Tab (no popover) is a no-op inside the composer. Browser
  // default would advance focus out of the textarea, which we don't
  // want — users navigate panes/sidebar via explicit chords. With either
  // completion menu open, Tab belongs to the menu (it completes) and the
  // dispatch below claims it.
  if (e.key === 'Tab' && !e.shiftKey && !popoverOpen) {
    e.preventDefault();
    return;
  }

  // Popover dispatch short-circuits when the keystroke was consumed;
  // otherwise we fall through to the send guard below.
  if (handleMentionPopoverKeydown(e, deps.mentions)) return;
  if (handleSlashPopoverKeydown(e, deps.slash)) return;

  if (deps.claimAfterPopovers?.(e)) return;

  if (deps.placeholderKeydown(e)) return;

  // Enter mid-IME-composition confirms the candidate; the composed text is
  // still in the IME's buffer, not the textarea's value. Yield WITHOUT
  // preventDefault — the browser has to deliver it to the composition.
  if (e.key === 'Enter' && isImeComposingEvent(e)) return;

  if (e.key === 'Enter' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey) {
    if (deps.enterSends === false) return;
    e.preventDefault();
    deps.submitEnter();
  }
}

/** Focus the textarea with the caret collapsed at the given offset. */
export function focusTextareaAt(node: HTMLTextAreaElement, offset: number): void {
  // preventScroll is load-bearing: a bare focus() natively scrolls every
  // scrollable ancestor — the horizontal pane strip included — to the
  // textarea. Strip reveal belongs to revealPane/PaneHost alone; DOM focus
  // must never scroll (same contract as paneComposerFocus.ts).
  node.focus({ preventScroll: true });
  node.setSelectionRange(offset, offset);
}

/**
 * Focus the textarea and place the cursor at the end of its current
 * value. Shared idiom: any time the composer programmatically grabs
 * focus (initial mount after draft hydration, thread switch, etc.) we
 * want the caret to land where the user
 * would resume typing — at the end. Centralising it here keeps the
 * "focus + cursor end" contract in one place. (History recall is the
 * one caller that places the caret elsewhere — at offset 0 while
 * walking UP, so the next ArrowUp passes the caret gate again.)
 */
export function focusTextareaAtEnd(node: HTMLTextAreaElement): void {
  focusTextareaAt(node, node.value.length);
}
