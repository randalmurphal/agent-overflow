<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { wailsEventOn } from '../../stores/events';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type {
    Checkpoint,
    CheckpointCapturedEvent,
    CheckpointErrorEvent,
    CheckpointUnavailableEvent,
  } from '../../types/checkpoint';
  import type { DiffPanelSource, TurnCompareMode } from '../../stores/diffPanel.svelte';
  import {
    GetCheckpointToWorktreeDiff,
    GetTurnDiff,
    GetWorkingTreeDiff,
    GetPayloadData,
    ListThreadCheckpoints,
    RevertToTurn,
  } from '../../stores/bindings';
  import type { RevertMode } from '../../types/checkpoint';
  import { addToast } from '../../stores/toast.svelte';
  import RevertDialog from './diff-panel/RevertDialog.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import {
    aggregateAgentDiffs,
    selectAgentDiffEntries,
    summarizeEntries,
    type AgentDiffEntry,
    type DiffStats,
  } from '../../utils/diffAggregation';
  import CumulativeView from './diff-panel/CumulativeView.svelte';
  import ErrorBanner from './diff-panel/ErrorBanner.svelte';
  import PanelHeader from './diff-panel/PanelHeader.svelte';
  import TurnDiffView from './diff-panel/TurnDiffView.svelte';
  import WorkingTreeView from './diff-panel/WorkingTreeView.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  const store = $derived(pane.diffPanel);
  const threadId = $derived(pane.thread?.id ?? null);

  let loadingDiff = $state(false);
  let turnDiffText = $state('');
  let worktreeLoading = $state(false);
  let worktreeText = $state('');
  let cumulativeLoading = $state(false);
  let cumulativeText = $state('');

  // Insertion/deletion totals for the cumulative tab header.
  let cumulativeEntries = $derived<AgentDiffEntry[]>(selectAgentDiffEntries(pane.items));
  let cumulativeStats = $derived<DiffStats>(summarizeEntries(cumulativeEntries, pane.payloadMetas));

  // The panel body's visible tab. Derived so the SourceTabs component stays
  // synchronized with store.source.
  let activeSource = $derived<DiffPanelSource>(store.source);

  // --- Turn list lifecycle ---

  async function refreshCheckpoints(): Promise<void> {
    if (!threadId) return;
    try {
      const raw = (await ListThreadCheckpoints(threadId)) as Checkpoint[] | null;
      store.setCheckpoints(raw ?? []);
    } catch (err) {
      store.setError(`Failed to load checkpoints: ${errString(err)}`);
    }
  }

  function errString(err: unknown): string {
    if (err instanceof Error) return err.message;
    return String(err);
  }

  // A thread "has had turns" when at least one persisted item carries a
  // non-negative turnIndex. The turn-diff tab is visible when either the
  // workspace is a git repo with checkpoints, or we have no strong signal that
  // checkpoints are unavailable.
  let threadHasTurns = $derived(pane.items.some((it) => it.turnIndex >= 0));
  let turnTabVisible = $derived.by(() => {
    if (store.checkpointsUnavailable) return false;
    // If we've loaded checkpoints for the thread and got zero back but the
    // thread has had turns, assume the workspace isn't a git repo. Keep the
    // tab hidden to avoid a confusing "no turns" empty state.
    if (store.checkpointsLoaded && store.checkpoints.length === 0 && threadHasTurns) {
      return false;
    }
    return true;
  });

  // --- Turn diff loading ---

  async function loadSelectedTurnDiff(): Promise<void> {
    if (!threadId) return;
    const turnIndex = store.selectedTurnIndex;
    const mode = store.turnCompareMode;
    if (turnIndex === null) {
      turnDiffText = '';
      return;
    }
    const cached = store.readTurnDiff(threadId, turnIndex, mode);
    if (cached !== undefined) {
      turnDiffText = cached;
      return;
    }
    loadingDiff = true;
    try {
      const text =
        mode === 'next'
          ? await GetTurnDiff(threadId, turnIndex)
          : await GetCheckpointToWorktreeDiff(threadId, turnIndex);
      const result = (text ?? '') as string;
      store.writeTurnDiff(threadId, turnIndex, mode, result);
      turnDiffText = result;
    } catch (err) {
      store.setError(`Failed to load turn diff: ${errString(err)}`);
      turnDiffText = '';
    } finally {
      loadingDiff = false;
    }
  }

  async function loadWorktreeDiff(force = false): Promise<void> {
    if (!threadId) return;
    if (!force && worktreeText.length > 0) return;
    worktreeLoading = true;
    try {
      const text = await GetWorkingTreeDiff(threadId);
      worktreeText = (text ?? '') as string;
    } catch (err) {
      store.setError(`Failed to load working tree diff: ${errString(err)}`);
      worktreeText = '';
    } finally {
      worktreeLoading = false;
    }
  }

  async function loadCumulativeDiff(force = false): Promise<void> {
    if (!threadId) return;
    if (force) store.invalidateCumulative();
    cumulativeLoading = true;
    try {
      const text = await aggregateAgentDiffs(
        cumulativeEntries,
        (id) => GetPayloadData(id) as Promise<string>,
        store.cumulativeCache,
      );
      cumulativeText = text;
    } catch (err) {
      store.setError(`Failed to aggregate agent diffs: ${errString(err)}`);
      cumulativeText = '';
    } finally {
      cumulativeLoading = false;
    }
  }

  // --- Event handlers wired to the panel header & views ---

  function handleSelectSource(next: DiffPanelSource): void {
    store.setSource(next);
  }

  function handleSelectTurn(turnIndex: number): void {
    store.selectTurn(turnIndex);
  }

  function handleCompareModeChange(mode: TurnCompareMode): void {
    store.setTurnCompareMode(mode);
  }

  // --- Revert dialog state and wiring ---

  let revertOpen = $state(false);
  let revertTurn = $state<number | null>(null);
  let reverting = $state(false);

  function handleRequestRevert(turnIndex: number): void {
    revertTurn = turnIndex;
    revertOpen = true;
  }

  function handleRevertCancel(): void {
    if (reverting) return; // Don't close while an in-flight revert is running.
    revertOpen = false;
    revertTurn = null;
  }

  async function handleRevert(mode: RevertMode): Promise<void> {
    if (revertTurn === null || !threadId || !pane.thread) return;
    reverting = true;
    try {
      await RevertToTurn(threadId, revertTurn, mode);
      revertOpen = false;
      revertTurn = null;
      addToast('success', mode === 'fork' ? 'Thread forked' : 'Thread reverted');
      // Reload the pane against the same thread — truncates in-memory items,
      // resets streaming state, and refreshes checkpoints.
      await pane.switchThread(pane.thread);
    } catch (err) {
      addToast('error', `Revert failed: ${errString(err)}`);
    } finally {
      reverting = false;
    }
  }

  function handleClose(): void {
    pane.setDiffPanelOpen(false);
  }

  function handleToggleWordWrap(): void {
    void updateSetting('diffWordWrap', !getSettings().diffWordWrap);
  }

  // Keyboard nav: ← / → step through the checkpoint list in the turn view.
  function handleKeydown(ev: KeyboardEvent): void {
    if (!store.open) return;
    if (store.source !== 'turn') return;
    // Don't steal keys the user typed into a control inside the panel.
    const target = ev.target as HTMLElement | null;
    const tag = target?.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return;
    if (ev.metaKey || ev.ctrlKey || ev.altKey) return;

    if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight') return;
    const list = store.checkpoints;
    if (list.length === 0) return;
    const current = store.selectedTurnIndex;
    const idx = list.findIndex((c) => c.turnIndex === current);
    let nextIdx: number;
    if (ev.key === 'ArrowLeft') {
      nextIdx = idx <= 0 ? 0 : idx - 1;
    } else {
      nextIdx = idx === -1 ? 0 : Math.min(list.length - 1, idx + 1);
    }
    const target2 = list[nextIdx];
    if (target2 && target2.turnIndex !== current) {
      ev.preventDefault();
      store.selectTurn(target2.turnIndex);
    }
  }

  // --- Wails event subscriptions ---

  let cancelCaptured: (() => void) | null = null;
  let cancelUnavailable: (() => void) | null = null;
  let cancelError: (() => void) | null = null;

  onMount(() => {
    cancelCaptured = wailsEventOn<CheckpointCapturedEvent | null>('checkpoint:captured', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      // A recapture produces a new diff, so drop every cached entry for this
      // turn across compare modes; the cumulative aggregation is also
      // invalidated because any turn's diff contributes to it.
      store.invalidateTurn(payload.threadId, payload.turnIndex);
      // If the re-captured turn is the one currently displayed, re-trigger
      // the load so the panel shows the fresh diff instead of leaving the
      // user on a stale snapshot until they re-select the turn.
      if (
        store.source === 'turn'
        && store.selectedTurnIndex === payload.turnIndex
      ) {
        void loadSelectedTurnDiff();
      }
      void refreshCheckpoints();
    });
    cancelUnavailable = wailsEventOn<CheckpointUnavailableEvent | null>('checkpoint:unavailable', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      store.markCheckpointsUnavailable(payload.reason ?? 'unknown');
    });
    cancelError = wailsEventOn<CheckpointErrorEvent | null>('checkpoint:error', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      store.setError(`Checkpoint capture failed (turn ${payload.turnIndex}): ${payload.error}`);
    });

    window.addEventListener('keydown', handleKeydown);
    void refreshCheckpoints();
  });

  onDestroy(() => {
    cancelCaptured?.();
    cancelUnavailable?.();
    cancelError?.();
    window.removeEventListener('keydown', handleKeydown);
  });

  // Auto-select the latest checkpoint the first time the list lands so the
  // user doesn't see a bare "pick a turn" screen.
  let lastAutoSelected = $state<number | null>(null);
  $effect(() => {
    const list = store.checkpoints;
    if (list.length === 0) return;
    if (store.selectedTurnIndex !== null) return;
    if (lastAutoSelected !== null) return;
    const latest = list[list.length - 1]!;
    store.selectTurn(latest.turnIndex);
    lastAutoSelected = latest.turnIndex;
  });

  // If the Turn tab becomes invisible (e.g. checkpoint:unavailable), drop the
  // user to the working-tree view so they're never stuck on a hidden tab.
  $effect(() => {
    if (activeSource === 'turn' && !turnTabVisible) {
      store.setSource('worktree');
    }
  });

  // Reload turn diff whenever its inputs change.
  $effect(() => {
    store.selectedTurnIndex;
    store.turnCompareMode;
    if (activeSource === 'turn') {
      void loadSelectedTurnDiff();
    }
  });

  // Lazy-load worktree and cumulative views when the user first opens them.
  $effect(() => {
    if (activeSource === 'worktree') {
      void loadWorktreeDiff(false);
    } else if (activeSource === 'cumulative') {
      void loadCumulativeDiff(false);
    }
  });
</script>

<aside
  class="flex flex-col border-t border-border bg-surface-0 shrink-0"
  style="height: 340px"
  aria-label="Diff panel"
  data-testid="diff-panel-drawer"
>
  <PanelHeader
    source={activeSource}
    {turnTabVisible}
    wordWrap={getSettings().diffWordWrap}
    onSelectSource={handleSelectSource}
    onToggleWordWrap={handleToggleWordWrap}
    onClose={handleClose}
  />

  {#if store.error}
    <ErrorBanner message={store.error} onDismiss={() => store.setError(null)} />
  {/if}

  {#if store.checkpointsUnavailable && activeSource !== 'worktree'}
    <div class="border-b border-border bg-surface-1 px-3 py-2 text-xs text-text-secondary">
      Workspace is not a git repo — per-turn checkpoints are unavailable.
    </div>
  {/if}

  {#if activeSource === 'turn'}
    <TurnDiffView
      {store}
      checkpoints={store.checkpoints}
      {loadingDiff}
      diffText={turnDiffText}
      onSelectTurn={handleSelectTurn}
      onCompareModeChange={handleCompareModeChange}
      onRequestRevert={handleRequestRevert}
    />
  {:else if activeSource === 'worktree'}
    <WorkingTreeView
      loading={worktreeLoading}
      diffText={worktreeText}
      onRefresh={() => void loadWorktreeDiff(true)}
    />
  {:else}
    <CumulativeView
      loading={cumulativeLoading}
      diffText={cumulativeText}
      stats={cumulativeStats}
      onRefresh={() => void loadCumulativeDiff(true)}
    />
  {/if}
</aside>

<RevertDialog
  open={revertOpen}
  turnIndex={revertTurn ?? 0}
  provider={(pane.thread?.provider ?? '').toLowerCase()}
  onRevert={handleRevert}
  onCancel={handleRevertCancel}
/>
