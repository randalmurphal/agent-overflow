<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import RhsSidebarResizer from './RhsSidebarResizer.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { wailsEventOn } from '../../stores/events';
  import {
    DIFF_PANEL_MIN_WIDTH,
    getDiffPanelMaxWidth,
    getDiffPanelWidth,
    persistDiffPanelWidth,
    setDiffPanelWidthLive,
  } from '../../stores/diffPanelLayout.svelte';
  import {
    GetCheckpointRangeDiff,
    GetSessionAgentDiff,
    GetWorkspaceCurrentDiff,
    ListThreadCheckpoints,
    RevertToCheckpoint,
  } from '../../stores/bindings';
  import type {
    Checkpoint,
    CheckpointCapturedEvent,
    CheckpointErrorEvent,
    CheckpointRevertedEvent,
    CheckpointUnavailableEvent,
    RevertMode,
  } from '../../types/checkpoint';
  import { parsePatchFiles } from '../../utils/patchFiles';
  import { buildRevertAffectedFiles } from '../../utils/checkpointRevertPreview';
  import RevertDialog from './diff-panel/RevertDialog.svelte';
  import DiffPanelHeaderBar from './diff-panel/DiffPanelHeaderBar.svelte';
  import DiffPanelChipStrip from './diff-panel/DiffPanelChipStrip.svelte';
  import DiffPanelFileCard from './diff-panel/DiffPanelFileCard.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  const checkpoints = $derived(pane.diffPanel.checkpoints);
  // Chip strip shows the baseline (count=0) plus turns that produced
  // file changes. Empty turns add visual noise without giving the user
  // anything to inspect — the inline TurnDiffBadge already follows the
  // same rule via buildTurnDiffView's null-return.
  const visibleCheckpoints = $derived(
    checkpoints.filter(
      (c) => c.checkpointTurnCount === 0 || (c.files?.length ?? 0) > 0,
    ),
  );
  const selectedTurnCount = $derived(pane.diffPanel.selectedCheckpointTurnCount);
  const error = $derived(pane.diffPanel.error);
  const viewMode = $derived(pane.diffPanel.viewMode);
  const tabMode = $derived(pane.diffPanel.tabMode);
  let diffText = $state('');
  let loading = $state(false);
  let expanded = $state<Set<string>>(new Set());
  let revertOpen = $state(false);
  let reverting = $state(false);
  let checkpointRequestID = 0;
  let diffRequestID = 0;

  const threadId = $derived(pane.thread?.id ?? null);
  const latestTurnCount = $derived.by(() => {
    const latest = checkpoints.at(-1);
    return latest ? latest.checkpointTurnCount : 0;
  });
  const selectedRange = $derived.by(() => {
    if (selectedTurnCount === null) return { from: 0, to: latestTurnCount };
    return { from: Math.max(0, selectedTurnCount - 1), to: selectedTurnCount };
  });
  const selectedCheckpoint = $derived(
    selectedTurnCount === null
      ? null
      : checkpoints.find((c) => c.checkpointTurnCount === selectedTurnCount) ?? null,
  );
  // Per-turn tab filters the parsed PatchFile[] to the selected turn's
  // tool_paths so manual edits to unrelated files don't leak into the
  // "what did the agent do this turn?" view. Session and Workspace tabs
  // bypass the filter — the backend already constrains Session by
  // cumulative tool_paths, and Workspace is intentionally unfiltered.
  const filterPaths = $derived.by(() => {
    if (tabMode !== 'per-turn') return null;
    if (!selectedCheckpoint) return null;
    const paths = selectedCheckpoint.toolPaths ?? [];
    if (paths.length === 0) return null;
    return new Set(paths);
  });
  const allFiles = $derived(parsePatchFiles(diffText));
  const files = $derived.by(() => {
    if (!filterPaths) return allFiles;
    return allFiles.filter((file) => filterPaths.has(file.path));
  });
  const totals = $derived.by(() => files.reduce(
    (acc, file) => ({
      files: acc.files + 1,
      additions: acc.additions + file.additions,
      deletions: acc.deletions + file.deletions,
    }),
    { files: 0, additions: 0, deletions: 0 },
  ));
  const wordWrap = $derived(getSettings().diffWordWrap);
  const showChipStrip = $derived(tabMode === 'per-turn');
  const showRevert = $derived(tabMode === 'per-turn' && selectedTurnCount !== null);
  const revertAffectedFiles = $derived(buildRevertAffectedFiles(checkpoints, selectedTurnCount));

  async function refreshCheckpoints(): Promise<void> {
    const requestID = ++checkpointRequestID;
    if (!threadId) {
      pane.diffPanel.setCheckpoints([]);
      return;
    }
    try {
      const next = ((await ListThreadCheckpoints(threadId)) ?? []) as Checkpoint[];
      if (requestID !== checkpointRequestID) return;
      const sorted = [...next].sort((a, b) => a.checkpointTurnCount - b.checkpointTurnCount);
      pane.diffPanel.setCheckpoints(sorted);
      if (selectedTurnCount !== null && !sorted.some((c) => c.checkpointTurnCount === selectedTurnCount)) {
        pane.diffPanel.selectCheckpointTurnCount(null);
      }
    } catch (err) {
      if (requestID !== checkpointRequestID) return;
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function loadDiff(): Promise<void> {
    const requestID = ++diffRequestID;
    if (!threadId) {
      if (requestID === diffRequestID) diffText = '';
      return;
    }
    // Per-turn and session views need a checkpoint history to anchor the
    // diff; workspace view is checkpoint-independent and just wants a
    // current `git diff HEAD`.
    if (tabMode !== 'workspace' && checkpoints.length === 0) {
      if (requestID === diffRequestID) diffText = '';
      return;
    }
    loading = true;
    pane.diffPanel.setError(null);
    try {
      let nextDiff = '';
      if (tabMode === 'per-turn') {
        const range = selectedRange;
        nextDiff = ((await GetCheckpointRangeDiff(threadId, range.from, range.to)) ?? '') as string;
      } else if (tabMode === 'session') {
        nextDiff = ((await GetSessionAgentDiff(threadId)) ?? '') as string;
      } else {
        nextDiff = ((await GetWorkspaceCurrentDiff(threadId)) ?? '') as string;
      }
      if (requestID !== diffRequestID) return;
      diffText = nextDiff;
      expanded = new Set();
    } catch (err) {
      if (requestID !== diffRequestID) return;
      diffText = '';
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    } finally {
      if (requestID === diffRequestID) loading = false;
    }
  }

  function selectCheckpoint(turnCount: number | null): void {
    pane.diffPanel.selectCheckpointTurnCount(turnCount);
  }

  function toggleFile(path: string): void {
    const next = new Set(expanded);
    if (next.has(path)) next.delete(path);
    else next.add(path);
    expanded = next;
  }

  function setAllFiles(open: boolean): void {
    if (open && totals.files > 40 && !window.confirm(`Expand all ${totals.files} changed files? Large diffs can take a moment to render.`)) {
      return;
    }
    expanded = open ? new Set(files.map((file) => file.path)) : new Set();
  }

  async function handleRevert(mode: RevertMode): Promise<void> {
    if (!threadId || selectedTurnCount === null || !pane.thread || reverting) return;
    reverting = true;
    try {
      await RevertToCheckpoint(threadId, selectedTurnCount, mode);
      revertOpen = false;
      addToast('success', mode === 'conversation-only' ? 'Conversation reverted' : 'Conversation and files reverted');
      await pane.switchThread(pane.thread);
    } catch (err) {
      addToast('error', `Revert failed: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      reverting = false;
    }
  }

  let cancelCaptured: (() => void) | null = null;
  let cancelUpdated: (() => void) | null = null;
  let cancelUnavailable: (() => void) | null = null;
  let cancelError: (() => void) | null = null;
  let cancelReverted: (() => void) | null = null;

  onMount(() => {
    cancelCaptured = wailsEventOn<CheckpointCapturedEvent | null>('checkpoint:captured', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      void refreshCheckpoints();
    });
    cancelUpdated = wailsEventOn<CheckpointCapturedEvent | null>('checkpoint:updated', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      void refreshCheckpoints();
    });
    cancelUnavailable = wailsEventOn<CheckpointUnavailableEvent | null>('checkpoint:unavailable', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      pane.diffPanel.markCheckpointsUnavailable(payload.reason);
      pane.diffPanel.setError('Workspace is not a git repo. Checkpoint diffs are unavailable.');
    });
    cancelError = wailsEventOn<CheckpointErrorEvent | null>('checkpoint:error', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      pane.diffPanel.setError(`Checkpoint failed: ${payload.error}`);
    });
    // Refresh after a successful revert so chips reflect the truncated
    // checkpoint history (post-revert refs are deleted by the backend).
    cancelReverted = wailsEventOn<CheckpointRevertedEvent | null>('checkpoint:reverted', (payload) => {
      if (!payload || payload.threadId !== threadId) return;
      void refreshCheckpoints();
    });
  });

  onDestroy(() => {
    cancelCaptured?.();
    cancelUpdated?.();
    cancelUnavailable?.();
    cancelError?.();
    cancelReverted?.();
  });

  $effect(() => {
    threadId;
    void refreshCheckpoints();
  });

  $effect(() => {
    threadId;
    selectedTurnCount;
    latestTurnCount;
    tabMode;
    void loadDiff();
  });
</script>

<aside
  aria-label="Diff Panel"
  data-testid="diff-panel-drawer"
  style="width: {getDiffPanelWidth()}px"
  class="relative flex h-full shrink-0 flex-col border-l border-border bg-surface-0"
>
  <header class="border-b border-border bg-surface-1/70">
    <DiffPanelHeaderBar
      {totals}
      {viewMode}
      setViewMode={(mode) => pane.diffPanel.setViewMode(mode)}
      {wordWrap}
      setWordWrap={(next) => updateSetting('diffWordWrap', next)}
      {tabMode}
      setTabMode={(mode) => pane.diffPanel.setTabMode(mode)}
      onClose={() => pane.setDiffPanelOpen(false)}
    />
    {#if showChipStrip}
      <DiffPanelChipStrip
        {visibleCheckpoints}
        {selectedTurnCount}
        onSelectTurn={selectCheckpoint}
        {showRevert}
        {reverting}
        onRevertClick={() => (revertOpen = true)}
      />
    {/if}
  </header>

  <div class="flex min-h-0 flex-1 flex-col">
    {#if error}
      <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-[12px] text-error" data-testid="diff-panel-error">{error}</div>
    {/if}
    <div class="flex items-center gap-2 border-b border-border-subtle px-3 py-2">
      <button class="rounded border border-border-subtle px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={() => setAllFiles(true)}>Expand all</button>
      <button class="rounded border border-border-subtle px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={() => setAllFiles(false)}>Collapse all</button>
      <span class="ml-auto text-[11px] text-fg-muted">
        {#if tabMode === 'per-turn'}
          {selectedRange.from} → {selectedRange.to}
        {:else if tabMode === 'session'}
          Agent edits since baseline
        {:else}
          Uncommitted vs HEAD
        {/if}
      </span>
    </div>

    <div class="min-h-0 flex-1 overflow-auto px-3 py-3">
      {#if loading}
        <div class="py-8 text-center text-[13px] text-fg-muted" role="status">Loading diff...</div>
      {:else if checkpoints.length === 0}
        <div class="py-8 text-center text-[13px] text-fg-muted">No checkpoints yet.</div>
      {:else if files.length === 0}
        <div class="py-8 text-center text-[13px] text-fg-muted">No changes in this range.</div>
      {:else}
        <div class="space-y-2" data-testid="diff-viewer">
          {#each files as file (file.path)}
            <DiffPanelFileCard
              {file}
              open={expanded.has(file.path)}
              {viewMode}
              {wordWrap}
              onToggle={() => toggleFile(file.path)}
            />
          {/each}
        </div>
      {/if}
    </div>
  </div>

  <RhsSidebarResizer
    width={getDiffPanelWidth()}
    minWidth={DIFF_PANEL_MIN_WIDTH}
    getMaxWidth={getDiffPanelMaxWidth}
    onResizeLive={setDiffPanelWidthLive}
    onResizeEnd={persistDiffPanelWidth}
    ariaLabel="Resize Diff Panel"
    testId="diff-panel-resizer"
  />
</aside>

<RevertDialog
  open={revertOpen}
  checkpointTurnCount={selectedTurnCount ?? 0}
  provider={(pane.thread?.provider ?? '').toLowerCase()}
  affectedFiles={revertAffectedFiles}
  reverting={reverting}
  onRevert={handleRevert}
  onCancel={() => {
    if (!reverting) revertOpen = false;
  }}
/>

