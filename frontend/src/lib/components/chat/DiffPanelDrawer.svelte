<script lang="ts">
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import {
    GetMessageCheckpointDiff,
    GetSessionAgentDiff,
    GetWorkspaceCurrentDiff,
    ListThreadCheckpoints,
  } from '../../stores/bindings';
  import type { Checkpoint } from '../../types/checkpoint';
  import { parsePatchFiles, patchFileRowId } from '../../utils/patchFiles';
  import DiffPanelHeaderBar from './diff-panel/DiffPanelHeaderBar.svelte';
  import DiffPanelChipStrip from './diff-panel/DiffPanelChipStrip.svelte';
  import DiffPanelFileCard from './diff-panel/DiffPanelFileCard.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let diffText = $state('');
  let loading = $state(false);
  let expanded = $state<Set<string>>(new Set());
  let checkpointRequestID = 0;
  let diffRequestID = 0;

  const checkpoints = $derived(pane.diffPanel.checkpoints);
  const visibleCheckpoints = $derived(
    checkpoints.filter((c) => c.turnIndex === 0 || (c.files?.length ?? 0) > 0),
  );
  const selectedUserItemId = $derived(pane.diffPanel.selectedCheckpointUserItemId);
  const selectedCheckpoint = $derived(
    selectedUserItemId === null
      ? null
      : checkpoints.find((c) => c.userItemId === selectedUserItemId) ?? null,
  );
  const error = $derived(pane.diffPanel.error);
  const viewMode = $derived(pane.diffPanel.viewMode);
  const tabMode = $derived(pane.diffPanel.tabMode);
  const threadId = $derived(pane.thread?.id ?? null);
  const files = $derived(parsePatchFiles(diffText));
  const fileRows = $derived(files.map((file, index) => ({ file, rowId: patchFileRowId(file, index) })));
  const totals = $derived.by(() => files.reduce(
    (acc, file) => ({
      files: acc.files + 1,
      additions: acc.additions + file.additions,
      deletions: acc.deletions + file.deletions,
    }),
    { files: 0, additions: 0, deletions: 0 },
  ));
  const wordWrap = $derived(getSettings().diffWordWrap);
  const showChipStrip = $derived(tabMode === 'messages');

  async function refreshCheckpoints(): Promise<void> {
    const requestID = ++checkpointRequestID;
    if (!threadId) {
      pane.diffPanel.setCheckpoints([]);
      return;
    }
    try {
      const next = ((await ListThreadCheckpoints(threadId)) ?? []) as Checkpoint[];
      if (requestID !== checkpointRequestID) return;
      const sorted = [...next].sort((a, b) => a.turnIndex - b.turnIndex);
      pane.diffPanel.setCheckpoints(sorted);
      if (selectedUserItemId !== null && !sorted.some((c) => c.userItemId === selectedUserItemId)) {
        pane.diffPanel.selectCheckpointUserItem(null);
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
    if (tabMode !== 'workspace' && checkpoints.length === 0) {
      if (requestID === diffRequestID) diffText = '';
      return;
    }
    loading = true;
    pane.diffPanel.setError(null);
    try {
      let nextDiff = '';
      if (tabMode === 'messages') {
        nextDiff = selectedCheckpoint
          ? (((await GetMessageCheckpointDiff(threadId, selectedCheckpoint.userItemId)) ?? '') as string)
          : (((await GetSessionAgentDiff(threadId)) ?? '') as string);
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

  function selectCheckpoint(userItemId: string | null): void {
    pane.diffPanel.selectCheckpointUserItem(userItemId);
  }

  function jumpToSelectedCheckpoint(): void {
    if (!selectedCheckpoint) return;
    pane.requestScrollToItem(selectedCheckpoint.userItemId, {
      behavior: 'animated',
      flash: true,
    });
  }

  function toggleFile(rowId: string): void {
    const next = new Set(expanded);
    if (next.has(rowId)) next.delete(rowId);
    else next.add(rowId);
    expanded = next;
  }

  function setAllFiles(open: boolean): void {
    if (open && totals.files > 40 && !window.confirm(`Expand all ${totals.files} changed files? Large diffs can take a moment to render.`)) {
      return;
    }
    expanded = open ? new Set(fileRows.map((row) => row.rowId)) : new Set();
  }

  $effect(() => {
    threadId;
    void refreshCheckpoints();
  });

  $effect(() => {
    threadId;
    selectedUserItemId;
    tabMode;
    void loadDiff();
  });
</script>

<section
  aria-label="Diff Panel"
  data-testid="diff-panel-drawer"
  class="flex min-h-0 flex-1 flex-col bg-surface-0"
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
        selectedUserItemId={selectedUserItemId}
        onSelectCheckpoint={selectCheckpoint}
        onJumpToCheckpoint={jumpToSelectedCheckpoint}
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
        {#if tabMode === 'messages'}
          {selectedCheckpoint ? `Message ${selectedCheckpoint.turnIndex + 1}` : 'All message checkpoints'}
        {:else}
          Uncommitted vs HEAD
        {/if}
      </span>
    </div>

    <div class="min-h-0 flex-1 overflow-auto px-3 py-3">
      {#if loading}
        <div class="py-8 text-center text-[13px] text-fg-muted" role="status">Loading diff...</div>
      {:else if checkpoints.length === 0 && tabMode !== 'workspace'}
        <div class="py-8 text-center text-[13px] text-fg-muted">No checkpoints yet.</div>
      {:else if files.length === 0}
        <div class="py-8 text-center text-[13px] text-fg-muted">No changes in this range.</div>
      {:else}
        <div class="space-y-2" data-testid="diff-viewer">
          {#each fileRows as { file, rowId } (rowId)}
            <DiffPanelFileCard
              {file}
              open={expanded.has(rowId)}
              workspacePath={paneWorkspacePath(pane)}
              {viewMode}
              {wordWrap}
              onToggle={() => toggleFile(rowId)}
            />
          {/each}
        </div>
      {/if}
    </div>
  </div>
</section>
