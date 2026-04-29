import type { Attachment } from '../types/attachment';
import type { Draft, TerminalChip } from '../types/draft';
import type { SourceProposedPlan } from '../types/models';
import {
  ClearDraft,
  GetDraft,
  ListAttachments,
  SaveDraft,
} from './bindings';
import { errString } from '../utils/errors';
import { ensureImagePlaceholders } from '../utils/imagePlaceholders';

const DEFAULT_DEBOUNCE_MS = 500;
const MAX_CACHED_DRAFTS = 100;

interface DraftStoreOptions {
  debounceMs?: number;
}

interface ComposerDraftSnapshot {
  content: string;
  attachments: Attachment[];
  terminalChips: TerminalChip[];
  sourceProposedPlan: SourceProposedPlan | null;
}

// Unsaved local edits are kept here so a fast A -> B -> A switch can restore
// immediately even before the debounce write reaches SQLite. Entries are
// removed once their snapshot is durably saved or cleared.
const localDraftSnapshots = new Map<string, ComposerDraftSnapshot>();

function cloneSourceProposedPlan(source: SourceProposedPlan | null): SourceProposedPlan | null {
  return source ? { ...source } : null;
}

function cloneSnapshot(snapshot: ComposerDraftSnapshot): ComposerDraftSnapshot {
  return {
    content: snapshot.content,
    attachments: snapshot.attachments.map((attachment) => ({ ...attachment })),
    terminalChips: snapshot.terminalChips.map((chip) => ({ ...chip })),
    sourceProposedPlan: cloneSourceProposedPlan(snapshot.sourceProposedPlan),
  };
}

function rememberDraftSnapshot(threadId: string, snapshot: ComposerDraftSnapshot): void {
  if (localDraftSnapshots.has(threadId)) {
    localDraftSnapshots.delete(threadId);
  }
  localDraftSnapshots.set(threadId, cloneSnapshot(snapshot));
  if (localDraftSnapshots.size <= MAX_CACHED_DRAFTS) return;
  const oldestThreadId = localDraftSnapshots.keys().next().value as string | undefined;
  if (oldestThreadId) {
    localDraftSnapshots.delete(oldestThreadId);
  }
}

export function resetComposerDraftSnapshotsForTest(): void {
  localDraftSnapshots.clear();
}

/**
 * Per-thread composer draft state. One instance manages its own timers and
 * requests; the caller is expected to call `setThread` whenever the active
 * thread changes so we can hydrate / flush correctly.
 */
export function createComposerDraftStore(options: DraftStoreOptions = {}) {
  const debounceMs = options.debounceMs ?? DEFAULT_DEBOUNCE_MS;

  let threadId: string | null = $state(null);
  let content: string = $state('');
  let attachments: Attachment[] = $state([]);
  let terminalChips: TerminalChip[] = $state([]);
  // sourceProposedPlan links the draft back to a proposed-plan item when the
  // draft was seeded by "Implement plan in new thread". The send path reads
  // it so the original plan is marked Accepted on first send; cleared
  // afterwards so subsequent turns in this thread don't re-mark.
  let sourceProposedPlan: SourceProposedPlan | null = $state(null);
  let hydrating: boolean = $state(false);
  let error: string | null = $state(null);

  let debounceTimer: ReturnType<typeof setTimeout> | null = null;
  let pendingSaveGeneration = 0;
  let switchGeneration = 0;
  let hasPendingSave = false;

  function clearDebounce() {
    if (debounceTimer) {
      clearTimeout(debounceTimer);
      debounceTimer = null;
    }
  }

  function snapshotsMatch(a: ComposerDraftSnapshot, b: ComposerDraftSnapshot): boolean {
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

  function clearLocalSnapshotIfCurrent(id: string, savedSnapshot: ComposerDraftSnapshot): void {
    const current = localDraftSnapshots.get(id);
    if (current && snapshotsMatch(current, savedSnapshot)) {
      localDraftSnapshots.delete(id);
    }
    if (threadId === id && snapshotsMatch(buildSnapshot(), savedSnapshot)) {
      hasPendingSave = false;
    }
  }

  async function saveSnapshot(id: string, snapshot: ComposerDraftSnapshot, failureLabel: string): Promise<void> {
    try {
      await SaveDraft(
        id,
        snapshot.content,
        snapshot.attachments.map((a) => a.id),
        snapshot.terminalChips,
        snapshot.sourceProposedPlan,
      );
      clearLocalSnapshotIfCurrent(id, snapshot);
    } catch (err) {
      rememberDraftSnapshot(id, snapshot);
      if (threadId === id && snapshotsMatch(buildSnapshot(), snapshot)) {
        hasPendingSave = true;
      }
      error = `${failureLabel}: ${errString(err)}`;
      throw err;
    }
  }

  async function loadAttachments(id: string): Promise<Attachment[]> {
    const records = (await ListAttachments(id)) as Attachment[] | null;
    return records ?? [];
  }

  async function hydrate(id: string, expectedGeneration: number): Promise<void> {
    hydrating = true;
    error = null;
    const cached = localDraftSnapshots.get(id);
    if (cached) {
      applySnapshot(cached);
      hasPendingSave = true;
    }
    try {
      const [draft, records] = await Promise.all([
        GetDraft(id) as Promise<Draft>,
        loadAttachments(id),
      ]);
      if (threadId !== id || switchGeneration !== expectedGeneration) return; // thread switched while loading
      const currentCached = localDraftSnapshots.get(id);
      if (currentCached) {
        applySnapshot(currentCached);
        return;
      }

      const attachmentIds = new Set(draft.attachmentIds ?? []);
      const draftAttachments = records.filter((rec) => attachmentIds.has(rec.id));
      const snapshot: ComposerDraftSnapshot = {
        content: ensureImagePlaceholders(draft.content ?? '', draftAttachments),
        attachments: draftAttachments,
        terminalChips: draft.terminalChips ?? [],
        sourceProposedPlan: draft.sourceProposedPlan ?? null,
      };
      applySnapshot(snapshot);
    } catch (err) {
      if (threadId === id && switchGeneration === expectedGeneration) {
        error = `Failed to load draft: ${errString(err)}`;
      }
    } finally {
      if (threadId === id && switchGeneration === expectedGeneration) {
        hydrating = false;
      }
    }
  }

  function queueSave(): void {
    if (!threadId) return;
    clearDebounce();
    const id = threadId;
    const generation = ++pendingSaveGeneration;
    const snapshot = buildSnapshot();
    hasPendingSave = true;
    debounceTimer = setTimeout(async () => {
      debounceTimer = null;
      if (threadId !== id || generation !== pendingSaveGeneration) return;
      try {
        await saveSnapshot(id, snapshot, 'Failed to save draft');
        if (threadId === id && generation === pendingSaveGeneration) {
          hasPendingSave = false;
        }
      } catch {
        // saveSnapshot already recorded the user-facing error and retained
        // the unsaved local snapshot.
      }
    }, debounceMs);
  }

  async function flush(): Promise<void> {
    if (!threadId) return;
    clearDebounce();
    const id = threadId;
    const snapshot = buildSnapshot();
    try {
      await saveSnapshot(id, snapshot, 'Failed to save draft');
      hasPendingSave = false;
    } catch {
      // saveSnapshot already recorded the user-facing error and retained
      // the unsaved local snapshot.
    }
  }

  async function flushPending(): Promise<void> {
    if (!hasPendingSave) return;
    await flush();
  }

  function buildSnapshot(): ComposerDraftSnapshot {
    return {
      content,
      attachments,
      terminalChips,
      sourceProposedPlan,
    };
  }

  function applySnapshot(snapshot: ComposerDraftSnapshot): void {
    const cloned = cloneSnapshot(snapshot);
    content = cloned.content;
    attachments = cloned.attachments;
    terminalChips = cloned.terminalChips;
    sourceProposedPlan = cloned.sourceProposedPlan;
  }

  function rememberCurrentDraft(): void {
    if (!threadId) return;
    rememberDraftSnapshot(threadId, buildSnapshot());
  }

  async function setThread(id: string | null): Promise<void> {
    if (threadId === id) return;
    const previousId = threadId;
    const previousSnapshot = previousId && hasPendingSave ? buildSnapshot() : null;
    clearDebounce();
    pendingSaveGeneration++;
    const generation = ++switchGeneration;
    if (previousId && previousSnapshot) {
      rememberDraftSnapshot(previousId, previousSnapshot);
      void saveSnapshot(previousId, previousSnapshot, 'Failed to save draft')
        .catch(() => {
          // saveSnapshot keeps the local snapshot and error state.
        });
    }
    threadId = id;
    content = '';
    attachments = [];
    terminalChips = [];
    sourceProposedPlan = null;
    hasPendingSave = false;
    error = null;
    hydrating = false;
    if (id) {
      await hydrate(id, generation);
      if (generation !== switchGeneration || threadId !== id) return;
    }
  }

  return {
    // ---- reads ----
    get threadId() { return threadId; },
    get content() { return content; },
    get attachments() { return attachments; },
    get terminalChips() { return terminalChips; },
    get sourceProposedPlan() { return sourceProposedPlan; },
    get hydrating() { return hydrating; },
    get error() { return error; },
    get hasPendingSave() { return hasPendingSave; },
    get hasDraft() {
      return content.trim().length > 0 || attachments.length > 0 || terminalChips.length > 0;
    },

    // ---- thread lifecycle ----
    setThread,
    flush,
    flushPending,

    // ---- content mutations ----
    setContent(next: string): void {
      content = next;
      rememberCurrentDraft();
      queueSave();
    },

    setContentAndAttachments(nextContent: string, nextAttachments: Attachment[]): void {
      content = nextContent;
      attachments = [...nextAttachments];
      rememberCurrentDraft();
      queueSave();
    },

    removeAttachment(id: string): void {
      const next = attachments.filter((a) => a.id !== id);
      if (next.length === attachments.length) return;
      attachments = next;
      rememberCurrentDraft();
      queueSave();
    },

    addAttachment(attachment: Attachment): void {
      if (attachments.some((a) => a.id === attachment.id)) return;
      attachments = [...attachments, attachment];
      rememberCurrentDraft();
      queueSave();
    },

    addTerminalChip(chip: TerminalChip): void {
      if (terminalChips.some((c) => c.id === chip.id)) return;
      terminalChips = [...terminalChips, chip];
      rememberCurrentDraft();
      queueSave();
    },

    removeTerminalChip(id: string): void {
      const next = terminalChips.filter((c) => c.id !== id);
      if (next.length === terminalChips.length) return;
      terminalChips = next;
      rememberCurrentDraft();
      queueSave();
    },

    setError(message: string | null): void {
      error = message;
    },

    /**
     * Called after a successful Send. Clears local state and the backend
     * row so the thread re-loads empty next time. The source-plan ref is
     * cleared too — the linkage was consumed by the send and any future
     * turn in this thread should be a regular turn, not "implementing the
     * plan again."
     */
    async clearAfterSend(): Promise<void> {
      const id = threadId;
      clearDebounce();
      pendingSaveGeneration++;
      content = '';
      attachments = [];
      terminalChips = [];
      sourceProposedPlan = null;
      hasPendingSave = false;
      if (id) {
        localDraftSnapshots.delete(id);
      }
      if (!id) return;
      try {
        await ClearDraft(id);
      } catch (err) {
        error = `Failed to clear draft: ${errString(err)}`;
      }
    },

    /**
     * Restore a draft snapshot to a specific thread. Used when a send
     * rejects AFTER the user has switched panes: we don't want to silently
     * dump thread A's failed message into thread B's composer. If the draft
     * store is still on the given thread we also restore the local UI state
     * so the composer re-populates immediately; otherwise the backend row
     * is repopulated so the user sees the draft next time they return.
     */
    async restoreDraftFor(
      id: string,
      snapshot: {
        content: string;
        attachments: Attachment[];
        terminalChips: TerminalChip[];
        sourceProposedPlan?: SourceProposedPlan | null;
      },
    ): Promise<void> {
      // Persist the snapshot back to the backend regardless of active thread
      // so the draft lives across thread switches.
      const restoredSnapshot: ComposerDraftSnapshot = {
        content: snapshot.content,
        attachments: snapshot.attachments,
        terminalChips: snapshot.terminalChips,
        sourceProposedPlan: snapshot.sourceProposedPlan ?? null,
      };
      try {
        await saveSnapshot(id, restoredSnapshot, 'Failed to restore draft');
      } catch {
        return;
      }
      // If the store is still pointed at the same thread, mirror the
      // snapshot into the local state so the composer shows it right away.
      if (threadId === id) {
        clearDebounce();
        pendingSaveGeneration++;
        applySnapshot(restoredSnapshot);
        hasPendingSave = false;
      }
    },

    /**
     * Build the outgoing text payload for Send. Terminal chip contents
     * are inlined as fenced blocks; image attachments travel separately as
     * structured attachment ids so providers receive real image inputs.
     */
    composeOutgoingMessage(): string {
      const messageContent = ensureImagePlaceholders(content, attachments);
      const chipsBlock = terminalChips
        .map((chip) => `\n\n\`\`\`terminal ${chip.label}\n${chip.content}\n\`\`\``)
        .join('');
      return messageContent + chipsBlock;
    },
  };
}

export type ComposerDraftStore = ReturnType<typeof createComposerDraftStore>;
