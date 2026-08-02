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
 * Handle a keydown against an open slash-command popover. Same contract as
 * `handleMentionPopoverKeydown` — `true` means consumed — and the same
 * navigation reducer, so the two menus cannot drift on what Enter or Escape
 * does. The two are mutually exclusive in practice (`/` only triggers at the
 * start of the draft, `@` only after whitespace), but the caller still
 * dispatches them in a fixed order rather than relying on that.
 */
export function handleSlashPopoverKeydown(
  e: KeyboardEvent,
  slash: ComposerSlashHandle,
): boolean {
  if (popoverYieldsKey(e)) return false;
  if (!slash.slashTrigger) return false;

  const action = popoverNav({
    key: e.key,
    activeIndex: slash.slashActiveIndex,
    itemCount: slash.slashResults.length,
  });
  if (action.kind === 'move') {
    e.preventDefault();
    slash.setSlashActiveIndex(action.nextIndex);
    return true;
  }
  if (action.kind === 'insert') {
    const target = slash.slashResults[slash.slashActiveIndex];
    if (target) {
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

/**
 * Focus the textarea and place the cursor at the end of its current
 * value. Shared idiom: any time the composer programmatically grabs
 * focus (initial mount after draft hydration, thread switch, etc.) we
 * want the caret to land where the user
 * would resume typing — at the end. Centralising it here keeps the
 * "focus + cursor end" contract in one place.
 */
export function focusTextareaAtEnd(node: HTMLTextAreaElement): void {
  const end = node.value.length;
  // preventScroll is load-bearing: a bare focus() natively scrolls every
  // scrollable ancestor — the horizontal pane strip included — to the
  // textarea. Strip reveal belongs to revealPane/PaneHost alone; DOM focus
  // must never scroll (same contract as paneComposerFocus.ts).
  node.focus({ preventScroll: true });
  node.setSelectionRange(end, end);
}
