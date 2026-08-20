// Composer ArrowUp history recall: shell-style walking of the thread's
// past user messages from the composer textarea.
//
// The caret gate (ArrowUp only at offset 0, ArrowDown only at the end)
// lives with the host's keydown claim; this module owns the session —
// which entry is showing, what the user had typed before the walk
// started, and when the session is over.
//
// A session's validity is ATTESTED, never event-tracked: it holds only
// while the draft's content is exactly what the session last painted on
// the same thread, and the draft carries no attachments or terminal
// chips. Anything else that writes the composer — a keystroke, a send
// clearing it, a thread switch blanking it, an interrupt-revert repaint,
// an upload or terminal chip landing — invalidates the session
// structurally, with no listener to forget. That is also what implements
// "modifying a recalled message takes over": the edit went through
// `setContent` (persisted, so it IS the draft now — and a chip or
// attachment landing queues the same save), and the next ArrowUp starts
// a fresh session with the edited text as its stash. Recall also never
// STARTS over a draft holding attachments or chips: a preview would
// strip the `[Image #n]` placeholders the attachment machinery
// reconciles against, and the first keystroke would delete the records.
//
// Persistence contract: the typed draft is flushed durably BEFORE the
// first preview paints, and previews go through
// `draft.applyHistoryPreview`, which never persists. A restart therefore
// always restores what the user typed, never a browsed entry.

import type { Item } from '../../types/models';
import { errString } from '../../utils/errors';
import { isReaderAuthoredUserText, stripAttachmentImages } from '../../utils/userMessageMeta';

/** One backend history row — `store.UserMessageHistoryEntry` on the wire. */
export interface RecallHistoryRow {
  id: string;
  turnIndex: number;
  itemIndex: number;
  summary: string;
}

/** How many past messages one session can walk. Also passed as the
 * backend read's limit; the merge below can only shrink the list. */
export const HISTORY_RECALL_LIMIT = 50;

/**
 * The pure half of the host's caret gate: which recall direction an
 * arrow keydown expresses, given the textarea's selection. ArrowUp
 * only from the very first position, ArrowDown only from the very
 * last, collapsed selection only, no modifiers — everywhere else the
 * native caret behavior keeps the key. IME composition and the
 * user-input prompt are the host's to check (they need the event's
 * environment, not its geometry).
 */
/** Where a paint parks the caret: the walk's leading edge. */
export type RecallCaret = 'start' | 'end';

export function recallArrowIntent(
  e: { key: string; shiftKey: boolean; ctrlKey: boolean; metaKey: boolean; altKey: boolean },
  sel: { start: number; end: number; valueLength: number },
): 'up' | 'down' | null {
  if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return null;
  if (e.shiftKey || e.ctrlKey || e.metaKey || e.altKey) return null;
  if (sel.start !== sel.end) return null;
  if (e.key === 'ArrowUp') return sel.start === 0 ? 'up' : null;
  return sel.end === sel.valueLength ? 'down' : null;
}

export interface ComposerHistoryRecallDeps {
  threadId(): string | null;
  draftContent(): string;
  /** True while the draft store still owes SQLite a save. */
  draftHasPendingSave(): boolean;
  /** True when the draft holds attachments or terminal chips. Recall
   * refuses to start over one (a preview would strip the `[Image #n]`
   * placeholders the attachment machinery reconciles against, and the
   * first keystroke would delete the attachment records), and one
   * LANDING mid-session — an upload completing, a terminal
   * send-to-composer — ends the session: it queues a save of what is
   * on screen, which is the same takeover an edit is. */
  draftHasAttachments(): boolean;
  /** Flush the typed draft durably (draft.flushPending). */
  flushDraft(): Promise<void>;
  /** Backend baseline, newest first. */
  fetchHistory(threadId: string): Promise<readonly RecallHistoryRow[]>;
  /** The pane's loaded window — covers optimistic just-sent rows the
   * backend read can still miss. */
  paneItems(): readonly Item[];
  /** Mid-turn sends not yet echoed as items (flushed + queued),
   * oldest first. Newer than every item. */
  pendingMessages(): readonly string[];
  /** Paint a preview into the composer (ephemeral). The caret parks at
   * the walk's leading edge — offset 0 going UP, the end going DOWN —
   * so a repeated press of the same arrow passes the caret gate and
   * keeps walking. */
  paint(text: string, caret: RecallCaret): void;
  reportError(message: string): void;
}

interface RecallSession {
  threadId: string;
  /** Newest first. Never empty, never a blank entry. */
  entries: readonly string[];
  /** Index of the entry currently painted. */
  index: number;
  /** The typed draft the walk started over; ArrowDown past the newest
   * entry restores it. */
  stash: string;
  /** What this session last wrote — the validity attestation. */
  lastPainted: string;
}

/**
 * Merge the three sources of "messages the reader sent" into the walk
 * list, newest first: pending mid-turn sends, then the loaded window's
 * reader-authored rows overlaid on the backend baseline by position
 * (window wins — it holds optimistic rows and any content the wire is
 * fresher on). Attachment-image blocks are stripped (recall is text
 * only; attachments stay whatever the draft holds), blank entries are
 * dropped, consecutive duplicates collapse, and an entry identical to
 * the stash never leads — the first ArrowUp must visibly change
 * something.
 */
export function buildRecallEntries(
  backendRows: readonly RecallHistoryRow[],
  paneItems: readonly Item[],
  pendingMessages: readonly string[],
  stash: string,
): string[] {
  const byPosition = new Map<string, RecallHistoryRow>();
  for (const row of backendRows) {
    byPosition.set(`${row.turnIndex}:${row.itemIndex}`, row);
  }
  for (const item of paneItems) {
    if (!isReaderAuthoredUserText(item)) continue;
    byPosition.set(`${item.turnIndex}:${item.itemIndex}`, {
      id: item.id,
      turnIndex: item.turnIndex,
      itemIndex: item.itemIndex,
      summary: item.summary,
    });
  }
  const itemRows = [...byPosition.values()].sort(
    (a, b) => b.turnIndex - a.turnIndex || b.itemIndex - a.itemIndex,
  );

  const texts: string[] = [];
  const push = (raw: string) => {
    // Recall is text-only, so both attachment shapes the send path wrote
    // into the summary go: the trailing image blocks and the inline
    // `[Image #n]` placeholder labels, which would otherwise recall as
    // dangling literals with no attachment behind them.
    const text = stripAttachmentImages(raw).replace(/\[Image #\d+\]/g, '');
    if (text.trim() === '') return;
    if (texts[texts.length - 1] === text) return;
    texts.push(text);
  };
  for (let i = pendingMessages.length - 1; i >= 0; i--) push(pendingMessages[i]);
  for (const row of itemRows) push(row.summary);

  if (texts[0] === stash) texts.shift();
  return texts.slice(0, HISTORY_RECALL_LIMIT);
}

/**
 * The per-composer recall controller. `arrowUp` / `arrowDown` are called
 * only when the host's caret gate passed; both return true when the
 * keystroke is recall's (the caller then preventDefaults).
 */
export function createComposerHistoryRecall(deps: ComposerHistoryRecallDeps) {
  let session: RecallSession | null = null;
  // Supersedes an in-flight session start: any newer ArrowUp, and any
  // start that lost its precondition while fetching, dies here.
  let startGeneration = 0;
  // A held ArrowUp auto-repeats faster than a slow link answers; while
  // one start is fetching, further ArrowUps are swallowed instead of
  // superseding it with an identical RPC each repeat.
  let startInFlight = false;

  function validSession(): RecallSession | null {
    if (!session) return null;
    if (session.threadId !== deps.threadId()) {
      session = null;
      return null;
    }
    if (deps.draftContent() !== session.lastPainted) {
      session = null;
      return null;
    }
    // An attachment or chip landed mid-walk without touching content.
    // Its own save persisted what is on screen — the same takeover an
    // edit is — so the session is over.
    if (deps.draftHasAttachments()) {
      session = null;
      return null;
    }
    return session;
  }

  function paintEntry(s: RecallSession, index: number, caret: RecallCaret): void {
    s.index = index;
    s.lastPainted = s.entries[index];
    deps.paint(s.lastPainted, caret);
  }

  async function startSession(threadId: string, stash: string, generation: number): Promise<void> {
    startInFlight = true;
    try {
      // The typed draft must be durable before a preview replaces it on
      // screen: from here on, SQLite is what "restored on restart" means.
      if (deps.draftHasPendingSave()) {
        await deps.flushDraft();
        if (deps.draftHasPendingSave()) return; // save failed (already surfaced); don't paint over an unsaved draft
      }
      let rows: readonly RecallHistoryRow[];
      try {
        rows = await deps.fetchHistory(threadId);
      } catch (err) {
        if (generation === startGeneration && deps.threadId() === threadId) {
          deps.reportError(`Failed to load message history: ${errString(err)}`);
        }
        return;
      }
      if (generation !== startGeneration) return;
      if (deps.threadId() !== threadId) return;
      if (deps.draftContent() !== stash) return; // the user typed while we fetched
      if (deps.draftHasAttachments()) return; // an upload/chip landed while we fetched
      const entries = buildRecallEntries(rows, deps.paneItems(), deps.pendingMessages(), stash);
      if (entries.length === 0) return;
      session = { threadId, entries, index: 0, stash, lastPainted: entries[0] };
      paintEntry(session, 0, 'start');
    } finally {
      startInFlight = false;
    }
  }

  return {
    /** ArrowUp with the caret at the very first position. */
    arrowUp(): boolean {
      const s = validSession();
      if (s) {
        // At the oldest entry the keystroke is still recall's — swallowed,
        // like a shell at the top of its history.
        if (s.index + 1 < s.entries.length) paintEntry(s, s.index + 1, 'start');
        return true;
      }
      // A start is already fetching (held-key auto-repeat): swallow the
      // repeat rather than superseding it with an identical RPC.
      if (startInFlight) return true;
      const threadId = deps.threadId();
      if (!threadId) return false;
      if (deps.draftHasAttachments()) return false;
      void startSession(threadId, deps.draftContent(), ++startGeneration);
      // Claimed even though the paint is async: the caret is already at
      // offset 0, so the native move this suppresses was a no-op anyway.
      return true;
    },

    /** ArrowDown with the caret at the very last position. */
    arrowDown(): boolean {
      const s = validSession();
      // No session — nothing below the message being typed. Deliberate
      // no-op, and unclaimed so the native (equally inert) behavior keeps
      // the keystroke.
      if (!s) return false;
      if (s.index === 0) {
        // Walked back past the newest entry: restore what the user had
        // typed and end the session.
        deps.paint(s.stash, 'end');
        session = null;
        return true;
      }
      paintEntry(s, s.index - 1, 'end');
      return true;
    },

    /** Test seam: whether a session currently attests valid. */
    hasActiveSession(): boolean {
      return validSession() !== null;
    },
  };
}

export type ComposerHistoryRecall = ReturnType<typeof createComposerHistoryRecall>;
