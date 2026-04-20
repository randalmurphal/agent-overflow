// Pure keyboard dispatch helpers for the Composer textarea.
//
// The real keyboard handler lives in Composer.svelte because it needs to
// call into the mention / slash / send flows, but the popover navigation
// logic (ArrowUp / ArrowDown / Tab / Enter / Escape) is mechanical enough
// that it can be expressed as a reducer-style helper. Keeping it here
// means Composer.svelte stops carrying ~60 lines of branching.
//
// Contract: each helper returns an action token the caller can dispatch,
// plus next-state for the active index. The caller is responsible for
// preventing the default event, moving focus, firing insert functions,
// etc.

import type { ComposerMentionsHandle } from './composerMentions.svelte';

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
 * mention / slash popover. Returns 'none' for any key we don't care
 * about, so the caller can fall through to its default handling.
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
 * Handle a keydown against an open mention or slash popover. Returns
 * `true` when the keystroke was consumed (caller must not fall through),
 * `false` when the caller should continue its own logic (e.g. Enter to
 * send).
 */
export function handleMentionPopoverKeydown(
  e: KeyboardEvent,
  mentions: ComposerMentionsHandle,
): boolean {
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

  if (mentions.slashTrigger) {
    // Mutually exclusive with the mention trigger — refreshTriggers keeps
    // only one open at a time, so we only reach this branch when
    // `mentionTrigger` is null.
    const action = popoverNav({
      key: e.key,
      activeIndex: mentions.slashActiveIndex,
      itemCount: mentions.slashFilteredCommands.length,
    });
    if (action.kind === 'move') {
      e.preventDefault();
      mentions.setSlashActiveIndex(action.nextIndex);
      return true;
    }
    if (action.kind === 'insert') {
      const cmd = mentions.slashFilteredCommands[mentions.slashActiveIndex];
      if (cmd) {
        e.preventDefault();
        mentions.insertSlashCommand(cmd);
        return true;
      }
    }
    if (action.kind === 'close') {
      e.preventDefault();
      mentions.closeSlash();
      return true;
    }
  }

  return false;
}
