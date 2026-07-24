import type { Attachment } from '../types/attachment';
import type { Draft, TerminalChip } from '../types/draft';
import type { SourceProposedPlan } from '../types/models';
import {
  cloneDraftSnapshot,
  draftSnapshotMatchesPersistedState,
  forgetDraftSnapshot,
  forgetDraftSnapshotIfMatches,
  getRememberedDraftSnapshotForStore,
  rememberDraftSnapshot,
  resetComposerDraftSnapshotStateForTest,
  trackActiveDraftSave,
  waitForActiveDraftSaves,
  type ComposerDraftSnapshot,
} from './composerDraftSnapshots';
import {
  ClearDraft,
  GetDraft,
  ListAttachments,
  SaveDraft,
} from './bindings';
import { errString } from '../utils/errors';
import { ensureImagePlaceholders } from '../utils/imagePlaceholders';

const DEFAULT_DEBOUNCE_MS = 500;

interface DraftStoreOptions {
  debounceMs?: number;
}

export function resetComposerDraftSnapshotsForTest(): void {
  resetComposerDraftSnapshotStateForTest();
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
  let hasPendingSave: boolean = $state(false);
  let optimisticRestoredDraft: { threadId: string; snapshot: ComposerDraftSnapshot } | null = null;
  let optimisticRestoredDraftDirty = false;

  function clearDebounce() {
    if (debounceTimer) {
      clearTimeout(debounceTimer);
      debounceTimer = null;
    }
  }

  function clearLocalSnapshotIfCurrent(id: string, savedSnapshot: ComposerDraftSnapshot): void {
    forgetDraftSnapshotIfMatches(id, savedSnapshot);
    if (threadId === id && draftSnapshotMatchesPersistedState(buildSnapshot(), savedSnapshot)) {
      hasPendingSave = false;
    }
  }

  async function saveSnapshot(id: string, snapshot: ComposerDraftSnapshot, failureLabel: string): Promise<void> {
    try {
      const savePromise = SaveDraft(
        id,
        snapshot.content,
        snapshot.attachments.map((a) => a.id),
        snapshot.terminalChips,
        snapshot.sourceProposedPlan,
      );
      trackActiveDraftSave(id, savePromise);
      await savePromise;
      clearLocalSnapshotIfCurrent(id, snapshot);
    } catch (err) {
      rememberDraftSnapshot(id, snapshot);
      if (threadId === id && draftSnapshotMatchesPersistedState(buildSnapshot(), snapshot)) {
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
    const cached = getRememberedDraftSnapshotForStore(id);
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
      const currentCached = getRememberedDraftSnapshotForStore(id);
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
    const cloned = cloneDraftSnapshot(snapshot);
    content = cloned.content;
    attachments = cloned.attachments;
    terminalChips = cloned.terminalChips;
    sourceProposedPlan = cloned.sourceProposedPlan;
  }

  function emptySnapshot(): ComposerDraftSnapshot {
    return {
      content: '',
      attachments: [],
      terminalChips: [],
      sourceProposedPlan: null,
    };
  }

  function markOptimisticRestoredDraftDirty(): void {
    if (optimisticRestoredDraft?.threadId === threadId) {
      optimisticRestoredDraftDirty = true;
    }
  }

  function clearOptimisticRestoredDraftMarker(): void {
    optimisticRestoredDraft = null;
    optimisticRestoredDraftDirty = false;
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
    clearOptimisticRestoredDraftMarker();
    hasPendingSave = false;
    error = null;
    hydrating = false;
    if (id) {
      await hydrate(id, generation);
      if (generation !== switchGeneration || threadId !== id) return;
    }
  }

  function adoptThread(id: string): void {
    if (threadId === id) return;
    clearDebounce();
    pendingSaveGeneration++;
    switchGeneration++;
    threadId = id;
    hydrating = false;
    error = null;
    clearOptimisticRestoredDraftMarker();
    rememberDraftSnapshot(id, buildSnapshot());
    if (content.trim() || attachments.length > 0 || terminalChips.length > 0 || sourceProposedPlan) {
      hasPendingSave = true;
      queueSave();
    }
  }

  async function reloadFromBackend(id: string | null = threadId): Promise<void> {
    if (!id || threadId !== id) return;
    if (
      optimisticRestoredDraft?.threadId === id
      && (
        optimisticRestoredDraftDirty
        || !draftSnapshotMatchesPersistedState(buildSnapshot(), optimisticRestoredDraft.snapshot)
      )
    ) {
      clearOptimisticRestoredDraftMarker();
      return;
    }
    clearDebounce();
    pendingSaveGeneration++;
    if (optimisticRestoredDraft?.threadId === id) {
      clearOptimisticRestoredDraftMarker();
    }
    forgetDraftSnapshot(id);
    hasPendingSave = false;
    const generation = ++switchGeneration;
    await hydrate(id, generation);
  }

  async function prepareForExternalDraftReplace(id: string | null = threadId): Promise<void> {
    if (!id) return;
    if (threadId === id) {
      clearDebounce();
      pendingSaveGeneration++;
      clearOptimisticRestoredDraftMarker();
      hasPendingSave = false;
    }
    forgetDraftSnapshot(id);
    await waitForActiveDraftSaves(id);
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
    adoptThread,
    reloadFromBackend,
    prepareForExternalDraftReplace,
    flush,
    flushPending,

    /**
     * Paints a reverted prompt back into the live composer without
     * touching SQLite. The backend writes the durable draft as part of
     * the locked revert; this only removes the visual gap between the
     * optimistic timeline truncate and the later confirmation event.
     */
    applyOptimisticRestoredDraft(id: string, snapshot: ComposerDraftSnapshot): void {
      if (threadId !== id) return;
      clearDebounce();
      pendingSaveGeneration++;
      applySnapshot(snapshot);
      optimisticRestoredDraft = {
        threadId: id,
        snapshot: cloneDraftSnapshot(snapshot),
      };
      optimisticRestoredDraftDirty = false;
      hasPendingSave = false;
      error = null;
    },

    /**
     * Undo the local-only optimistic restore when the backend declines
     * or fails the revert. If the user has edited meanwhile, preserve
     * that newer composer state.
     *
     * Returns true when the untouched restore was cleared back to the
     * empty baseline — the caller may then safely repaint whatever the
     * composer held before the optimistic apply (the explicit
     * revert-to-message path restores the user's pre-revert draft).
     * Returns false when the user's newer edits were preserved (or the
     * store has moved to another thread) and repainting would clobber
     * them.
     */
    clearOptimisticRestoredDraft(id: string, snapshot: ComposerDraftSnapshot): boolean {
      if (threadId !== id) return false;
      if (
        optimisticRestoredDraftDirty
        || !draftSnapshotMatchesPersistedState(buildSnapshot(), snapshot)
      ) {
        clearOptimisticRestoredDraftMarker();
        return false;
      }
      clearOptimisticRestoredDraftMarker();
      clearDebounce();
      pendingSaveGeneration++;
      applySnapshot(emptySnapshot());
      forgetDraftSnapshot(id);
      hasPendingSave = false;
      return true;
    },

    // ---- content mutations ----
    setContent(next: string): void {
      markOptimisticRestoredDraftDirty();
      content = next;
      rememberCurrentDraft();
      queueSave();
    },

    setContentAndAttachments(nextContent: string, nextAttachments: Attachment[]): void {
      markOptimisticRestoredDraftDirty();
      content = nextContent;
      attachments = [...nextAttachments];
      rememberCurrentDraft();
      queueSave();
    },

    removeAttachment(id: string): void {
      const next = attachments.filter((a) => a.id !== id);
      if (next.length === attachments.length) return;
      markOptimisticRestoredDraftDirty();
      attachments = next;
      rememberCurrentDraft();
      queueSave();
    },

    addAttachment(attachment: Attachment): void {
      if (attachments.some((a) => a.id === attachment.id)) return;
      markOptimisticRestoredDraftDirty();
      attachments = [...attachments, attachment];
      rememberCurrentDraft();
      queueSave();
    },

    addTerminalChip(chip: TerminalChip): void {
      if (terminalChips.some((c) => c.id === chip.id)) return;
      markOptimisticRestoredDraftDirty();
      terminalChips = [...terminalChips, chip];
      rememberCurrentDraft();
      queueSave();
    },

    removeTerminalChip(id: string): void {
      const next = terminalChips.filter((c) => c.id !== id);
      if (next.length === terminalChips.length) return;
      markOptimisticRestoredDraftDirty();
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
    clearAfterSend(): void {
      const id = threadId;
      markOptimisticRestoredDraftDirty();
      clearDebounce();
      pendingSaveGeneration++;
      content = '';
      attachments = [];
      terminalChips = [];
      sourceProposedPlan = null;
      hasPendingSave = false;
      if (id) {
        forgetDraftSnapshot(id);
        void ClearDraft(id).catch(() => {});
      }
    },

    /**
     * Clear only the visible composer after the backend has accepted a queued
     * send. RegisterQueueItem owns the durable draft clear; issuing a separate
     * ClearDraft here can race a session-death restore and delete the restored
     * draft after it was written.
     */
    clearLocalAfterQueue(): void {
      const id = threadId;
      markOptimisticRestoredDraftDirty();
      clearDebounce();
      pendingSaveGeneration++;
      content = '';
      attachments = [];
      terminalChips = [];
      sourceProposedPlan = null;
      hasPendingSave = false;
      if (id) {
        forgetDraftSnapshot(id);
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
        clearOptimisticRestoredDraftMarker();
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
