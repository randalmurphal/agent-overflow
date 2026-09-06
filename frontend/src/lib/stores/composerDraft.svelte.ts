import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { threadHasScope } from '../transport/entityScopes';
import type { Attachment } from '../types/attachment';
import type { Draft, TerminalChip } from '../types/draft';
import type { SourceProposedPlan } from '../types/models';
import {
  ClearDraft,
  GetDraft,
  ListAttachments,
  SaveDraft,
} from './bindings';
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
import { addToast } from './toast.svelte';
import { errString } from '../utils/errors';
import { ensureImagePlaceholders } from '../utils/imagePlaceholders';

const DEFAULT_DEBOUNCE_MS = 500;

export interface DraftStoreOptions {
  debounceMs?: number;
  /**
   * Where mutations go. Fixed for the store's lifetime — a store never
   * changes modes, so no call sequence can leave persisted state behind
   * from a mode it has since left, and `persists` below is the one guard
   * every persistence-touching function opens with.
   *
   * - `'backend'` (default) — the thread's draft row plus the shared
   *   snapshot registry. This is the composer.
   * - `'none'` — purely local state. No RPC, no registry, no ClearDraft;
   *   `hasPendingSave` stays false and nothing hydrates. Seed it with
   *   `seedLocalSnapshot`. For surfaces that edit a COPY of a message and
   *   must not touch the draft the thread's real composer is holding —
   *   one SaveDraft or one registry write from such a store would
   *   overwrite work the user can see.
   */
  persistence?: 'backend' | 'none';
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
  const persists = options.persistence !== 'none';

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

  let debounceTimer: ReturnType<typeof setTimeout> | null = null;
  let pendingSaveGeneration = 0;
  let switchGeneration = 0;
  let hasPendingSave: boolean = $state(false);
  // Edge-trigger for the fire-and-forget save paths (debounce, flush,
  // switch-flush): the first failure after a working period toasts, the
  // retries a failing backend provokes every debounce tick do not. Reset
  // by any successful save. The awaited paths (restoreDraftFor) rethrow
  // instead — their callers own the message.
  let saveFailureSurfaced = false;
  let optimisticRestoredDraft: { threadId: string; snapshot: ComposerDraftSnapshot } | null = null;
  let optimisticRestoredDraftDirty = false;

  function clearDebounce() {
    if (debounceTimer) {
      clearTimeout(debounceTimer);
      debounceTimer = null;
    }
  }

  // ---- persistence ----
  //
  // Every RPC and every write to the shared snapshot registry passes
  // through one of the functions below, and each opens with the same
  // `persists` guard. That is the ONE mechanism that makes a local store
  // inert: there is no second place to consult, and a new call site is
  // inert by construction rather than by remembering to add a check.
  //
  // `draftRowsReachable()` is the second, narrower guard, and it answers
  // a different question: not "is this store allowed to persist" but
  // "can this SESSION reach a draft row at all". GetDraft, SaveDraft and
  // ClearDraft all carry `threads:operate`, so a view-only device that
  // opens a thread and types would spend one refused hydrate per open
  // and one refused save per debounce tick — and the save path toasts.
  // It gates only the three RPC-issuing functions; the shared snapshot
  // registry stays live, so a draft typed on such a device still follows
  // it between panes for as long as the tab is open. Read per call, not
  // captured: a pane can be constructed before the bootstrap manifest
  // resolves, and a captured `false` would leave the owner's own
  // composer silently not saving.
  function draftRowsReachable(id = threadId): boolean {
    return persists && threadHasScope('threads:operate', id);
  }

  function rememberSnapshot(id: string, snapshot: ComposerDraftSnapshot): void {
    if (!persists) return;
    rememberDraftSnapshot(id, snapshot);
  }

  function forgetSnapshot(id: string): void {
    if (!persists) return;
    forgetDraftSnapshot(id);
  }

  function peekSnapshot(id: string): ComposerDraftSnapshot | undefined {
    if (!persists) return undefined;
    return getRememberedDraftSnapshotForStore(id);
  }

  /**
   * Drop the durable row. Fire-and-forget by design — the send it follows
   * has already succeeded and nothing the user sees waits on the delete —
   * but a failure leaves a stale draft that will reappear on the next
   * thread open, so it is logged rather than swallowed.
   */
  function clearPersistedDraft(id: string): void {
    if (!draftRowsReachable(id)) return;
    void ClearDraft(id).catch((err) => {
      console.error(`Failed to clear the persisted draft for thread ${id}:`, err);
    });
  }

  function clearLocalSnapshotIfCurrent(id: string, savedSnapshot: ComposerDraftSnapshot): void {
    if (!persists) return;
    forgetDraftSnapshotIfMatches(id, savedSnapshot);
    if (threadId === id && draftSnapshotMatchesPersistedState(buildSnapshot(), savedSnapshot)) {
      hasPendingSave = false;
    }
  }

  /**
   * Read the persisted draft row and its attachment records as a snapshot.
   * The image-placeholder normalization is part of the read: a row whose
   * content lost a `[Image #n]` marker must come back with it, or the
   * attachment renders as an invisible extra on the next send. A file
   * attachment is untouched by it — the records carry their own kind, and a
   * file never had a marker to restore.
   *
   * Rejects with the underlying error — the callers decide what a failed
   * read means for them.
   */
  async function fetchPersistedSnapshot(id: string): Promise<ComposerDraftSnapshot> {
    const [draft, records] = await Promise.all([
      GetDraft(id) as Promise<Draft>,
      ListAttachments(id).then((rows) => (rows as Attachment[] | null) ?? []),
    ]);
    const attachmentIds = new Set(draft.attachmentIds ?? []);
    const attachments = records.filter((record) => attachmentIds.has(record.id));
    return {
      content: ensureImagePlaceholders(draft.content ?? '', attachments),
      attachments,
      terminalChips: draft.terminalChips ?? [],
      sourceProposedPlan: draft.sourceProposedPlan ?? null,
    };
  }

  async function saveSnapshot(id: string, snapshot: ComposerDraftSnapshot): Promise<void> {
    if (!draftRowsReachable(id)) return;
    try {
      const savePromise = SaveDraft(
        id,
        snapshot.content,
        snapshot.attachments.map((attachment) => attachment.id),
        snapshot.terminalChips,
        snapshot.sourceProposedPlan,
      );
      // Tracked with no await between the timer firing and here: quiesceSaves
      // cancels the timer and then joins this set, so a save is always in one.
      trackActiveDraftSave(id, savePromise);
      await savePromise;
      clearLocalSnapshotIfCurrent(id, snapshot);
      saveFailureSurfaced = false;
    } catch (err) {
      rememberSnapshot(id, snapshot);
      if (threadId === id && draftSnapshotMatchesPersistedState(buildSnapshot(), snapshot)) {
        hasPendingSave = true;
      }
      throw err;
    }
  }

  // The one surfacing point for the paths that swallow saveSnapshot's
  // rejection. The text itself is safe either way (remembered snapshot +
  // `hasPendingSave` retry machinery) — this is about the user KNOWING
  // their draft is not durably saved, instead of finding out on the next
  // restart.
  function surfaceSwallowedSaveFailure(failureLabel: string, err: unknown): void {
    if (saveFailureSurfaced) return;
    saveFailureSurfaced = true;
    addToast('error', `${failureLabel}: ${errString(err)}`);
  }

  async function hydrate(id: string, expectedGeneration: number): Promise<void> {
    if (!draftRowsReachable(id)) return;
    hydrating = true;
    const cached = peekSnapshot(id);
    if (cached) {
      applySnapshot(cached);
      hasPendingSave = true;
    }
    try {
      const loaded = await fetchPersistedSnapshot(id);
      if (threadId !== id || switchGeneration !== expectedGeneration) return; // thread switched while loading
      const currentCached = peekSnapshot(id);
      if (currentCached) {
        applySnapshot(currentCached);
        return;
      }
      applySnapshot(loaded);
    } catch (err) {
      if (isPassiveConnectionFailure(err)) return;
      if (threadId === id && switchGeneration === expectedGeneration) {
        // The composer renders empty over a row that still exists — say
        // so, or a draft the user KNOWS they left here reads as lost.
        addToast('error', `Failed to load draft: ${errString(err)}`);
      }
    } finally {
      if (threadId === id && switchGeneration === expectedGeneration) {
        hydrating = false;
      }
    }
  }

  function queueSave(): void {
    if (!threadId || !draftRowsReachable()) return;
    clearDebounce();
    const id = threadId;
    const generation = ++pendingSaveGeneration;
    const snapshot = buildSnapshot();
    hasPendingSave = true;
    debounceTimer = setTimeout(async () => {
      debounceTimer = null;
      if (threadId !== id || generation !== pendingSaveGeneration) return;
      try {
        await saveSnapshot(id, snapshot);
        if (threadId === id && generation === pendingSaveGeneration) {
          hasPendingSave = false;
        }
      } catch (err) {
        // The unsaved snapshot is retained; surfacing is edge-triggered
        // so a failing backend doesn't toast on every debounce tick.
        surfaceSwallowedSaveFailure('Failed to save draft', err);
      }
    }, debounceMs);
  }

  async function flush(): Promise<void> {
    if (!threadId || !draftRowsReachable()) return;
    clearDebounce();
    const id = threadId;
    const snapshot = buildSnapshot();
    try {
      await saveSnapshot(id, snapshot);
      hasPendingSave = false;
    } catch (err) {
      surfaceSwallowedSaveFailure('Failed to save draft', err);
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

  // Called on every mutation, so it checks `persists` itself rather than
  // building a snapshot per keystroke for `rememberSnapshot` to discard.
  function rememberCurrentDraft(): void {
    if (!persists || !threadId) return;
    rememberSnapshot(threadId, buildSnapshot());
  }

  async function setThread(id: string | null): Promise<void> {
    if (threadId === id) return;
    const previousId = threadId;
    const previousSnapshot = previousId && hasPendingSave ? buildSnapshot() : null;
    clearDebounce();
    pendingSaveGeneration++;
    const generation = ++switchGeneration;
    if (previousId && previousSnapshot) {
      rememberSnapshot(previousId, previousSnapshot);
      void saveSnapshot(previousId, previousSnapshot)
        .catch((err: unknown) => {
          surfaceSwallowedSaveFailure('Failed to save draft', err);
        });
    }
    threadId = id;
    content = '';
    attachments = [];
    terminalChips = [];
    sourceProposedPlan = null;
    clearOptimisticRestoredDraftMarker();
    hasPendingSave = false;
    hydrating = false;
    if (id && persists) {
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
    clearOptimisticRestoredDraftMarker();
    rememberSnapshot(id, buildSnapshot());
    if (
      persists
      && (content.trim() || attachments.length > 0 || terminalChips.length > 0 || sourceProposedPlan)
    ) {
      hasPendingSave = true;
      queueSave();
    }
  }

  async function reloadFromBackend(id: string | null = threadId): Promise<void> {
    if (!id || threadId !== id) return;
    // A local store has no backend row behind it: local state IS the draft,
    // and re-reading would blank it.
    if (!persists) return;
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
    forgetSnapshot(id);
    hasPendingSave = false;
    const generation = ++switchGeneration;
    await hydrate(id, generation);
  }

  async function prepareForExternalDraftReplace(id: string | null = threadId): Promise<void> {
    if (!id) return;
    // Nothing external can replace a local draft — there is no row to
    // replace and no in-flight write to wait for.
    if (!persists) return;
    if (threadId === id) {
      clearDebounce();
      pendingSaveGeneration++;
      clearOptimisticRestoredDraftMarker();
      hasPendingSave = false;
    }
    forgetSnapshot(id);
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
    get hasPendingSave() { return hasPendingSave; },
    get hasDraft() {
      return content.trim().length > 0 || attachments.length > 0 || terminalChips.length > 0;
    },
    /** False for a `persistence: 'none'` store. */
    get persists() { return persists; },

    // ---- thread lifecycle ----
    setThread,
    adoptThread,
    reloadFromBackend,
    prepareForExternalDraftReplace,
    flush,
    flushPending,

    /**
     * Leave no save in flight before a send consumes the row. The backend
     * runs a connection's RPCs concurrently, so a debounced SaveDraft still
     * on the wire can land AFTER the send's delete and resurrect the draft
     * the user just sent. The pending timer is cancelled (nothing new can
     * start before the caller's synchronous clear) and the saves already
     * issued are awaited; resolves immediately when there are none.
     */
    async quiesceSaves(): Promise<void> {
      const id = threadId;
      if (!id || !persists) return;
      clearDebounce();
      await waitForActiveDraftSaves(id);
    },

    /**
     * Read a thread's persisted draft as a snapshot, normalized exactly
     * as hydration normalizes it (they share one loader, so a caller that
     * merges against the row can never disagree with what a hydrate would
     * have shown). Pure read: it touches no store state, which is why it
     * takes the thread explicitly and works regardless of where this store
     * is pointed.
     *
     * Rejects on failure. A caller merging edited text into the row has to
     * know it never read the row — an empty snapshot would look like an
     * empty draft and silently take the "nothing to merge" branch.
     */
    loadPersistedSnapshot(id: string): Promise<ComposerDraftSnapshot> {
      return fetchPersistedSnapshot(id);
    },

    /**
     * Point a local store at a thread and fill it, without hydrating.
     * The seeded content is the caller's — a message being edited, say —
     * not the thread's persisted draft, so there is nothing to load and
     * nothing to save.
     *
     * Refuses on a persisting store: seeding one would show state the
     * backend row does not have and the next mutation would overwrite the
     * user's real draft with it.
     */
    seedLocalSnapshot(id: string | null, snapshot: ComposerDraftSnapshot): void {
      if (persists) {
        throw new Error('seedLocalSnapshot requires a store created with persistence: "none"');
      }
      clearDebounce();
      pendingSaveGeneration++;
      switchGeneration++;
      threadId = id;
      clearOptimisticRestoredDraftMarker();
      applySnapshot(snapshot);
      hasPendingSave = false;
      hydrating = false;
      },

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
      },

    /**
     * Undo the local-only optimistic restore when the backend declines
     * or fails the revert. If the user has edited meanwhile, preserve
     * that newer composer state.
     *
     * Returns true when the untouched restore was cleared back to the
     * empty baseline — the caller may then safely repaint whatever the
     * composer held before the optimistic apply. Returns false when the
     * user's newer edits were preserved (or the store has moved to another
     * thread) and repainting would clobber them.
     *
     * The Stop/Esc un-send path (`revertOnInterrupt`) is the one caller.
     * Edit-and-resend also paints optimistically — once, through
     * `applyOptimisticRestoredDraft`, when a committed revert's resend
     * fails — but it has nothing to undo: that paint IS the recovery, so
     * it never reaches for this.
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
      forgetSnapshot(id);
      hasPendingSave = false;
      return true;
    },

    /**
     * Paint a history-recall preview into the composer WITHOUT touching
     * persistence: no debounce queue, no snapshot registry, no SQLite.
     * Browsing past messages must never overwrite the durable draft row —
     * a restart restores what the user TYPED, not what they were looking
     * at. Self-guarding: while a save is pending (`hasPendingSave`), the
     * paint is refused outright — the pending snapshot holds real typed
     * state, and painting over it would let the switch-flush persist the
     * preview. The recall controller flushes before its first paint, so
     * the refusal only fires on a failed-save race; it leaves content
     * unchanged, which the controller's attestation reads as session
     * over. Previews never queue a save, so after the guard passes there
     * is no timer to cancel and nothing here touches the debounce. The
     * next real edit goes through `setContent` as usual, which is what
     * makes an edited recall take over as the draft.
     */
    applyHistoryPreview(text: string): void {
      if (hasPendingSave) return;
      markOptimisticRestoredDraftDirty();
      content = text;
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
        forgetSnapshot(id);
        clearPersistedDraft(id);
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
        forgetSnapshot(id);
      }
    },

    /**
     * Restore a draft snapshot to a specific thread. Used when a send
     * rejects AFTER the user has switched panes: we don't want to silently
     * dump thread A's failed message into thread B's composer. If the draft
     * store is still on the given thread the snapshot is painted into the
     * local UI state FIRST, then persisted — the paint is what puts the
     * user's text back where they can act on it, and it must not be
     * hostage to the write succeeding (the write failing is precisely
     * when the text has nowhere else to live). Otherwise only the backend
     * row is repopulated so the draft is there next time they return.
     *
     * REJECTS when the write fails. The caller asked for the user's text
     * to be put somewhere it SURVIVES; painted-but-unsaved is not that,
     * so it has to be able to say so. The store is left in the ordinary
     * unsaved-draft state (`hasPendingSave` + the remembered snapshot),
     * which the debounce and switch-flush machinery retries like any
     * other unsaved keystrokes.
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
      const restoredSnapshot: ComposerDraftSnapshot = {
        content: snapshot.content,
        attachments: snapshot.attachments,
        terminalChips: snapshot.terminalChips,
        sourceProposedPlan: snapshot.sourceProposedPlan ?? null,
      };
      if (threadId === id) {
        clearDebounce();
        pendingSaveGeneration++;
        applySnapshot(restoredSnapshot);
      }
      try {
        await saveSnapshot(id, restoredSnapshot);
      } catch (err) {
        // The marker claims "painted locally, and the backend already has
        // it" — which this failure disproves. Only THIS thread's marker:
        // the store may be sitting on another thread, whose paint this
        // failure says nothing about. (Same-thread: saveSnapshot has
        // already re-remembered the snapshot and raised `hasPendingSave`,
        // so the painted text survives and retries.)
        if (optimisticRestoredDraft?.threadId === id) {
          clearOptimisticRestoredDraftMarker();
        }
        throw err;
      }
      if (threadId === id) {
        clearOptimisticRestoredDraftMarker();
        hasPendingSave = false;
      }
    },

    /**
     * Build the outgoing text payload for Send. Terminal chip contents are
     * inlined as fenced blocks. ATTACHMENTS of both kinds travel separately,
     * as structured attachment ids: an image is bound to its `[Image #N]`
     * marker here so the provider receives a real image input, and a file
     * contributes no text at all — the backend appends its path line to the
     * provider payload, never to what is persisted or rendered.
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
