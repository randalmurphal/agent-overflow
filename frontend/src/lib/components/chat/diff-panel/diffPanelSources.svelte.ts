// Diff-panel source data loaders.
//
// Extracted from DiffPanelDrawer.svelte to keep the shell focused on
// composition. Owns the three async fetch paths the panel drives:
//
//   - Turn diff (fetch+cache by (threadId, turnIndex, compareMode))
//   - Working-tree diff
//   - Cumulative (agent-authored) diff
//
// All three paths share the same pattern: track an in-flight `loading`
// flag, surface the resulting text as a string, and push errors up to
// the panel's store.setError. Extracting keeps the .svelte file under
// the 300-line guideline.

import {
  GetCheckpointToWorktreeDiff,
  GetPayloadData,
  GetTurnDiff,
  GetWorkingTreeDiff,
  ListThreadCheckpoints,
} from '../../../stores/bindings';
import {
  aggregateAgentDiffs,
  type AgentDiffEntry,
} from '../../../utils/diffAggregation';
import type { Checkpoint } from '../../../types/checkpoint';
import type { DiffPanelState, TurnCompareMode } from '../../../stores/diffPanel.svelte';

function errString(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}

export interface DiffPanelSourcesOptions {
  /** Returns the currently-open thread id. `null` short-circuits every load. */
  getThreadId: () => string | null;
  /** Shared diff panel store — reads selection, writes errors + caches. */
  store: DiffPanelState;
  /** Cumulative view reads agent-authored diff entries from the pane's items. */
  getCumulativeEntries: () => AgentDiffEntry[];
}

export interface DiffPanelSourcesHandle {
  readonly loadingTurn: boolean;
  readonly turnDiffText: string;

  readonly worktreeLoading: boolean;
  readonly worktreeText: string;

  readonly cumulativeLoading: boolean;
  readonly cumulativeText: string;

  refreshCheckpoints(): Promise<void>;
  loadSelectedTurnDiff(): Promise<void>;
  loadWorktreeDiff(force?: boolean): Promise<void>;
  loadCumulativeDiff(force?: boolean): Promise<void>;
}

export function createDiffPanelSources(
  opts: DiffPanelSourcesOptions,
): DiffPanelSourcesHandle {
  let loadingTurn = $state(false);
  let turnDiffText = $state('');
  let worktreeLoading = $state(false);
  let worktreeText = $state('');
  let cumulativeLoading = $state(false);
  let cumulativeText = $state('');

  async function refreshCheckpoints(): Promise<void> {
    const threadId = opts.getThreadId();
    if (!threadId) return;
    try {
      const raw = (await ListThreadCheckpoints(threadId)) as Checkpoint[] | null;
      opts.store.setCheckpoints(raw ?? []);
    } catch (err) {
      opts.store.setError(`Failed to load checkpoints: ${errString(err)}`);
    }
  }

  async function loadSelectedTurnDiff(): Promise<void> {
    const threadId = opts.getThreadId();
    if (!threadId) return;
    const turnIndex = opts.store.selectedTurnIndex;
    const mode: TurnCompareMode = opts.store.turnCompareMode;
    if (turnIndex === null) {
      turnDiffText = '';
      return;
    }
    const cached = opts.store.readTurnDiff(threadId, turnIndex, mode);
    if (cached !== undefined) {
      turnDiffText = cached;
      return;
    }
    loadingTurn = true;
    try {
      const text =
        mode === 'next'
          ? await GetTurnDiff(threadId, turnIndex)
          : await GetCheckpointToWorktreeDiff(threadId, turnIndex);
      const result = (text ?? '') as string;
      opts.store.writeTurnDiff(threadId, turnIndex, mode, result);
      turnDiffText = result;
    } catch (err) {
      opts.store.setError(`Failed to load turn diff: ${errString(err)}`);
      turnDiffText = '';
    } finally {
      loadingTurn = false;
    }
  }

  async function loadWorktreeDiff(force = false): Promise<void> {
    const threadId = opts.getThreadId();
    if (!threadId) return;
    if (!force && worktreeText.length > 0) return;
    worktreeLoading = true;
    try {
      const text = await GetWorkingTreeDiff(threadId);
      worktreeText = (text ?? '') as string;
    } catch (err) {
      opts.store.setError(`Failed to load working tree diff: ${errString(err)}`);
      worktreeText = '';
    } finally {
      worktreeLoading = false;
    }
  }

  async function loadCumulativeDiff(force = false): Promise<void> {
    const threadId = opts.getThreadId();
    if (!threadId) return;
    if (force) opts.store.invalidateCumulative();
    cumulativeLoading = true;
    try {
      const text = await aggregateAgentDiffs(
        opts.getCumulativeEntries(),
        async (id) => (await GetPayloadData(id)).data,
        opts.store.cumulativeCache,
      );
      cumulativeText = text;
    } catch (err) {
      opts.store.setError(`Failed to aggregate agent diffs: ${errString(err)}`);
      cumulativeText = '';
    } finally {
      cumulativeLoading = false;
    }
  }

  return {
    get loadingTurn() { return loadingTurn; },
    get turnDiffText() { return turnDiffText; },
    get worktreeLoading() { return worktreeLoading; },
    get worktreeText() { return worktreeText; },
    get cumulativeLoading() { return cumulativeLoading; },
    get cumulativeText() { return cumulativeText; },

    refreshCheckpoints,
    loadSelectedTurnDiff,
    loadWorktreeDiff,
    loadCumulativeDiff,
  };
}
