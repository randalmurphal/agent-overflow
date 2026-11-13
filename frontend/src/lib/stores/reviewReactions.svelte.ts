import { SetPRCommentReaction } from './bindings';
import { threadWhere } from './reviewCommentWrites.svelte';
import type { ForgeOps } from './reviewForgeOps.svelte';
import { reactionGlyph } from '../utils/reactionEmoji';
import { prReferenceWire } from '../utils/prReference';
import type { CommentReaction, ReviewComment, ReviewThread } from '../types/models';

// Reactions on a review pane's PR comments
// (docs/specs/pull-requests.md#suggestions-reactions-and-preview): a chip per reaction present,
// which toggles the viewer's own, and the Add reaction popover's eight.
// Each chip is one operation (reviewForgeOps.svelte.ts): pending from
// the press until the forge's answer is on screen, one at a time, its
// failure on the comment's chip row. Which comment's popover is open
// lives here, so the pane's PR changing closes it.

export interface ReviewReactions {
  /** The backend reads and writes reactions (pr-suggestions-reactions.v1)
   * and the pane is on a PR: the chips show. */
  readonly available: boolean;
  /** Add reaction is offered: available, and the forge's reactions read
   * answered (a reaction added while they cannot be read could not show). */
  readonly canAdd: boolean;
  /** Why a reaction write is withheld now (the now writes' reason), or null. */
  readonly withheld: string | null;
  /** The reactions present on a comment, chip order (the forge's). */
  reactionsOf(comment: ReviewComment): readonly CommentReaction[];
  /** The chip for `name` on the comment is waiting for the forge. */
  pending(thread: ReviewThread, comment: ReviewComment, name: string): boolean;
  /** The chip row's failure, the backend's sentence. */
  error(thread: ReviewThread, comment: ReviewComment): string | null;
  /** Adds (`reacted`) or removes the viewer's reaction. */
  toggle(thread: ReviewThread, comment: ReviewComment, name: string, reacted: boolean): Promise<void>;
  /** The comment whose Add reaction popover is open. */
  pickerOpen(thread: ReviewThread, comment: ReviewComment): boolean;
  setPickerOpen(thread: ReviewThread, comment: ReviewComment, open: boolean): void;
}

export interface ReviewReactionsHost {
  readonly ops: ForgeOps;
  capable(): boolean;
  onPR(): boolean;
  withheld(): string | null;
  /** Why the comments carry no reactions now, or null. */
  unavailable(): string | null;
}

const NO_REACTIONS: readonly CommentReaction[] = Object.freeze([]);

function commentKey(thread: ReviewThread, comment: ReviewComment): string {
  return `${thread.id}\u0000${comment.id}`;
}

export function reactionOpKey(thread: ReviewThread, comment: ReviewComment, name: string): string {
  return `reaction:${commentKey(thread, comment)}\u0000${name}`;
}

export function createReviewReactions(host: ReviewReactionsHost): ReviewReactions {
  const available = $derived(host.capable() && host.onPR());
  const canAdd = $derived(available && host.unavailable() === null);
  // The open popover, with the PR epoch it opened in: a PR change closes it.
  let picker = $state<{ key: string; epoch: number } | null>(null);
  const openPicker = $derived(picker !== null && picker.epoch === host.ops.epoch ? picker.key : null);

  return {
    get available() { return available; },
    get canAdd() { return canAdd; },
    get withheld() { return host.withheld(); },
    reactionsOf: (comment) => (available ? comment.reactions ?? NO_REACTIONS : NO_REACTIONS),
    pending: (thread, comment, name) => host.ops.pending(reactionOpKey(thread, comment, name)),
    error: (thread, comment) => host.ops.error(`reactions:${commentKey(thread, comment)}`),
    async toggle(thread, comment, name, reacted) {
      if (!available || host.withheld() !== null || comment.id === '') return;
      const target = { threadId: thread.id, commentId: comment.id };
      await host.ops.run(
        reactionOpKey(thread, comment, name),
        `reactions:${commentKey(thread, comment)}`,
        (ref) => SetPRCommentReaction(prReferenceWire(ref), target, name, reacted),
        { subject: `Your ${reactionGlyph(name)} reaction on ${threadWhere(thread)}`, notDone: reacted ? 'was not added' : 'was not removed' },
      );
    },
    pickerOpen: (thread, comment) => openPicker === commentKey(thread, comment),
    setPickerOpen(thread, comment, open) {
      const key = commentKey(thread, comment);
      if (open) picker = { key, epoch: host.ops.epoch };
      else if (picker?.key === key) picker = null;
    },
  };
}
