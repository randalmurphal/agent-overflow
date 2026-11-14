import { SvelteMap, SvelteSet } from 'svelte/reactivity';
import { untrack } from 'svelte';
import { peekPRSeq } from './prReviewStore.svelte';
import { DeletePRComment, EditPRComment, PostPRComment } from './bindings';
import { toastLostWrite, writeFailure, type LostWriteText } from './lostWrite';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { prReferenceWire, type PRRef } from '../utils/prReference';
import type { ReviewComment, ReviewThread } from '../types/models';

// The review pane's writes that post to the forge now: a comment on a
// line, a range or a file, a reply, a comment on the PR itself, and an
// edit or delete of a sent comment. Nothing is optimistic. A write's
// editor stays open and reads as pending from its call until the pane has
// applied the PR frame the write answered with (`PRActionResult.seq`, the
// pump's sequence after the poll the write asked for); then the forge's
// answer is on screen and the editor closes. A failure keeps the editor
// and its text and shows the forge's one sentence in place. A write the
// pane's gates withhold (`withheld`) makes no call.
//
// A write with text whose pane went while it was in flight, and that then
// fails, has no editor left: one toast says so, stays until dismissed, and
// copies the text (lostWrite.ts). Nothing is kept for a later pane.
//
// Also the state of the sent-comment actions: the text of an open edit
// (the row is virtualized, so it lives here like every other editor) and
// the delete waiting for its in-place confirm.

export interface CommentWritesHost {
  readonly backend: BackendKey;
  /** The PR the pane shows, or null outside pr scope. */
  ref(): PRRef | null;
  /** The PR's entity key on its computer, whose applied seq settles a write. */
  entityKey(): string | null;
  /** The backend process's launch: a restarted pump numbers from one. */
  launch(): string;
  /** The pump is failing: its next answer is unknown, so a wait ends. */
  updateFailing(): boolean;
  /** Why a write to the PR is withheld now, or null. */
  withheld(): string | null;
  /** Reads the PR's threads again, for an answer with no pump (seq 0). */
  refreshThreads(): Promise<void>;
  /** Shows the PR comment the viewer posted (`commentId`, the forge's id
   * for it as the read names it): it counts as seen in the Conversation
   * feed, never in "N new", and the section scrolls it into view. */
  showPostedComment(commentId: string): void;
}

interface Awaited {
  /** The PR entity whose applied seq settles it. */
  readonly entity: string;
  readonly seq: number;
  readonly launch: string;
  readonly onSettled: (shown: Promise<void>) => void;
}

/** A write's text, for the toast when it fails after its pane is gone. */
export type CommentWriteText = LostWriteText;

/** `src/a.ts:12`, the place a thread names, or the pull request. */
export function threadWhere(thread: Pick<ReviewThread, 'path' | 'line' | 'originalLine'>): string {
  if (!thread.path) return 'the pull request';
  const line = thread.line ?? thread.originalLine;
  return line ? `${thread.path}:${line}` : thread.path;
}

/** What each write is keyed by, so its editor reads its state. */
export const commentWriteKey = {
  editor: (editorId: string) => `editor:${editorId}`,
  reply: (threadId: string) => `reply:${threadId}`,
  edit: (threadId: string, commentId: string) => `edit:${threadId}\u0000${commentId}`,
  delete: (threadId: string, commentId: string) => `delete:${threadId}\u0000${commentId}`,
  prComment: 'pr-comment',
} as const;

/** The thread a write's key belongs to: every key of a thread's write
 * starts `<kind>:<threadId>` (a reply, an edit, a delete, and
 * reviewForgeOps' reactions and suggestions). Null for a key that is
 * no thread's (a new comment's editor, the PR comment). */
export function threadOfWriteKey(key: string): string | null {
  const colon = key.indexOf(':');
  if (colon < 0 || !THREAD_WRITE_KINDS.has(key.slice(0, colon))) return null;
  return key.slice(colon + 1).split('\u0000')[0] || null;
}

const THREAD_WRITE_KINDS: ReadonlySet<string> = new Set(['reply', 'edit', 'delete', 'reaction', 'reactions', 'suggestion']);

export interface CommentWrites {
  /** Runs `call` unless withheld; resolves true once it was accepted (the
   * settle may still be waiting for its frame), false when it failed or
   * was withheld. `onSettled` runs once the editor may close: when the
   * answer is on screen, or at once for a write with no seq to wait for,
   * whose answer is on screen when `shown` resolves (the threads' re-read).
   * `withheld` replaces the host's gate for a write that has its own.
   * `text` is the write's text: one that fails after dispose is toasted. */
  run(
    key: string, call: (ref: PRRef) => Promise<{ seq: number } | null | undefined>, onSettled: (shown: Promise<void>) => void,
    withheld?: string | null, text?: CommentWriteText,
  ): Promise<boolean>;
  /** In flight, or waiting for its frame. */
  pending(key: string): boolean;
  error(key: string): string | null;
  clearError(key: string): void;

  /** A sent comment open in its editor: the unsaved text, or null. */
  editBodyFor(thread: ReviewThread, comment: ReviewComment): string | null;
  /** Opens (a body), changes or, with null, closes a sent comment's edit;
   * an edit being saved stays open, so a failure shows in it. */
  setEditBody(thread: ReviewThread, comment: ReviewComment, body: string | null): void;
  /** The `threadId\0commentId` of every sent comment open in its editor. */
  readonly editing: ReadonlySet<string>;
  /** Threads with a reply, edit or delete in flight or waiting for its
   * frame, a sent comment open in its editor, or a delete being
   * confirmed. A failed write's surface is one of these or the reply's
   * kept text, so its failure needs no entry of its own. */
  readonly activeThreads: ReadonlySet<string>;
  saveEdit(thread: ReviewThread, comment: ReviewComment): Promise<void>;
  confirmingDelete(thread: ReviewThread, comment: ReviewComment): boolean;
  setConfirmDelete(thread: ReviewThread, comment: ReviewComment, confirming: boolean): void;
  deleteComment(thread: ReviewThread, comment: ReviewComment): Promise<void>;

  /** The Conversation's "Comment…" field: a comment on the PR itself. */
  readonly prCommentBody: string;
  setPRCommentBody(body: string): void;
  readonly prCommentOpen: boolean;
  setPRCommentOpen(open: boolean): void;
  postPRComment(): Promise<void>;

  dispose(): void;
}

function sentKey(thread: ReviewThread, comment: ReviewComment): string {
  return `${thread.id}\u0000${comment.id}`;
}

export function createCommentWrites(host: CommentWritesHost): CommentWrites {
  const inFlight = new SvelteSet<string>();
  const awaiting = new SvelteMap<string, Awaited>();
  // A write's failure lives exactly as long as the surface that shows
  // it: a reply's while its composer is open or its text kept
  // (reviewCommentActions drops it otherwise, and when the reply is sent
  // or held), an edit's while its editor is open, a delete's while its
  // confirm is. Dismissing the surface clears it, so a failure stored is
  // a failure shown.
  const errors = new SvelteMap<string, string>();
  const editBodies = new SvelteMap<string, string>();
  const confirming = new SvelteSet<string>();
  let prCommentBody = $state('');
  let prCommentOpen = $state(false);
  let disposed = false;

  // A wait ends when the frame at its seq is applied, the backend that
  // numbered it is gone, or the pump is failing (its next answer is
  // unknown, and the pane says so).
  const disposeSettle = $effect.root(() => {
    $effect(() => {
      const launch = host.launch();
      const failing = host.updateFailing();
      const done: [string, Awaited][] = [];
      for (const [key, wait] of awaiting) {
        if (launch !== wait.launch || failing || peekPRSeq(wait.entity) >= wait.seq) done.push([key, wait]);
      }
      if (done.length === 0) return;
      untrack(() => {
        for (const [key, wait] of done) {
          awaiting.delete(key);
          wait.onSettled(Promise.resolve());
        }
      });
    });
  });

  const activeThreads = $derived.by<ReadonlySet<string>>(() => {
    const out = new Set<string>();
    for (const keys of [inFlight, awaiting.keys()]) {
      for (const key of keys) {
        const thread = threadOfWriteKey(key);
        if (thread !== null) out.add(thread);
      }
    }
    for (const keys of [editBodies.keys(), confirming]) for (const key of keys) out.add(key.split('\u0000')[0]);
    return out;
  });

  function isPending(key: string): boolean {
    return inFlight.has(key) || awaiting.has(key);
  }

  async function run(
    key: string,
    call: (ref: PRRef) => Promise<{ seq: number } | null | undefined>,
    onSettled: (shown: Promise<void>) => void,
    gate: string | null = host.withheld(),
    text?: CommentWriteText,
  ): Promise<boolean> {
    if (isPending(key)) return false;
    const ref = host.ref();
    const entity = host.entityKey();
    const withheld = gate;
    if (!ref || !entity || withheld !== null) {
      errors.set(key, withheld ?? 'This review is not on a pull request.');
      return false;
    }
    const launch = host.launch();
    inFlight.add(key);
    errors.delete(key);
    let seq: number;
    try {
      // An older backend's reply answers nothing: read as no pump.
      seq = (await withBackendTarget(host.backend, () => call(ref)))?.seq ?? 0;
    } catch (err) {
      if (!disposed) errors.set(key, writeFailure(err).sentence);
      else if (text) toastLostWrite(text, err);
      return false;
    } finally {
      inFlight.delete(key);
    }
    if (disposed) return true;
    if (seq <= 0) {
      // Nothing to wait for (no pump, or its refresh was not observed):
      // the editor closes now, and a read of the threads brings the answer.
      onSettled(host.refreshThreads().catch(() => {}));
      return true;
    }
    if (host.launch() !== launch || peekPRSeq(entity) >= seq) {
      onSettled(Promise.resolve());
      return true;
    }
    awaiting.set(key, { entity, seq, launch, onSettled });
    return true;
  }

  return {
    run,
    pending: isPending,
    error: (key) => errors.get(key) ?? null,
    clearError: (key) => { errors.delete(key); },

    editBodyFor: (thread, comment) => editBodies.get(sentKey(thread, comment)) ?? null,
    setEditBody(thread, comment, body) {
      const key = sentKey(thread, comment);
      if (body === null) {
        if (isPending(commentWriteKey.edit(thread.id, comment.id))) return;
        editBodies.delete(key);
        errors.delete(commentWriteKey.edit(thread.id, comment.id));
      } else {
        editBodies.set(key, body);
      }
    },
    get editing() { return new Set(editBodies.keys()); },
    get activeThreads() { return activeThreads; },
    async saveEdit(thread, comment) {
      const key = sentKey(thread, comment);
      const body = (editBodies.get(key) ?? '').trim();
      if (!body) return;
      await run(
        commentWriteKey.edit(thread.id, comment.id),
        (ref) => EditPRComment(prReferenceWire(ref), { threadId: thread.id, commentId: comment.id }, body),
        () => { editBodies.delete(key); },
        undefined,
        { body, subject: `Your edit to the comment on ${threadWhere(thread)}` },
      );
    },
    confirmingDelete: (thread, comment) => confirming.has(sentKey(thread, comment)),
    setConfirmDelete(thread, comment, on) {
      const key = sentKey(thread, comment);
      if (on) confirming.add(key);
      else {
        // A delete that is out keeps its confirm: its answer shows there.
        if (isPending(commentWriteKey.delete(thread.id, comment.id))) return;
        confirming.delete(key);
        errors.delete(commentWriteKey.delete(thread.id, comment.id));
      }
    },
    async deleteComment(thread, comment) {
      const key = sentKey(thread, comment);
      await run(
        commentWriteKey.delete(thread.id, comment.id),
        (ref) => DeletePRComment(prReferenceWire(ref), { threadId: thread.id, commentId: comment.id }),
        () => {
          confirming.delete(key);
          editBodies.delete(key);
        },
      );
    },

    get prCommentBody() { return prCommentBody; },
    setPRCommentBody(body) { prCommentBody = body; },
    get prCommentOpen() { return prCommentOpen; },
    setPRCommentOpen(open) {
      // A comment being posted stays open, so a failure shows in it.
      if (!open && isPending(commentWriteKey.prComment)) return;
      prCommentOpen = open;
    },
    async postPRComment() {
      const body = prCommentBody.trim();
      if (!body) return;
      // The answer names the created comment; one it does not name (an
      // older backend) joins the feed as any arrival does.
      let created = '';
      await run(commentWriteKey.prComment, async (ref) => {
        const answer = await PostPRComment(prReferenceWire(ref), body);
        created = answer?.commentId ?? '';
        return answer;
      }, (shown) => {
        if (prCommentBody.trim() === body) prCommentBody = '';
        prCommentOpen = false;
        void shown.then(() => { if (!disposed) host.showPostedComment(created); });
      }, undefined, { body, subject: 'Your comment on the pull request' });
    },

    dispose() {
      disposed = true;
      disposeSettle();
    },
  };
}
