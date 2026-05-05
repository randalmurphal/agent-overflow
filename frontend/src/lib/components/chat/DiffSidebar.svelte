<script lang="ts">
  /*
   * Per-tool diff sidebar body. RhsSidebarShell owns the outer pane,
   * width, and resizer; ThreadPane's shared RHS slot guarantees this
   * body is mutex'd against PlanSidebar and DiffPanelDrawer.
   *
   * Patch text comes from the existing `createPayloadExpansion`
   * helper — same lazy fetch as inline DiffFileStack (32 KiB preview
   * → 256 KiB chunks → showFull). Multi-file payloads (Claude
   * `file_change` tool_results) are split via parsePatchFiles and
   * rendered as a stacked virtualized list.
   *
   * Lines render with line-tint backgrounds first; once tokens
   * arrive from the Shiki worker pool they fade in as colored
   * spans. Pre-tokenize render is always usable.
   */
  import { onDestroy, onMount, untrack } from 'svelte';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import type { DiffViewMode } from '../../stores/diffPanel.svelte';
  import { parsePatchFiles, patchFileRowId, type PatchFile } from '../../utils/patchFiles';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';
  import DiffSidebarBody from './DiffSidebarBody.svelte';
  import DiffSidebarHeader from './DiffSidebarHeader.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let payloadId = $derived(pane.activeDiffPayload?.payloadId ?? '');
  let focusFilePath = $derived(pane.activeDiffPayload?.filePath);

  // Reactive expansion handle. The thread id and payload id are read
  // through getters so the handle re-targets when the user opens a
  // new payload without re-mounting the whole sidebar.
  const expansion = createPayloadExpansion(
    () => payloadId || undefined,
    () => pane.thread?.id ?? undefined,
  );

  // Each new payloadId starts as expanded (we want the patch loaded
  // immediately) and resets the prior load. The reset/expand calls
  // mutate the expansion's internal `$state` (expanded, chunks…)
  // and also read it (e.g. `expand()` returns early when already
  // expanded). Wrapping in `untrack` keeps the effect's reactive
  // dep set restricted to `payloadId` — otherwise the read-during-
  // write on `expanded` triggers an effect-update loop.
  $effect(() => {
    const id = payloadId;
    untrack(() => {
      expansion.reset();
      if (id) {
        void expansion.expand();
      }
    });
  });

  // Per-panel view state. Word-wrap defaults to off (matches the
  // settings.diffWordWrap default for inline diffs); split view
  // defaults to off because most edits are small and stacked reads
  // better at narrow widths.
  //
  // expandedFiles + scrollTop live here (not in the body) so the
  // sidebar can persist them via pane.recordDiffSidebarUI on every
  // change, and re-apply them on mount via consumeDiffSidebarRestoreState.
  let viewMode: DiffViewMode = $state('stacked');
  let wordWrap = $state(false);
  let expandedFiles: string[] = $state([]);
  let scrollTop = $state(0);
  // Per-payload guard for the default-expand heuristic. Tracks the
  // payloadId we've already applied defaults for (or whose state
  // came from a restore). Resets when payloadId changes, blocking
  // re-fires on `parsedFiles` ticks (which churn while patch chunks
  // stream in) from clobbering the user's manual collapse/expand
  // edits or the just-restored snapshot.
  let defaultsAppliedFor: string | null = $state(null);

  let parsedFiles: PatchFile[] = $derived.by(() => {
    const data = expansion.displayData;
    if (!data) return [];
    return parsePatchFiles(data);
  });

  let totals = $derived.by(() => {
    let insertions = 0;
    let deletions = 0;
    for (const file of parsedFiles) {
      insertions += file.additions;
      deletions += file.deletions;
    }
    return { insertions, deletions };
  });

  let title = $derived.by(() => {
    if (!pane.activeDiffPayload) return 'Diff';
    if (parsedFiles.length === 1) return parsedFiles[0]?.path ?? 'Diff';
    if (focusFilePath) return focusFilePath;
    return 'Diff';
  });

  let subtitle = $derived.by(() => {
    if (parsedFiles.length <= 1) return '';
    return `${parsedFiles.length} files`;
  });

  // Esc closes the sidebar when focus is anywhere inside it.
  let asideEl: HTMLElement | undefined = $state(undefined);

  function onKeydown(event: KeyboardEvent): void {
    if (event.key !== 'Escape') return;
    if (!asideEl?.contains(document.activeElement)) return;
    event.preventDefault();
    pane.closeDiffSidebar();
  }

  onMount(() => {
    window.addEventListener('keydown', onKeydown);
    // Apply any restore state pushed by the pane during thread switch.
    // Done once per mount; subsequent changes flow via the $effect
    // below that records UI state back to the pane.
    //
    // Mark `defaultsAppliedFor = payloadId` even though `parsedFiles`
    // is still empty (patch fetch is in flight). The default-expand
    // effect compares against the same payloadId once data lands and
    // bails — preserving the restored expandedFiles/scrollTop.
    const restored = pane.consumeDiffSidebarRestoreState();
    if (restored) {
      viewMode = restored.viewMode;
      wordWrap = restored.wordWrap;
      expandedFiles = restored.expandedFiles;
      scrollTop = restored.scrollTop;
      defaultsAppliedFor = payloadId;
    }
  });

  onDestroy(() => {
    window.removeEventListener('keydown', onKeydown);
    expansion.reset();
  });

  // Default-expand heuristic: when a fresh payload lands and the
  // user hasn't restored from snapshot, expand all files when the
  // count is small, otherwise just the focus file. Keyed on
  // payloadId only — `parsedFiles` may stream in over multiple
  // chunks but the heuristic still runs at most once per payload.
  $effect(() => {
    const files = parsedFiles;
    const focus = focusFilePath;
    const id = payloadId;
    if (defaultsAppliedFor === id) return;
    if (files.length === 0) return;
    untrack(() => {
      const next: string[] = [];
      const small = files.length <= 5;
      for (let index = 0; index < files.length; index += 1) {
        const file = files[index];
        if (!file) continue;
        const rowId = patchFileRowId(file, index);
        if (small) next.push(rowId);
        else if (focus && file.path === focus) next.push(rowId);
      }
      if (next.length === 0 && files[0]) next.push(patchFileRowId(files[0], 0));
      expandedFiles = next;
      defaultsAppliedFor = id;
    });
  });

  // Reset bookkeeping + scroll when the payload *changes* (not on
  // initial mount — the onMount block above may have just applied
  // a restore, and this effect runs after mount in Svelte 5; without
  // the lastPayloadId guard we'd zero the restored scrollTop here
  // before the body has a chance to apply it).
  let lastPayloadId: string | null = null;
  $effect(() => {
    const id = payloadId;
    const wasTransition = lastPayloadId !== null && lastPayloadId !== id;
    lastPayloadId = id;
    if (!wasTransition) return;
    untrack(() => {
      defaultsAppliedFor = null;
      scrollTop = 0;
    });
  });

  // Push the merged UI state up to the pane so thread-switch
  // snapshots have the latest values.
  $effect(() => {
    const ui = {
      viewMode,
      wordWrap,
      expandedFiles,
      scrollTop,
    };
    untrack(() => {
      pane.recordDiffSidebarUI(ui);
    });
  });

  function toggleFile(rowId: string): void {
    const next = expandedFiles.slice();
    const idx = next.indexOf(rowId);
    if (idx >= 0) next.splice(idx, 1);
    else next.push(rowId);
    expandedFiles = next;
  }

  function expandAllFiles(): void {
    expandedFiles = parsedFiles.map((file, index) => patchFileRowId(file, index));
  }

  function collapseAllFiles(): void {
    expandedFiles = [];
  }

  function recordScroll(top: number): void {
    scrollTop = top;
  }
</script>

<section
  bind:this={asideEl}
  aria-label="Diff Sidebar"
  data-testid="diff-sidebar"
  class="flex min-h-0 flex-1 flex-col"
>
  <DiffSidebarHeader
    {title}
    {subtitle}
    insertions={totals.insertions}
    deletions={totals.deletions}
    {viewMode}
    {wordWrap}
    onChangeViewMode={(mode) => (viewMode = mode)}
    onToggleWordWrap={() => (wordWrap = !wordWrap)}
    onClose={() => pane.closeDiffSidebar()}
  />

  {#if expansion.error}
    <div class="flex flex-col items-start gap-2 px-3 py-3 text-xs text-text-secondary" data-testid="diff-sidebar-error">
      <p>Couldn't load this diff: <span class="text-fg-muted">{expansion.error}</span></p>
      <button
        type="button"
        onclick={() => void expansion.expand()}
        class="rounded text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        Retry
      </button>
    </div>
  {:else if expansion.loading && !expansion.displayData}
    <p class="px-3 py-3 text-xs text-text-secondary" role="status" aria-live="polite" data-testid="diff-sidebar-loading">
      Loading diff…
    </p>
  {:else if parsedFiles.length === 0}
    <p class="px-3 py-3 text-xs text-text-secondary" data-testid="diff-sidebar-empty">
      No diff content available for this tool call.
    </p>
  {:else}
    {#key payloadId}
      <DiffSidebarBody
        files={parsedFiles}
        {focusFilePath}
        threadId={pane.thread?.id ?? ''}
        workspacePath={paneWorkspacePath(pane)}
        {viewMode}
        {wordWrap}
        {expandedFiles}
        initialScrollTop={scrollTop}
        onToggleFile={toggleFile}
        onExpandAll={expandAllFiles}
        onCollapseAll={collapseAllFiles}
        onScroll={recordScroll}
      />
    {/key}
    {#if expansion.hasMore}
      <div class="border-t border-border-subtle px-3 py-2 shrink-0">
        <button
          type="button"
          onclick={() => void expansion.showFull()}
          data-testid="diff-sidebar-load-full"
          class="text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
        >
          Load full diff ({formatPayloadSize(expansion.totalSize)}) ↓
        </button>
      </div>
    {:else if !expansion.isComplete}
      <div class="border-t border-border-subtle px-3 py-2 text-[11px] text-text-secondary shrink-0" data-testid="diff-sidebar-truncated">
        Truncated — content beyond the loaded window is not shown.
      </div>
    {/if}
  {/if}

</section>
