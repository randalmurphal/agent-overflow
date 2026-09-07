import type { Attachment } from '../types/attachment';
import type { TerminalChip } from '../types/draft';
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
const localDraftSnapshots = new Map<string, ComposerDraftSnapshot>();
interface DraftWrite {
  write: () => Promise<unknown>;
  replaceable: boolean;
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
  return a.content === b.content
    && a.sourceProposedPlan?.threadId === b.sourceProposedPlan?.threadId
    && a.sourceProposedPlan?.itemId === b.sourceProposedPlan?.itemId
    && a.sourceProposedPlan?.payloadId === b.sourceProposedPlan?.payloadId
    && a.sourceProposedPlan?.title === b.sourceProposedPlan?.title
    && a.attachments.length === b.attachments.length
    && a.attachments.every((attachment, index) => attachment.id === b.attachments[index]?.id)
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

/** Autosaves coalesce; captured send writes are ordering boundaries. */
export function queueDraftSave(
  threadId: string,
  write: () => Promise<unknown>,
  kind: 'autosave' | 'send' = 'autosave',
): Promise<boolean> {
  let resolve!: DraftWrite['resolve'];
  let reject!: DraftWrite['reject'];
  const promise = new Promise<boolean>((done, fail) => { resolve = done; reject = fail; });
  const operation: DraftWrite = { write, replaceable: kind === 'autosave', promise, resolve, reject };
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
}
