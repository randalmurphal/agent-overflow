<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import { wailsEventOn } from '../../stores/events';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type {
    CheckpointCapturedEvent,
    CheckpointErrorEvent,
    CheckpointUnavailableEvent,
  } from '../../types/checkpoint';
  import type { DiffPanelSource, TurnCompareMode } from '../../stores/diffPanel.svelte';
  import { RevertToTurn } from '../../stores/bindings';
  import type { RevertMode } from '../../types/checkpoint';
  import { addToast } from '../../stores/toast.svelte';
  import RevertDialog from './diff-panel/RevertDialog.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import CumulativeView from './diff-panel/CumulativeView.svelte';
  import ErrorBanner from './diff-panel/ErrorBanner.svelte';
  import PanelHeader from './diff-panel/PanelHeader.svelte';
  import TurnDiffView from './diff-panel/TurnDiffView.svelte';
  import WorkingTreeView from './diff-panel/WorkingTreeView.svelte';
  import { createCumulativeDiffItems } from './diff-panel/cumulativeDiffItems.svelte';
  import { createDiffPanelSources } from './diff-panel/diffPanelSources.svelte';
  import { selectTurnForKey } from './diff-panel/diffPanelKeyboard';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  const store = $derived(pane.diffPanel);
  const threadId = $derived(pane.thread?.id ?? null);

  // Cumulative diff state is sourced from ListThreadDiffPayloads — a
  // dedicated backend binding that returns every diff- or tool_result-
  // kind item for the thread, independent of the pane's loaded window.
  // The factory owns the fetch/debounce/subscribe wiring so this file
  // stays focused on panel composition.
  const cumulative = createCumulativeDiffItems({
    getThreadId: () => pane.thread?.id ?? null,
  });

  // pane.diffPanel is stable for the lifetime of the pane (created once in
  // createThreadPane and reset internally), so passing the initial reference
  // is intentional. Svelte's static check can't see that — but the module
  // never captures `pane` directly, only this dereferenced store.
  // svelte-ignore state_referenced_locally
  const diffPanelStore = pane.diffPanel;
  const sources = createDiffPanelSources({
    getThreadId: () => pane.thread?.id ?? null,
    store: diffPanelStore,
    getCumulativeEntries: () => cumulative.entries,
  });

  // Active-tab derivation stays in sync with the store.
  let activeSource = $derived<DiffPanelSource>(store.source);

  // A thread "has had turns" when the pane knows a loaded floor. The
  // pane sets `oldestLoadedTurnIndex` to a non-null value when the
  // backend returned any paged items from ListRecentThreadItems — this
  // is semantically identical to "at least one persisted item exists"
  // but costs O(1) instead of O(window). Keeps working with a paged
  // window where pane.items is a bounded tail.
  let threadHasTurns = $derived(pane.oldestLoadedTurnIndex !== null);
  let turnTabVisible = $derived.by(() => {
    if (store.checkpointsUnavailable) return false;
    if (store.checkpointsLoaded && store.checkpoints.length === 0 && threadHasTurns) {
      return false;
    }
    return true;
  });

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
      const msg = err instanceof Error ? err.message : String(err);
      addToast('error', `Revert failed: ${msg}`);
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
  // Pure dispatch lives in diffPanelKeyboard.ts; this shell just forwards
  // the DOM event fields and acts on the suggested turn index.
  function handleKeydown(ev: KeyboardEvent): void {
    const target = ev.target as HTMLElement | null;
    const nextTurn = selectTurnForKey({
      key: ev.key,
      metaKey: ev.metaKey,
      ctrlKey: ev.ctrlKey,
      altKey: ev.altKey,
      targetTag: target?.tagName,
      source: store.source,
      panelOpen: store.open,
      checkpoints: store.checkpoints,
      selectedTurnIndex: store.selectedTurnIndex,
    });
    if (nextTurn === null) return;
    ev.preventDefault();
    store.selectTurn(nextTurn);
  }

  // --- Wails event subscriptions ---

  let cancelCaptured: (() => void) | null = null;
  let cancelUnavailable: (() => void) | null = null;
  let cancelError: (() => void) | null = null;

  onMount(() => {
    cancelCaptured = wailsEventOn<CheckpointCapturedEvent | null>('checkpoint:captured', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      // A recapture produces a new diff, so drop every cached entry for this
      // turn across compare modes; cumulative is invalidated too because
      // any turn's diff contributes to it.
      store.invalidateTurn(payload.threadId, payload.turnIndex);
      if (store.source === 'turn' && store.selectedTurnIndex === payload.turnIndex) {
        void sources.loadSelectedTurnDiff();
      }
      void sources.refreshCheckpoints();
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
    void sources.refreshCheckpoints();
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
      void sources.loadSelectedTurnDiff();
    }
  });

  // Lazy-load worktree and cumulative views when the user first opens them.
  $effect(() => {
    if (activeSource === 'worktree') {
      void sources.loadWorktreeDiff(false);
    } else if (activeSource === 'cumulative') {
      void sources.loadCumulativeDiff(false);
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
      loadingDiff={sources.loadingTurn}
      diffText={sources.turnDiffText}
      onSelectTurn={handleSelectTurn}
      onCompareModeChange={handleCompareModeChange}
      onRequestRevert={handleRequestRevert}
    />
  {:else if activeSource === 'worktree'}
    <WorkingTreeView
      loading={sources.worktreeLoading}
      diffText={sources.worktreeText}
      onRefresh={() => void sources.loadWorktreeDiff(true)}
    />
  {:else}
    <CumulativeView
      loading={sources.cumulativeLoading}
      diffText={sources.cumulativeText}
      stats={cumulative.stats}
      onRefresh={() => void sources.loadCumulativeDiff(true)}
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
