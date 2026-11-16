import type { Attachment } from '../types/attachment';
import type { DraftSnapshot, TerminalChip } from '../types/draft';
import type { SourceProposedPlan } from '../types/models';

const MAX_CACHED_DRAFTS = 100;

export interface ComposerDraftSnapshot {
  content: string;
  attachments: Attachment[];
  terminalChips: TerminalChip[];
  sourceProposedPlan: SourceProposedPlan | null;
}

// Unsaved local edits are kept here so a fast A -> B -> A switch can restore
// immediately even before the debounce write reaches SQLite. Entries are
// removed once their snapshot is durably saved, explicitly cleared, or evicted
// from the bounded LRU. Writes share one per-thread tail so saves cannot overtake each other and
// send/replacement operations can fence the writes admitted before them.
// The empty-draft cleanup joins the same tail: once it deletes the thread,
// no write for that id is sent again.
const localDraftSnapshots = new Map<string, ComposerDraftSnapshot>();
const MAX_DELETED_THREADS = 100;
const deletedDraftThreads = new Set<string>();
interface DraftWrite {
  write: () => Promise<unknown>;
  replaceable: boolean;
  cleanup: boolean;
  promise: Promise<boolean>;
  resolve: (written: boolean) => void;
  reject: (error: unknown) => void;
}
interface DraftWriter { active?: DraftWrite; pending: DraftWrite[]; }
const draftWriters = new Map<string, DraftWriter>();

function cloneSourceProposedPlan(source: SourceProposedPlan | null): SourceProposedPlan | null {
  return source ? { ...source } : null;
}

export function cloneDraftSnapshot(snapshot: ComposerDraftSnapshot): ComposerDraftSnapshot {
  return {
    content: snapshot.content,
    attachments: snapshot.attachments.map((attachment) => ({ ...attachment })),
    terminalChips: snapshot.terminalChips.map((chip) => ({ ...chip })),
    sourceProposedPlan: cloneSourceProposedPlan(snapshot.sourceProposedPlan),
  };
}

/**
 * Compares only the fields persisted through SaveDraft. Attachment metadata
 * such as filename is intentionally ignored; draft rows persist attachment
 * IDs, and fresh metadata is loaded from ListAttachments.
 */
export function draftSnapshotMatchesPersistedState(
  a: ComposerDraftSnapshot,
  b: ComposerDraftSnapshot,
): boolean {
  return draftTextAndContextMatch(a, b)
    && a.attachments.length === b.attachments.length
    && a.attachments.every((attachment, index) => attachment.id === b.attachments[index]?.id);
}

/** Attachment identities change on a move; text and captured context do not. */
export function draftTextAndContextMatch(
  a: Pick<DraftSnapshot, 'content' | 'terminalChips' | 'sourceProposedPlan'>,
  b: Pick<DraftSnapshot, 'content' | 'terminalChips' | 'sourceProposedPlan'>,
): boolean {
  return a.content === b.content
    && a.sourceProposedPlan?.threadId === b.sourceProposedPlan?.threadId
    && a.sourceProposedPlan?.itemId === b.sourceProposedPlan?.itemId
    && a.sourceProposedPlan?.payloadId === b.sourceProposedPlan?.payloadId
    && a.sourceProposedPlan?.title === b.sourceProposedPlan?.title
    && a.terminalChips.length === b.terminalChips.length
    && a.terminalChips.every((chip, index) => {
      const other = b.terminalChips[index];
      return chip.id === other?.id
        && chip.label === other.label
        && chip.preview === other.preview
        && chip.content === other.content
        && chip.createdAt === other.createdAt;
    });
}

export function getRememberedDraftSnapshot(threadId: string): ComposerDraftSnapshot | undefined {
  const snapshot = localDraftSnapshots.get(threadId);
  return snapshot ? cloneDraftSnapshot(snapshot) : undefined;
}

export function getRememberedDraftSnapshotForStore(threadId: string): ComposerDraftSnapshot | undefined {
  return localDraftSnapshots.get(threadId);
}

/**
 * Does this client hold composer work for the thread that the durable row
 * does not have yet? An entry exists only between a mutation and the save
 * that persists it, so this is exactly "there is unsaved text here".
 *
 * Read by the `draft:updated` applier, which must not pull another client's
 * text over a buffer the user is still typing into. That client's write is
 * not the last one yet — this client's pending save is.
 */
export function hasRememberedDraftSnapshot(threadId: string): boolean {
  return localDraftSnapshots.has(threadId);
}

export function rememberDraftSnapshot(threadId: string, snapshot: ComposerDraftSnapshot): void {
  if (localDraftSnapshots.has(threadId)) {
    localDraftSnapshots.delete(threadId);
  }
  localDraftSnapshots.set(threadId, cloneDraftSnapshot(snapshot));
  if (localDraftSnapshots.size <= MAX_CACHED_DRAFTS) return;

  const oldestThreadId = localDraftSnapshots.keys().next().value as string | undefined;
  if (oldestThreadId) {
    localDraftSnapshots.delete(oldestThreadId);
  }
}

export function forgetDraftSnapshot(threadId: string): void {
  localDraftSnapshots.delete(threadId);
}

export function forgetDraftSnapshotIfMatches(
  threadId: string,
  snapshot: ComposerDraftSnapshot,
): void {
  const current = localDraftSnapshots.get(threadId);
  if (current && draftSnapshotMatchesPersistedState(current, snapshot)) {
    localDraftSnapshots.delete(threadId);
  }
}

function startDraftWrite(threadId: string, owner: DraftWriter, operation: DraftWrite): void {
  owner.active = operation;
  const finish = () => {
    owner.active = undefined;
    if (draftWriters.get(threadId) !== owner) {
      for (const pending of owner.pending.splice(0)) pending.resolve(false);
      return;
    }
    const next = owner.pending.shift();
    if (next) startDraftWrite(threadId, owner, next);
    else draftWriters.delete(threadId);
  };
  // Start the first write synchronously: send preparation admits its snapshot
  // before clearing locally, so new typing always queues behind it.
  let result: Promise<unknown>;
  try { result = operation.write(); }
  catch (error) { result = Promise.reject(error); }
  void Promise.resolve(result).then(
    () => { operation.resolve(true); finish(); },
    (error) => { operation.reject(error); finish(); },
  );
}

/** A draft write for a thread the empty-draft cleanup deleted. Never sent. */
export class DraftThreadDeletedError extends Error {
  constructor(readonly threadId: string) {
    super('This new thread was removed before the text typed into it was saved.');
    this.name = 'DraftThreadDeletedError';
  }
}

/**
 * Autosaves coalesce; captured send writes and the empty-draft cleanup are
 * ordering boundaries. Rejects with DraftThreadDeletedError, without calling
 * `write`, once the cleanup has deleted the thread.
 */
export function queueDraftSave(
  threadId: string,
  write: () => Promise<unknown>,
  kind: 'autosave' | 'send' | 'cleanup' = 'autosave',
): Promise<boolean> {
  if (deletedDraftThreads.has(threadId)) {
    localDraftSnapshots.delete(threadId);
    return Promise.reject(new DraftThreadDeletedError(threadId));
  }
  let resolve!: DraftWrite['resolve'];
  let reject!: DraftWrite['reject'];
  const promise = new Promise<boolean>((done, fail) => { resolve = done; reject = fail; });
  const operation: DraftWrite = { write, replaceable: kind === 'autosave', cleanup: kind === 'cleanup', promise, resolve, reject };
  let owner = draftWriters.get(threadId);
  if (!owner) { owner = { pending: [] }; draftWriters.set(threadId, owner); }
  if (!owner.active) startDraftWrite(threadId, owner, operation);
  else {
    const previous = owner.pending.at(-1);
    if (operation.replaceable && previous?.replaceable) {
      owner.pending[owner.pending.length - 1] = operation;
      previous.resolve(false);
    } else owner.pending.push(operation);
  }
  return promise;
}

/**
 * Run the empty-draft cleanup behind every write admitted before it. Writes
 * admitted after it wait for its answer; `run` settles them through
 * markDraftThreadDeleted or abandonDraftThreadWrites before it returns.
 */
export async function queueDraftThreadCleanup<T>(threadId: string, run: () => Promise<T>): Promise<T> {
  let answer!: T;
  if (!(await queueDraftSave(threadId, async () => { answer = await run(); }, 'cleanup'))) {
    throw new Error('The empty-draft cleanup was dropped before it ran.');
  }
  return answer;
}

/** Unsaved local edits for the thread with something in them, if any. */
export function unsavedDraftSnapshot(threadId: string): ComposerDraftSnapshot | undefined {
  const snapshot = localDraftSnapshots.get(threadId);
  if (!snapshot) return undefined;
  const hasContent = snapshot.content.trim() !== '' || snapshot.attachments.length > 0
    || snapshot.terminalChips.length > 0 || snapshot.sourceProposedPlan !== null;
  return hasContent ? cloneDraftSnapshot(snapshot) : undefined;
}

/**
 * The thread is gone: reject the writes waiting behind its cleanup and every
 * later one. Ids are never reused, so the bounded record only has to outlive
 * the composers that held the thread. Returns the unsaved local edits it
 * held, for the caller to keep in another thread.
 */
export function markDraftThreadDeleted(threadId: string): ComposerDraftSnapshot | undefined {
  const unsaved = unsavedDraftSnapshot(threadId);
  deletedDraftThreads.add(threadId);
  if (deletedDraftThreads.size > MAX_DELETED_THREADS) {
    deletedDraftThreads.delete(deletedDraftThreads.values().next().value as string);
  }
  localDraftSnapshots.delete(threadId);
  const owner = draftWriters.get(threadId);
  if (owner) {
    for (const pending of owner.pending.splice(0)) pending.reject(new DraftThreadDeletedError(threadId));
    if (!owner.active) draftWriters.delete(threadId);
  }
  return unsaved;
}

/**
 * The thread may hold another client's draft: drop this client's
 * waiting writes so they cannot overwrite it, and return the unsaved edits
 * they carried for the caller to keep in another thread. A cleanup queued
 * behind them writes nothing, so it stays and runs in its turn.
 */
export function abandonDraftThreadWrites(threadId: string): ComposerDraftSnapshot | undefined {
  const unsaved = unsavedDraftSnapshot(threadId);
  localDraftSnapshots.delete(threadId);
  const owner = draftWriters.get(threadId);
  if (owner) {
    const cleanups: DraftWrite[] = [];
    for (const pending of owner.pending.splice(0)) {
      if (pending.cleanup) cleanups.push(pending);
      else pending.resolve(false);
    }
    owner.pending.push(...cleanups);
    if (!owner.active) {
      const next = owner.pending.shift();
      if (next) startDraftWrite(threadId, owner, next);
      else draftWriters.delete(threadId);
    }
  }
  return unsaved;
}

/** A finite fence over writes already admitted, never extended by later typing. */
export async function waitForActiveDraftSaves(threadId: string): Promise<void> {
  const owner = draftWriters.get(threadId);
  const through = owner?.pending.at(-1) ?? owner?.active;
  if (through) through.replaceable = false;
  await through?.promise.catch(() => {});
}

export function resetComposerDraftSnapshotStateForTest(): void {
  localDraftSnapshots.clear();
  draftWriters.clear();
  deletedDraftThreads.clear();
}
