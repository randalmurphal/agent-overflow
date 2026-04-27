<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import X from 'lucide-svelte/icons/x';
  import Columns2 from 'lucide-svelte/icons/columns-2';
  import Rows3 from 'lucide-svelte/icons/rows-3';
  import WrapText from 'lucide-svelte/icons/wrap-text';
  import RotateCcw from 'lucide-svelte/icons/rotate-ccw';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import EditorLink from '../common/EditorLink.svelte';
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
  import { buildSplitRows, parsePatchFiles, type PatchFile, type SplitDiffRow } from '../../utils/patchFiles';
  import { lineTintClass } from '../../utils/diffLineTint';
  import RevertDialog from './diff-panel/RevertDialog.svelte';

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
  let diffText = $state('');
  let loading = $state(false);
  let expanded = $state<Set<string>>(new Set());
  let splitRowsCache = $state<Map<string, SplitDiffRow[]>>(new Map());
  let revertOpen = $state(false);
  let reverting = $state(false);
  let checkpointRequestID = 0;
  let diffRequestID = 0;

  const threadId = $derived(pane.thread?.id ?? null);
  const latestTurnCount = $derived.by(() => {
    const latest = checkpoints.at(-1);
    return latest ? checkpointTurnCount(latest) : 0;
  });
  const selectedRange = $derived.by(() => {
    if (selectedTurnCount === null) return { from: 0, to: latestTurnCount };
    return { from: Math.max(0, selectedTurnCount - 1), to: selectedTurnCount };
  });
  const files = $derived(parsePatchFiles(diffText));
  const totals = $derived.by(() => files.reduce(
    (acc, file) => ({
      files: acc.files + 1,
      additions: acc.additions + file.additions,
      deletions: acc.deletions + file.deletions,
    }),
    { files: 0, additions: 0, deletions: 0 },
  ));
  const wordWrap = $derived(getSettings().diffWordWrap);

  async function refreshCheckpoints(): Promise<void> {
    const requestID = ++checkpointRequestID;
    if (!threadId) {
      pane.diffPanel.setCheckpoints([]);
      return;
    }
    try {
      const next = ((await ListThreadCheckpoints(threadId)) ?? []) as Checkpoint[];
      if (requestID !== checkpointRequestID) return;
      const sorted = [...next].sort((a, b) => checkpointTurnCount(a) - checkpointTurnCount(b));
      pane.diffPanel.setCheckpoints(sorted);
      if (selectedTurnCount !== null && !sorted.some((c) => checkpointTurnCount(c) === selectedTurnCount)) {
        pane.diffPanel.selectCheckpointTurnCount(null);
      }
    } catch (err) {
      if (requestID !== checkpointRequestID) return;
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function loadDiff(): Promise<void> {
    const requestID = ++diffRequestID;
    if (!threadId || checkpoints.length === 0) {
      if (requestID === diffRequestID) diffText = '';
      return;
    }
    loading = true;
    pane.diffPanel.setError(null);
    try {
      const range = selectedRange;
      const nextDiff = ((await GetCheckpointRangeDiff(threadId, range.from, range.to)) ?? '') as string;
      if (requestID !== diffRequestID) return;
      diffText = nextDiff;
      expanded = new Set();
      splitRowsCache = new Map();
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

  function checkpointTurnCount(checkpoint: Checkpoint): number {
    return checkpoint.checkpointTurnCount;
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

  function splitCellClass(line: PatchFile['lines'][number] | null): string {
    if (!line) return 'text-fg-muted/40';
    return lineTintClass(line.type);
  }

  function splitRowsFor(file: PatchFile): SplitDiffRow[] {
    const cached = splitRowsCache.get(file.path);
    if (cached) return cached;
    const rows = buildSplitRows(file.lines);
    splitRowsCache = new Map(splitRowsCache).set(file.path, rows);
    return rows;
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
    <div class="flex items-center gap-2 px-3 py-2">
      <div class="min-w-0 flex-1">
        <div class="text-[12px] font-semibold uppercase tracking-[0.08em] text-fg-muted">Checkpoint Diff</div>
        <div class="mt-0.5 flex items-center gap-2 text-[12px] text-fg-muted">
          <span>{totals.files} files</span>
          <span class="text-success">+{totals.additions}</span>
          <span class="text-error">-{totals.deletions}</span>
        </div>
      </div>
      <button
        class="rounded p-1.5 hover:bg-surface-2 {viewMode === 'stacked' ? 'bg-surface-2 text-fg' : 'text-fg-muted'}"
        title="Stacked View"
        aria-pressed={viewMode === 'stacked'}
        onclick={() => pane.diffPanel.setViewMode('stacked')}
      >
        <Icon icon={Rows3} size={15} />
      </button>
      <button
        class="rounded p-1.5 hover:bg-surface-2 {viewMode === 'split' ? 'bg-surface-2 text-fg' : 'text-fg-muted'}"
        title="Split View"
        aria-pressed={viewMode === 'split'}
        onclick={() => pane.diffPanel.setViewMode('split')}
      >
        <Icon icon={Columns2} size={15} />
      </button>
      <button
        class="rounded p-1.5 hover:bg-surface-2 {wordWrap ? 'bg-surface-2 text-fg' : 'text-fg-muted'}"
        title="Wrap Lines"
        aria-pressed={wordWrap}
        onclick={() => updateSetting('diffWordWrap', !wordWrap)}
      >
        <Icon icon={WrapText} size={15} />
      </button>
      <button class="rounded p-1.5 text-fg-muted hover:bg-surface-2" aria-label="Close Diff Panel" data-testid="diff-panel-close" onclick={() => pane.setDiffPanelOpen(false)}>
        <Icon icon={X} size={15} />
      </button>
    </div>

    <div class="flex gap-1 overflow-x-auto border-t border-border-subtle px-3 py-2">
      <button
        class="shrink-0 rounded border px-2.5 py-1 text-[12px] {selectedTurnCount === null ? 'border-accent/60 bg-accent/15 text-accent' : 'border-border-subtle text-fg-muted hover:bg-surface-2'}"
        onclick={() => selectCheckpoint(null)}
        data-testid="diff-all-turns"
      >
        All turns
      </button>
      {#each visibleCheckpoints as checkpoint (checkpoint.id)}
        <button
          class="shrink-0 rounded border px-2.5 py-1 text-[12px] {selectedTurnCount === checkpointTurnCount(checkpoint) ? 'border-accent/60 bg-accent/15 text-accent' : 'border-border-subtle text-fg-muted hover:bg-surface-2'}"
          onclick={() => selectCheckpoint(checkpointTurnCount(checkpoint))}
          data-testid={`diff-turn-${checkpointTurnCount(checkpoint)}`}
        >
          {checkpointTurnCount(checkpoint) === 0 ? 'Baseline' : `Turn ${checkpointTurnCount(checkpoint)}`}
          {#if checkpoint.status && checkpoint.status !== 'ready'}
            <span class="ml-1 text-[10px] text-warning">{checkpoint.status}</span>
          {/if}
        </button>
      {/each}
      {#if selectedTurnCount !== null}
        <button
          class="ml-auto inline-flex shrink-0 items-center gap-1 rounded border border-error/40 px-2.5 py-1 text-[12px] text-error hover:bg-error/10"
          onclick={() => (revertOpen = true)}
          disabled={reverting}
          data-testid="diff-turn-revert"
        >
          <Icon icon={RotateCcw} size={13} />
          Revert
        </button>
      {/if}
    </div>
  </header>

  <div class="flex min-h-0 flex-1 flex-col">
    {#if error}
      <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-[12px] text-error" data-testid="diff-panel-error">{error}</div>
    {/if}
    <div class="flex items-center gap-2 border-b border-border-subtle px-3 py-2">
      <button class="rounded border border-border-subtle px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={() => setAllFiles(true)}>Expand all</button>
      <button class="rounded border border-border-subtle px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={() => setAllFiles(false)}>Collapse all</button>
      <span class="ml-auto text-[11px] text-fg-muted">
        {selectedRange.from} → {selectedRange.to}
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
            {@render FileCard(file, expanded.has(file.path), viewMode, wordWrap, () => toggleFile(file.path))}
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
  reverting={reverting}
  onRevert={handleRevert}
  onCancel={() => {
    if (!reverting) revertOpen = false;
  }}
/>

{#snippet FileCard(file: PatchFile, open: boolean, viewMode: 'stacked' | 'split', wordWrap: boolean, onToggle: () => void)}
  <section class="overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-card/30">
    <!--
      Header: button toggles open/closed, EditorLink sibling opens the
      file in the user's editor. Same dual-control layout used by
      DiffPreview to avoid nested interactives.
    -->
    <div class="group/diff-panel-file flex w-full items-center gap-2 px-3 py-2 hover:bg-surface-2/40">
      <button
        class="flex flex-1 min-w-0 items-center gap-2 text-left bg-transparent border-0 p-0 cursor-pointer"
        onclick={onToggle}
        data-testid="diff-panel-file-toggle"
        data-path={file.path}
      >
        <Icon icon={ChevronDown} size={14} class={open ? '' : '-rotate-90'} />
        <span class="rounded bg-accent/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-[0.08em] text-accent">FileChange</span>
        <span class="min-w-0 flex-1 truncate font-mono text-[12px] text-fg">{file.path}</span>
        <span class="text-[11px] text-success">+{file.additions}</span>
        <span class="text-[11px] text-error">-{file.deletions}</span>
      </button>
      <EditorLink
        path={file.path}
        asIcon
        stopPropagation
        class="opacity-0 group-hover/diff-panel-file:opacity-100 focus-visible:opacity-100"
      />
    </div>
    {#if open}
      {#if viewMode === 'split'}
        <div class="max-h-[42rem] overflow-auto border-t border-border-subtle bg-surface-0 font-mono text-[12px] leading-relaxed">
          {#each splitRowsFor(file) as row}
            <div class="grid grid-cols-2 border-b border-border-subtle/40 last:border-b-0">
              <pre class="min-w-0 border-r border-border-subtle/50 px-3 py-0.5 {wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'} {splitCellClass(row.left)}">{row.left?.content ?? ''}</pre>
              <pre class="min-w-0 px-3 py-0.5 {wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'} {splitCellClass(row.right)}">{row.right?.content ?? ''}</pre>
            </div>
          {/each}
        </div>
      {:else}
      <pre class="max-h-[42rem] overflow-auto border-t border-border-subtle bg-surface-0 p-3 font-mono text-[12px] leading-relaxed {wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'}">{#each file.lines as line}<span class="block {lineTintClass(line.type)}">{line.content}
</span>{/each}</pre>
      {/if}
    {/if}
  </section>
{/snippet}
