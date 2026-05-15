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
// from the bounded LRU. Active saves are tracked separately so external draft
// replacement can wait for in-flight writes before it overwrites the backend.
const localDraftSnapshots = new Map<string, ComposerDraftSnapshot>();
const activeSavePromises = new Map<string, Set<Promise<unknown>>>();

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

export function trackActiveDraftSave(threadId: string, promise: Promise<unknown>): void {
  let saves = activeSavePromises.get(threadId);
  if (!saves) {
    saves = new Set();
    activeSavePromises.set(threadId, saves);
  }
  saves.add(promise);
  const cleanup = () => {
    const current = activeSavePromises.get(threadId);
    if (!current) return;
    current.delete(promise);
    if (current.size === 0) {
      activeSavePromises.delete(threadId);
    }
  };
  promise.then(cleanup, cleanup);
}

export async function waitForActiveDraftSaves(threadId: string): Promise<void> {
  while (true) {
    const saves = activeSavePromises.get(threadId);
    if (!saves || saves.size === 0) return;
    await Promise.allSettled([...saves]);
  }
}

export function resetComposerDraftSnapshotStateForTest(): void {
  localDraftSnapshots.clear();
  activeSavePromises.clear();
}
