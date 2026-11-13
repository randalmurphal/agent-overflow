import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import { PostPRLineComment, ReplyToPRThread } from './bindings';
import { getTransportHelloFor } from './transportStatus.svelte';
import { commentWriteKey, createCommentWrites, type CommentWrites } from './reviewCommentWrites.svelte';
import type { BackendKey } from '../transport/backendKey';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import { threadRange } from '../utils/reviewRange';
import type { CommentAnchor } from '../utils/reviewRows';
import type { DiffReviewComment, DiffReviewCommentInput, DiffReviewScope, DiffReviewThreadContext, ReviewThread } from '../types/models';

// Send now or hold (docs/specs/pull-requests.md#sending-and-managing-comments)
// and a PR thread's reply composer, for one review pane. "Now" posts one
// comment to the PR at once (reviewCommentWrites.svelte.ts settles it);
// "hold" saves a draft for the batch. The pane owns the diff, its editors
// and its drafts; this owns the gates on posting now, the reply
// composers' state, and the held replies among the drafts.

/** Where a PR thread's reply composer is: its diff row or its card in
 * the overview's Conversation section. */
export type ReviewReplySurface = 'diff' | 'conversation';

// The backend's comment actions (PostPRLineComment, PostPRComment,
// EditPRComment, DeletePRComment, held replies and ranges in drafts).
const PR_COMMENT_ACTIONS = 'pr-comment-actions.v1';

const NO_DRAFTS: readonly DiffReviewComment[] = Object.freeze([]);
const NO_ROOT = 'Thread has no top-level comment to reply to.';

/** The members a review pane state carries from here. */
export interface ReviewCommentActionsState {
  /** A comment box here can post to the PR now: the whole PR's diff on a
   * backend with the comment actions. */
  readonly canPostNow: boolean;
  /** Why a now write is withheld (stale diff, no grant, a failing pump),
   * or null; shown as the control's hover text. */
  readonly nowWithheldReason: string | null;
  /** Sent comments offer edit and delete (as the forge permits). */
  readonly sentCommentActions: boolean;
  /** A reply can be held as a draft for the batch. */
  readonly canHoldReply: boolean;
  /** What a comment box's primary button (and Ctrl/Cmd+Enter) does: now
   * while the pane holds no drafts for this PR, a draft once it holds one. */
  readonly primaryCommentAction: 'now' | 'draft';
  /** A now write keyed by commentWriteKey: in flight or awaiting its frame. */
  commentWritePending(key: string): boolean;
  /** The forge's sentence for a now write that failed, kept in place. */
  commentWriteError(key: string): string | null;
  /** Edit and delete of sent comments, and the PR-level comment. */
  readonly sentComments: CommentWrites;

  replyBodyFor(threadId: string): string;
  setReplyBody(threadId: string, body: string): void;
  /** Whether a thread's reply composer is open on one surface (the diff
   * row or the Conversation card); store-backed, both rows unmount. */
  replyOpen(threadId: string, surface: ReviewReplySurface): boolean;
  /** Opens the composer, or on a thread holding replies the newest one's
   * edit. */
  openReply(threadId: string, surface: ReviewReplySurface): void;
  /** Closes the composer; its text stays for the next open until a send
   * succeeds. */
  closeReply(threadId: string, surface: ReviewReplySurface): void;
  /** Posts the reply composer's text now; the composer stays open, pending,
   * until the forge's answer is on screen. */
  sendPRThreadReply(thread: ReviewThread): Promise<void>;
  /** Saves the reply composer's text as a held reply to the thread. */
  holdPRThreadReply(thread: ReviewThread): Promise<void>;
  replyErrorFor(threadId: string): string | null;
  sendingReply(threadId: string): boolean;
  /** A thread's held replies, oldest first. */
  replyDraftsFor(threadId: string): readonly DiffReviewComment[];
  /** Held replies whose thread the PR no longer lists. */
  readonly orphanedReplyDrafts: readonly DiffReviewComment[];
}

export interface ReviewCommentActionsHost {
  readonly backend: BackendKey;
  scope(): DiffReviewScope;
  selectedCommitSHA(): string | null;
  ref(): PRRef | null;
  entityKey(): string | null;
  launch(): string;
  updateFailing(): boolean;
  /** This device may write to the forge here (`git:operate`). */
  forgeGranted(): boolean;
  /** Why acting on the PR is withheld now (the merge actions' gates). */
  prWithheld(): string | null;
  /** Drafts can be saved here and this device may save them. */
  canDraft(): boolean;
  drafts(): readonly DiffReviewComment[];
  threads(): readonly ReviewThread[];
  /** A snapshot of the PR's threads is loaded. */
  threadsLoaded(): boolean;
  sourceKey(): string;
  /** The PR head the diff on screen was loaded at. */
  loadedHeadSHA(): string;
  saveDraft(input: DiffReviewCommentInput): Promise<void>;
  /** Opens a draft's edit unless it is open. */
  editDraft(draft: DiffReviewComment): void;
  refreshThreads(): Promise<void>;
  showPostedComment(body: string): void;
}

export interface ReviewCommentActions {
  /** Its members keep their getters: merge with Object.defineProperties. */
  readonly state: ReviewCommentActionsState;
  readonly writes: CommentWrites;
  /** Posts a new comment's line (a range from `start`) now, keyed by its
   * editor; `onSettled` closes the editor once the answer is on screen.
   * Throws where comments cannot post now. */
  postLineComment(editorId: string, anchor: CommentAnchor, start: CommentAnchor | null, body: string, onSettled: () => void): Promise<void>;
  /** The thread an agent delivery quotes for a held reply, or undefined. */
  replyContext(comment: DiffReviewComment, threads: readonly ReviewThread[]): DiffReviewThreadContext | undefined;
  dispose(): void;
}

function userFacingError(err: unknown): string {
  if (err instanceof Error) return err.message;
  return typeof err === 'string' ? err : 'The forge did not take this reply.';
}

export function createReviewCommentActions(host: ReviewCommentActionsHost): ReviewCommentActions {
  const replyBodies = new SvelteMap<string, string>();
  // `${surface}:${threadId}` of each open reply composer.
  const openReplies = new SvelteSet<string>();
  const replyErrors = new SvelteMap<string, string>();
  const holding = new SvelteSet<string>();

  // A comment box posts to the PR now only on the whole PR's diff (a
  // picked commit's numbers would misanchor against the head) of a
  // backend with the comment actions; a held reply needs a draft home
  // too. The primary is "now" until the pane holds a draft for the PR,
  // then "Add draft": the batch is what the reader is building.
  const capable = $derived(getTransportHelloFor(host.backend)?.capabilities.includes(PR_COMMENT_ACTIONS) ?? false);
  const sentCommentActions = $derived(capable && host.scope() === 'pr' && host.ref() !== null);
  const canPostNow = $derived(sentCommentActions && host.selectedCommitSHA() === null);
  const canHoldReply = $derived(canPostNow && host.canDraft());
  const nowWithheldReason = $derived(host.forgeGranted() ? host.prWithheld() : 'Not granted to this device');
  const primaryCommentAction = $derived<'now' | 'draft'>(host.drafts().length === 0 ? 'now' : 'draft');
  // A thread's held replies, oldest first. A recovered delivery can leave
  // several; each shows.
  const replyDrafts = $derived.by(() => {
    const out = new Map<string, DiffReviewComment[]>();
    for (const draft of host.drafts()) {
      if (draft.replyThreadId) out.set(draft.replyThreadId, [...(out.get(draft.replyThreadId) ?? []), draft]);
    }
    for (const list of out.values()) list.sort((a, b) => a.createdAt - b.createdAt);
    return out;
  });
  // A held reply whose thread a loaded snapshot no longer lists: it stays
  // a draft (shown where it can be deleted) and is left out of a review.
  const orphanedReplyDrafts = $derived.by<readonly DiffReviewComment[]>(() => {
    if (!host.threadsLoaded() || replyDrafts.size === 0) return NO_DRAFTS;
    const live = new Set(host.threads().map((thread) => thread.id));
    return [...replyDrafts.values()].flat().filter((draft) => !live.has(draft.replyThreadId!));
  });

  const writes = createCommentWrites({
    backend: host.backend,
    ref: host.ref,
    entityKey: host.entityKey,
    launch: host.launch,
    updateFailing: host.updateFailing,
    withheld: () => nowWithheldReason,
    refreshThreads: host.refreshThreads,
    showPostedComment: host.showPostedComment,
  });

  function closeEverywhere(threadId: string): void {
    openReplies.delete(`diff:${threadId}`);
    openReplies.delete(`conversation:${threadId}`);
  }

  // The composer's text, once it is gone where it went: kept if the reader
  // changed it meanwhile.
  function sent(threadId: string, body: string): void {
    if ((replyBodies.get(threadId) ?? '').trim() === body) replyBodies.delete(threadId);
    closeEverywhere(threadId);
  }

  const state: ReviewCommentActionsState = {
    get canPostNow() { return canPostNow; },
    get nowWithheldReason() { return nowWithheldReason; },
    get sentCommentActions() { return sentCommentActions; },
    get canHoldReply() { return canHoldReply; },
    get primaryCommentAction() { return primaryCommentAction; },
    commentWritePending: (key) => writes.pending(key),
    commentWriteError: (key) => writes.error(key),
    sentComments: writes,

    replyBodyFor: (threadId) => replyBodies.get(threadId) ?? '',
    setReplyBody(threadId, body) { replyBodies.set(threadId, body); },
    replyOpen: (threadId, surface) => openReplies.has(`${surface}:${threadId}`),
    openReply(threadId, surface) {
      const held = replyDrafts.get(threadId)?.at(-1);
      if (held) host.editDraft(held);
      else openReplies.add(`${surface}:${threadId}`);
    },
    closeReply(threadId, surface) {
      // A reply being posted stays open, so a failure shows in it.
      if (writes.pending(commentWriteKey.reply(threadId))) return;
      openReplies.delete(`${surface}:${threadId}`);
    },
    // A reply posts now wherever the thread shows (a picked commit's too),
    // under the gates every now write has.
    async sendPRThreadReply(thread) {
      const body = (replyBodies.get(thread.id) ?? '').trim();
      if (!host.ref() || !body || holding.has(thread.id)) return;
      const first = thread.comments[0];
      if (!first) {
        replyErrors.set(thread.id, NO_ROOT);
        return;
      }
      replyErrors.delete(thread.id);
      await writes.run(
        commentWriteKey.reply(thread.id),
        (ref) => ReplyToPRThread(prReferenceWire(ref), thread.id, first.databaseID, body),
        () => sent(thread.id, body),
        nowWithheldReason,
      );
    },
    // The held reply renders at the thread's foot and goes with the batch.
    async holdPRThreadReply(thread) {
      const body = (replyBodies.get(thread.id) ?? '').trim();
      const sourceKey = host.sourceKey();
      if (!body || !canHoldReply || !sourceKey || holding.has(thread.id)) return;
      const first = thread.comments[0];
      if (!first) {
        replyErrors.set(thread.id, NO_ROOT);
        return;
      }
      holding.add(thread.id);
      replyErrors.delete(thread.id);
      try {
        await host.saveDraft({
          scope: 'pr', sourceKey, commitSha: host.loadedHeadSHA(), filePath: thread.path, side: 'reply',
          replyThreadId: thread.id, replyRootId: first.databaseID, selectedText: '', body,
        });
        sent(thread.id, body);
      } catch (err) {
        replyErrors.set(thread.id, userFacingError(err));
      } finally {
        holding.delete(thread.id);
      }
    },
    replyErrorFor: (threadId) => replyErrors.get(threadId) ?? writes.error(commentWriteKey.reply(threadId)),
    sendingReply: (threadId) => holding.has(threadId) || writes.pending(commentWriteKey.reply(threadId)),
    replyDraftsFor: (threadId) => replyDrafts.get(threadId) ?? NO_DRAFTS,
    get orphanedReplyDrafts() { return orphanedReplyDrafts; },
  };

  return {
    state,
    writes,
    async postLineComment(editorId, anchor, start, body, onSettled) {
      if (!canPostNow) throw new Error('Comments post to the pull request only on its whole diff.');
      const request = {
        headSha: host.loadedHeadSHA(), filePath: anchor.filePath, side: anchor.side, oldLine: anchor.oldLine, newLine: anchor.newLine,
        startOldLine: start?.oldLine, startNewLine: start?.newLine, body,
      };
      await writes.run(commentWriteKey.editor(editorId), (ref) => PostPRLineComment(prReferenceWire(ref), request), onSettled);
    },
    // A held reply quotes the thread it answers, as the pane shows it. A
    // range from an old line to a new one is quoted by its last line: one
    // span of numbers cannot name both halves.
    replyContext(comment, threads) {
      const thread = threads.find((each) => each.id === comment.replyThreadId);
      if (!thread) return undefined;
      return {
        path: thread.path || undefined,
        line: thread.line ?? thread.originalLine ?? undefined,
        startLine: threadRange(thread)?.crossesHalves ? undefined : thread.startLine ?? undefined,
        comments: thread.comments.map((each) => ({ author: each.authorLogin, body: each.body })),
      };
    },
    dispose: () => writes.dispose(),
  };
}
