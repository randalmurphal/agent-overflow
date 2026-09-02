// stores/threadDraftPlaceholder.svelte.ts
//
// OWNS the pane's pre-materialization phase: the synthetic
// `DraftThreadPlaceholder`, the edits a user can make to it before any row
// exists (mode, provider/model defaults, workspace), the coalesced
// `CreateThread` that materializes it, the re-keying every id-keyed store
// needs at that moment (worktree intent, terminals, composer draft), and
// the reverse trip back to a placeholder when a materialized draft is
// emptied.
//
// MUST NOT own the mounted thread. It writes `thread` only while the pane
// is in the placeholder phase — seeding the synthetic row, adopting the
// created one — and every real thread switch stays in
// `threadSwitchLoad.svelte.ts`. It owns no timeline rows: the emptiness
// test arrives as a count, and `clearPane` / `snapshotForClose` are the
// pane's own, passed in so "+ New" over a live thread takes exactly the
// leaving-a-thread path a close does.

import type { Project, Thread } from '../types/models';
import type { ContextWindow } from '../types/events';
import { asProviderID } from '../types/providers';
import { CreateThread } from './bindings';
import { prependThread, removeThread } from './threads.svelte';
import {
  clearWorktreeIntent,
  migrateWorktreeIntent,
  seedDefaultWorktreeIntentForDraft,
} from './worktreeIntent.svelte';
import { getComposerDraftForPane } from './composerDraftRegistry.svelte';
import { adoptDiffSpanOwner } from '../utils/diffSpanCache.svelte';
import { errString } from '../utils/errors';
import { sameNormalizedPath } from '../utils/path';
import { seedContextWindow } from './threadContextWindow';
import type { ThreadSwitchLoad } from './threadSwitchLoad.svelte';
import type {
  DraftPlaceholderDefaults,
  DraftPlaceholderMode,
  DraftThreadPlaceholder,
} from './threadPaneShared';

export interface ThreadDraftPlaceholderOptions {
  paneId: string;
  getThread(): Thread | null;
  setThread(next: Thread | null): void;
  /** Rows in the loaded window — the "is this draft still empty?" test. */
  getItemCount(): number;
  getShowTerminal(): boolean;
  /** `switchGeneration++`: every placeholder edit is a same-thread re-switch. */
  bumpSwitchGeneration(): void;
  setContextWindow(next: ContextWindow | null): void;
  setGeneralError(message: string): void;
  /** Pane-close snapshot, so "+ New" over a live thread is a warm-restore edge. */
  snapshotForClose(): void;
  /** The pane's own `clear()`. */
  clearPane(): void;
  /** Constructed after this module; read only inside method bodies. */
  switchLoad(): ThreadSwitchLoad;
}

export function createThreadDraftPlaceholder(
  options: ThreadDraftPlaceholderOptions,
) {
  const { paneId } = options;
  let draftPlaceholder: DraftThreadPlaceholder | null = $state(null);
  // materializingThreadPromise coalesces concurrent ensureMaterializedThread
  // callers — composer input, paste/upload, send, toolbar pickers — into a
  // single CreateThread call. Cleared in `finally` so a subsequent
  // placeholder can materialize on its own.
  let materializingThreadPromise: Promise<string | null> | null = null;
  const invalidatedDraftTerminalIds = new Set<string>();

  function setDraftPlaceholderMode(mode: DraftPlaceholderMode): boolean {
    const thread = options.getThread();
    if (!draftPlaceholder || !thread) return false;
    const now = Date.now();
    draftPlaceholder = { ...draftPlaceholder, mode };
    options.setThread({
      ...thread,
      mode,
      updatedAt: now,
    });
    options.bumpSwitchGeneration();
    return true;
  }

  function applyDraftPlaceholderDefaults(
    defaults: DraftPlaceholderDefaults,
  ): boolean {
    const thread = options.getThread();
    if (!draftPlaceholder || !thread) return false;
    const provider = asProviderID(defaults.provider) ?? thread.provider;
    const next: Thread = {
      ...thread,
      provider,
      model: defaults.model ?? thread.model,
      reasoningEffort: (defaults.reasoningEffort ??
        thread.reasoningEffort) as Thread['reasoningEffort'],
      fastMode: defaults.fastMode ?? thread.fastMode,
      contextWindow: defaults.contextWindow ?? thread.contextWindow,
      runtimeMode: (defaults.runtimeMode ??
        thread.runtimeMode) as Thread['runtimeMode'],
      updatedAt: Date.now(),
    };
    options.setThread(next);
    options.setContextWindow(seedContextWindow(next));
    options.bumpSwitchGeneration();
    return true;
  }

  function applyDraftPlaceholderWorkspace(workspace: {
    workspacePath: string;
    worktreePath?: string;
    branch?: string;
  }): boolean {
    const thread = options.getThread();
    if (!draftPlaceholder || !thread) return false;
    const workspacePath = workspace.workspacePath.trim();
    if (!workspacePath) return false;
    if (!sameNormalizedPath(workspacePath, thread.workspacePath)) {
      options.switchLoad().closeDraftPlaceholderTerminals(draftPlaceholder.id);
    }
    options.setThread({
      ...thread,
      workspacePath,
      worktreePath: workspace.worktreePath ?? '',
      branch: workspace.branch ?? thread.branch,
      updatedAt: Date.now(),
    });
    options.bumpSwitchGeneration();
    return true;
  }

  function dematerializeEmptyDraftThread(): boolean {
    const current = options.getThread();
    if (draftPlaceholder || !current || options.getItemCount() > 0) return false;
    if (current.mode !== 'chat' && current.mode !== 'plan') return false;
    if (!current.projectId || !current.projectPath) return false;
    const now = Date.now();
    const mode = current.mode as DraftPlaceholderMode;
    const placeholder: DraftThreadPlaceholder = {
      id: `draft:${paneId}:${current.projectId}:${mode}:${now}`,
      projectId: current.projectId,
      projectName: '',
      projectPath: current.projectPath,
      mode,
      createdAt: now,
    };
    migrateWorktreeIntent(current.id, placeholder.id);
    draftPlaceholder = placeholder;
    options.setThread({
      ...current,
      id: placeholder.id,
      title: 'New Thread',
      createdAt: now,
      updatedAt: now,
      isDraft: true,
    });
    removeThread(current.id);
    options.bumpSwitchGeneration();
    return true;
  }

  function startDraftPlaceholder(
    project: Project,
    mode: DraftPlaceholderMode = 'chat',
    defaults?: DraftPlaceholderDefaults,
  ): void {
    // clearPane() drops any intent staged against the prior placeholder id,
    // so "+ New" on top of an existing placeholder doesn't leak entries.
    // "+ New" on a pane showing a thread is a leaving-the-thread edge
    // like close: cache the window first so returning to that thread
    // is a warm restore.
    options.snapshotForClose();
    options.clearPane();
    const now = Date.now();
    const placeholder: DraftThreadPlaceholder = {
      id: `draft:${paneId}:${project.id}:${mode}:${now}`,
      projectId: project.id,
      projectName: project.name,
      projectPath: project.path,
      mode,
      createdAt: now,
    };
    draftPlaceholder = placeholder;
    // Seed defaults mirror what CreateThread would have used. When the
    // caller couldn't fetch them (offline, race, etc.) we still render
    // a usable placeholder — the toolbar pickers fall back to their
    // own resolution paths.
    const seededProvider = asProviderID(defaults?.provider);
    options.setThread({
      id: placeholder.id,
      title: 'New Thread',
      provider: seededProvider ?? 'codex',
      workspacePath: defaults?.workspacePath || project.path,
      projectPath: project.path,
      projectId: project.id,
      mode,
      model: defaults?.model ?? '',
      reasoningEffort: defaults?.reasoningEffort as Thread['reasoningEffort'],
      fastMode: defaults?.fastMode,
      contextWindow: defaults?.contextWindow,
      runtimeMode: defaults?.runtimeMode as Thread['runtimeMode'],
      branch: defaults?.branch,
      createdAt: now,
      updatedAt: now,
      archived: false,
      // Match the backend projection: a synthetic placeholder has no
      // items, so isDraft is the truth even before the row exists.
      // Any consumer reading pane.thread?.isDraft gets the right
      // answer in both placeholder and materialized phases.
      isDraft: true,
    });
    options.bumpSwitchGeneration();
  }

  async function materializeDraftPlaceholder(): Promise<Thread | null> {
    const placeholder = draftPlaceholder;
    if (!placeholder) return options.getThread();
    const current = options.getThread();
    const created = (await CreateThread({
      projectId: placeholder.projectId,
      provider: current?.provider,
      model: current?.model,
      mode: current?.mode ?? placeholder.mode,
      reasoningEffort: current?.reasoningEffort,
      fastMode: current?.fastMode,
      contextWindow: current?.contextWindow,
      runtimeMode: current?.runtimeMode,
      worktreePath: current?.worktreePath,
      workspaceOverride: current?.workspacePath,
      branch: current?.branch,
    })) as Thread;
    return created;
  }

  function adoptMaterializedDraftThread(materializedThread: Thread): void {
    if (!draftPlaceholder) return;
    draftPlaceholder = null;
    options.setThread(materializedThread);
    options.setContextWindow(seedContextWindow(materializedThread));
    options.bumpSwitchGeneration();
  }

  /**
   * Materialize a draft placeholder into a real thread row, or return the
   * existing thread id when one is already present. Coalesces concurrent
   * callers so composer-input, paste/upload, and send don't each race
   * to `CreateThread`. Resolves to null when the pane
   * has neither a thread nor a placeholder, or when the placeholder was
   * replaced (e.g. another "+ New" click) before the create resolved —
   * the stale-create guard checks the placeholder id at completion.
   *
   * Side effects on success: seeds the default worktree intent for the
   * new thread, prepends it to the sidebar threads registry, adopts it
   * on the pane, and points the pane's registered composer-draft store
   * at the new thread id (so typed text saved against the placeholder
   * id flushes through to the real thread row).
   */
  async function ensureMaterializedThread(): Promise<string | null> {
    const existingId = draftPlaceholder
      ? null
      : (options.getThread()?.id ?? null);
    if (existingId) return existingId;
    const placeholder = draftPlaceholder;
    if (!placeholder) return null;
    if (materializingThreadPromise) return materializingThreadPromise;
    const placeholderId = placeholder.id;
    materializingThreadPromise = (async () => {
      try {
        const created = await materializeDraftPlaceholder();
        if (!created) return null;
        if (draftPlaceholder?.id !== placeholderId) return null;
        await options.switchLoad().migrateDraftPlaceholderTerminals(
          placeholderId,
          created.id,
        );
        // Re-key any intent staged against the placeholder id BEFORE we
        // adopt the real thread. Worktree/branch picks made on the
        // placeholder otherwise become orphaned when lookups switch to
        // the materialized thread id.
        migrateWorktreeIntent(placeholderId, created.id);
        // Same re-keying for the diff-span cache's OWNERSHIP records: a
        // review pane open on the placeholder filed its entries under the
        // synthetic id, and switch/close/delete only ever evict the real
        // one. The cached spans themselves are keyed by their workspace
        // subject, which this transition does not change, so the
        // remounted review companion re-reads them without a refetch.
        adoptDiffSpanOwner(placeholderId, created.id);
        seedDefaultWorktreeIntentForDraft(created);
        prependThread(created);
        adoptMaterializedDraftThread(created);
        const draftStore = getComposerDraftForPane(paneId);
        if (draftStore) draftStore.adoptThread(created.id);
        return created.id;
      } catch (err) {
        console.error('Failed to create draft thread:', err);
        options.setGeneralError(`Failed to create thread: ${errString(err)}`);
        return null;
      } finally {
        materializingThreadPromise = null;
      }
    })();
    return materializingThreadPromise;
  }

  function canAdoptOpenedTerminal(
    threadID: string,
    workspacePath: string | undefined,
  ): boolean {
    if (!threadID) return false;
    if (invalidatedDraftTerminalIds.has(threadID)) return false;
    const thread = options.getThread();
    if (draftPlaceholder?.id === threadID) {
      if (!options.getShowTerminal() || !thread) return false;
      if (
        workspacePath !== undefined &&
        !sameNormalizedPath(workspacePath, thread.workspacePath)
      ) {
        return false;
      }
      return true;
    }
    return thread?.id === threadID;
  }

  return {
    get placeholder(): DraftThreadPlaceholder | null {
      return draftPlaceholder;
    },
    /**
     * Ids of terminals opened against a placeholder whose cwd then changed:
     * a late open must not be adopted. Shared with the switch/load pipeline,
     * which is the other writer.
     */
    invalidatedDraftTerminalIds,
    /**
     * The pane's `clear()` half that belongs to the placeholder: any intent
     * staged against the (about-to-be-discarded) placeholder id must die
     * with it — otherwise repeated "+ New" clicks, thread switches, or pane
     * closes leak entries keyed by ids the rest of the app no longer reads.
     * Cleanup is keyed on the placeholder id because real threads keep their
     * entries until the thread itself is removed by the backend.
     */
    closePlaceholderIntents(): void {
      if (!draftPlaceholder) return;
      options.switchLoad().closeDraftPlaceholderTerminals(draftPlaceholder.id);
      clearWorktreeIntent(draftPlaceholder.id);
    },
    /** Drop the placeholder without touching intents (the pane's `clear()`). */
    reset(): void {
      draftPlaceholder = null;
    },
    setDraftPlaceholderMode,
    applyDraftPlaceholderDefaults,
    applyDraftPlaceholderWorkspace,
    dematerializeEmptyDraftThread,
    startDraftPlaceholder,
    materializeDraftPlaceholder,
    adoptMaterializedDraftThread,
    ensureMaterializedThread,
    canAdoptOpenedTerminal,
  };
}

export type ThreadDraftPlaceholder = ReturnType<
  typeof createThreadDraftPlaceholder
>;
