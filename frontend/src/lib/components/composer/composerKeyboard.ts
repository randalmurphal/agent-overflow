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
import type { ComposerDraftStore } from '../../stores/composerDraft.svelte';
import {
  combineForRetract,
  hasRetractableQueueItems,
  undoQueuedItems,
} from '../../stores/sendQueue.svelte';
import { ListAttachments } from '../../stores/bindings';
import type { Attachment } from '../../types/attachment';
import { errString } from '../../utils/errors';

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
  // Shift+Tab is reserved for the global `mode.cycle` chord even when
  // a popover is open. `popoverNav` collapses Tab and Enter into a
  // single `insert` action without inspecting the shift modifier, so
  // without this guard Shift+Tab while typing `@foo` would insert the
  // active item instead of cycling chat ↔ plan.
  if (e.key === 'Tab' && e.shiftKey) return false;
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

/**
 * Focus the textarea and place the cursor at the end of its current
 * value. Shared idiom: any time the composer programmatically grabs
 * focus (initial mount after draft hydration, queue retract restoring
 * a draft snapshot, etc.) we want the caret to land where the user
 * would resume typing — at the end. Centralising it here keeps the
 * "focus + cursor end" contract in one place.
 */
export function focusTextareaAtEnd(node: HTMLTextAreaElement): void {
  const end = node.value.length;
  node.focus();
  node.setSelectionRange(end, end);
}

// ---- UP-arrow queue retract ----

export interface QueueRetractTriggerArgs {
  /** Raw keydown event. */
  event: KeyboardEvent;
  /** Active thread id, or null if no thread is selected. */
  threadId: string | null;
  /** True when the composer draft has any user content (text, attachments,
   *  or terminal chips). Retract only fires when the composer is empty so
   *  legitimate typing isn't clobbered. */
  hasDraftContent: boolean;
}

/**
 * Predicate for whether a keydown should trigger queue retract.
 *
 * Mirrors Claude TUI's `popAllEditable`: when the composer is empty and
 * Zone 1 of the per-thread send queue has items, plain UP-arrow drops
 * every queued item and merges them into one editable composer draft.
 *
 * Conditions (all must hold):
 *   1. The key is plain ArrowUp (no Ctrl/Cmd/Alt/Shift).
 *   2. A thread is active.
 *   3. The composer has no draft content.
 *   4. Zone 1 has at least one retractable item.
 *   5. The cursor is at the start of the textarea (selectionStart === 0
 *      and selectionEnd === 0). For an empty textarea this is the only
 *      possible cursor position; the explicit check defends against the
 *      edge case where a multi-line empty value somehow ends up with a
 *      non-zero caret index.
 *
 * If any check fails the predicate returns false and the caller must
 * NOT preventDefault — the textarea's native UP behaviour (cursor
 * navigation) is what the user expects.
 */
export function shouldRetractQueueOnUpArrow(args: QueueRetractTriggerArgs): boolean {
  const { event, threadId, hasDraftContent } = args;
  if (event.key !== 'ArrowUp') return false;
  if (event.ctrlKey || event.metaKey || event.altKey || event.shiftKey) return false;
  if (!threadId) return false;
  if (hasDraftContent) return false;
  if (!hasRetractableQueueItems(threadId)) return false;
  const target = event.target;
  if (target instanceof HTMLTextAreaElement) {
    const start = target.selectionStart ?? 0;
    const end = target.selectionEnd ?? 0;
    if (start !== 0 || end !== 0) return false;
  }
  return true;
}

export interface QueueRetractDeps {
  threadId: string;
  draft: ComposerDraftStore;
  textarea: HTMLTextAreaElement | undefined;
  /** Surface RPC failures to the user. The composer wires this to
   *  `pane.setGeneralError` so the error appears in the same row used
   *  by send / interrupt failures. */
  reportError: (message: string) => void;
}

/**
 * Drive the retract: fetch the thread's attachment records (queue items
 * carry only ids, so we resolve them once), drop every Zone 1 item via
 * the backend RPC, combine the dropped items into a single draft
 * snapshot, and restore that snapshot to the composer.
 *
 * On failure the draft is left untouched and the error surfaces via
 * `reportError`. If the queue raced and is empty by the time the RPC
 * lands the function returns silently — the predicate's condition is no
 * longer true and the user's next keystroke can take a different path.
 */
export async function performQueueRetract(deps: QueueRetractDeps): Promise<void> {
  const { threadId, draft, textarea, reportError } = deps;
  let attachments: Attachment[] = [];
  try {
    const records = (await ListAttachments(threadId)) as Attachment[] | null;
    attachments = records ?? [];
  } catch (err) {
    reportError(`Failed to retract queued messages: ${errString(err)}`);
    return;
  }

  let items: Awaited<ReturnType<typeof undoQueuedItems>>;
  try {
    items = await undoQueuedItems(threadId);
  } catch (err) {
    reportError(`Failed to retract queued messages: ${errString(err)}`);
    return;
  }
  if (items.length === 0) return;

  const attachmentById = new Map<string, Attachment>();
  for (const attachment of attachments) {
    attachmentById.set(attachment.id, attachment);
  }
  const snapshot = combineForRetract(items, (id) => attachmentById.get(id));

  try {
    await draft.restoreDraftFor(threadId, snapshot);
  } catch (err) {
    // restoreDraftFor itself swallows save failures and records them on
    // the draft store, but we still guard against unexpected throws so a
    // bug there doesn't leave the focus / cursor in an odd state.
    reportError(`Failed to restore retracted draft: ${errString(err)}`);
    return;
  }

  if (textarea) {
    focusTextareaAtEnd(textarea);
  }
}
