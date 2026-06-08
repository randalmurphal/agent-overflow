<script lang="ts">
  import { untrack } from 'svelte';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import { getSettings } from '../../stores/settings.svelte';
  import type { Item, ToolInlineDiffFile, ToolResultMeta } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { parseDiffLines, type DiffLine } from '../../utils/diff';
  import { lineTintClass } from '../../utils/diffLineTint';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import CopyFooter from './CopyFooter.svelte';
  import Icon from '../primitives/Icon.svelte';
  import LazyContentBlock from './LazyContentBlock.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import EditorLink from '../common/EditorLink.svelte';
  import {
    createPayloadExpansion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import { openDiffSidebar } from './diffSidebarTrigger';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import Indicator from './Indicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorForStatus } from './rowState';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import {
    inlineDiffOmittedFiles,
    inlineDiffPreviewFiles,
  } from '../../utils/inlineThreshold';

  let { pane, item, meta, payloadId }: { pane?: ThreadPane; item: Item; meta: ToolResultMeta; payloadId?: string } = $props();

  function openSidebarForFile(filePath: string) {
    if (!pane || !payloadId) return;
    openDiffSidebar(pane, { payloadId, filePath });
  }

  function openSidebarForPatch() {
    if (!pane || !payloadId) return;
    openDiffSidebar(pane, { payloadId });
  }

  let canOpenSidebar = $derived(pane !== undefined && payloadId !== undefined);

  // detail/preview are unbounded provider text; LazyContentBlock caps
  // display length. The stored payload is the diff (Exact patch toggle),
  // so detailText doesn't get a payloadId — it's truncate-only.
  const detailText = $derived(meta.detail || meta.preview || '');

  // pane is stable across a row's lifetime; read once via `untrack`.
  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => payloadId,
          () => item.threadId,
          { payloadVersion: () => item.updatedAt },
        ),
  );
  const expansion = $derived(pane ? pane.expansionStateFor(item) : localFallback!);
  keepExpandedPayloadFresh(
    () => expansion,
    () => Boolean(payloadId),
  );

  const hasInlineDiff = $derived(Boolean(meta.inlineDiff && meta.inlineDiff.files.length > 0));
  const hasExactPatch = $derived(meta.inlineDiff?.availability === 'exact_patch');
  const canExpandExactPatch = $derived(hasExactPatch && Boolean(payloadId));
  const inlinePreviewFiles = $derived(inlineDiffPreviewFiles(meta.inlineDiff?.files));
  const inlineTotalFiles = $derived(meta.inlineDiff?.totalFiles ?? meta.inlineDiff?.files.length ?? 0);
  const inlineOmittedFiles = $derived(
    inlineDiffOmittedFiles(
      inlineTotalFiles,
      inlinePreviewFiles.length,
      meta.inlineDiff?.omittedFiles,
    ),
  );
  const wrapClass = $derived(getSettings().diffWordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre');
  const resultMeta = $derived(meta as unknown as Record<string, unknown>);
  const completionStatus = $derived(deriveCompletionStatus(item, { meta: resultMeta }));
  const indicatorState = $derived(indicatorStateForItem(item, { meta: resultMeta }));
  const rowError = $derived.by(() => {
    if (completionStatus !== 'failure') return null;
    return rowErrorForStatus(item.status, 'Tool output failed') ?? {
      tone: 'error' as const,
      msg: 'Tool output failed',
    };
  });
  const patchLines = $derived.by<DiffLine[] | null>(() => {
    if (expansion.displayData === null) return null;
    return parseDiffLines(expansion.displayData);
  });

  function kindClasses(file: ToolInlineDiffFile): string {
    switch (file.kind) {
      case 'added':
        return 'bg-success/15 text-success';
      case 'deleted':
        return 'bg-error/15 text-error';
      case 'renamed':
        return 'bg-accent/20 text-accent';
      default:
        return 'bg-warning/15 text-warning';
    }
  }

  function fileStats(file: ToolInlineDiffFile): string {
    const parts: string[] = [];
    if (file.insertions) parts.push(`+${file.insertions}`);
    if (file.deletions) parts.push(`-${file.deletions}`);
    return parts.join(' ');
  }

  function fileLabel(file: ToolInlineDiffFile): string {
    if (!file.previousPath) return file.path;
    return `${file.previousPath} -> ${file.path}`;
  }
</script>

<div class="group/tool overflow-hidden" data-testid="tool-result-card">
  <TranscriptDisclosureHeader
    expanded={false}
    expandable={false}
    testId="tool-result-row-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 text-[0.75rem] text-fg-muted"
  >
    {#snippet icon()}<ToolKindIcon kind="generic" ariaLabel="output" />{/snippet}
    {#snippet label()}<span>output</span>{/snippet}
    {#snippet body()}
      <p class="min-w-0 truncate text-[0.75rem] text-fg-muted/75">{meta.title || item.summary}</p>
    {/snippet}
    {#snippet actions()}
      <ToolDecisionChip decision={item.decision} />
      <ToolHeaderMeta statusSlotTestId="tool-result-status-slot" class="ml-auto">
        {#snippet status()}<Indicator state={completionStatus === 'failure' ? indicatorState : null} />{/snippet}
      </ToolHeaderMeta>
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if detailText || hasInlineDiff}
    <div class="ml-[5.25rem] px-3 pb-1">
      {#if detailText}
        <div>
          <LazyContentBlock {pane} payloadId={undefined} preview={detailText} />
        </div>
      {/if}
      {#if hasInlineDiff}
        <div class="mt-2 flex flex-wrap gap-1.5" data-testid="tool-result-inline-diffs">
          {#each inlinePreviewFiles as file (file.path)}
            <span class="group/chip inline-flex items-center gap-2 rounded-[var(--radius-control)] border border-border-subtle px-2 py-1 text-[0.6875rem] {kindClasses(file)}">
              <span class="font-mono">{fileLabel(file)}</span>
              {#if file.insertions || file.deletions}
                <span class="text-text-secondary">{fileStats(file)}</span>
              {/if}
              {#if canOpenSidebar}
                <button
                  type="button"
                  onclick={(e) => { e.stopPropagation(); openSidebarForFile(file.path); }}
                  title="Open in side panel"
                  aria-label={`Open Diff in Side Panel: ${file.path}`}
                  data-testid="tool-result-chip-open-sidebar"
                  data-file-path={file.path}
                  class="opacity-70 group-hover/chip:opacity-100 hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded p-0.5"
                >
                  <Icon icon={PanelRightOpen} size={12} />
                </button>
              {/if}
              <EditorLink
                path={file.path}
                workspacePath={paneWorkspacePath(pane)}
                asIcon
                stopPropagation
                class="opacity-70 hover:opacity-100"
              />
            </span>
          {/each}
          {#if inlineOmittedFiles > 0}
            <span class="inline-flex items-center gap-2 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/35 px-2 py-1 text-[0.6875rem] text-text-secondary">
              <span>{inlineOmittedFiles} more</span>
              {#if canOpenSidebar}
                <button
                  type="button"
                  onclick={(e) => { e.stopPropagation(); openSidebarForPatch(); }}
                  title="Open full diff in side panel"
                  aria-label="Open Full Diff in Side Panel"
                  data-testid="tool-result-overflow-open-sidebar"
                  class="opacity-70 hover:opacity-100 hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded p-0.5"
                >
                  <Icon icon={PanelRightOpen} size={12} />
                </button>
              {/if}
            </span>
          {/if}
        </div>
      {/if}
    </div>
  {/if}

  {#if rowError}
    <div class="ml-[5.25rem] px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}

  {#if hasExactPatch}
    <div class="group/patch ml-[5.25rem] border-t border-border-subtle">
      <TranscriptDisclosureHeader
        expanded={expansion.expanded}
        expandable={canExpandExactPatch}
        controls={canExpandExactPatch ? `tool-result-patch-${item.id}` : undefined}
        ariaLabel="Toggle Exact Patch"
        testId="tool-result-patch-toggle"
        class="rounded-[var(--radius-control)] px-1 py-1 text-[0.75rem] text-fg-muted {canExpandExactPatch ? 'hover:bg-surface-2/20' : ''}"
        onToggle={(event) => preservePaneScrollAnchor(pane, event, () => expansion.toggle())}
      >
        {#snippet icon()}<ToolKindIcon kind="file" ariaLabel="patch" />{/snippet}
        {#snippet label()}<span>patch</span>{/snippet}
        {#snippet body()}
          <span class="min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted/75">Exact patch</span>
        {/snippet}
        {#snippet actions()}
          {#if meta.inlineDiff?.insertions || meta.inlineDiff?.deletions}
          <span class="text-[0.6875rem]">
            {#if meta.inlineDiff?.insertions}<span class="text-success">+{meta.inlineDiff.insertions}</span>{/if}
            {#if meta.inlineDiff?.insertions && meta.inlineDiff?.deletions}<span> </span>{/if}
            {#if meta.inlineDiff?.deletions}<span class="text-error">-{meta.inlineDiff.deletions}</span>{/if}
          </span>
          {/if}
          {#if canOpenSidebar}
            <button
              type="button"
              onclick={(e) => { e.stopPropagation(); openSidebarForPatch(); }}
              title="Open in side panel"
              aria-label="Open Patch in Side Panel"
              data-testid="tool-result-patch-open-sidebar"
              class="opacity-0 group-hover/patch:opacity-100 focus-visible:opacity-100 rounded p-1 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
            >
              <Icon icon={PanelRightOpen} size={14} />
            </button>
          {/if}
        {/snippet}
      </TranscriptDisclosureHeader>

      {#if canExpandExactPatch && expansion.expanded}
        <div id="tool-result-patch-{item.id}" class="ml-5 border-l border-border-subtle bg-surface-0/35">
          <div class="px-3 py-2">
            {#if expansion.loading}
              <p class="text-xs text-text-secondary" role="status" aria-live="polite">Loading patch…</p>
            {:else if expansion.error}
              <p class="text-xs text-error" role="alert">Failed to load patch: {expansion.error}</p>
            {:else if patchLines}
              <pre class="max-h-[32em] overflow-auto font-mono text-xs leading-tight {wrapClass}">{#each patchLines as line}<span
                  class={lineTintClass(line.type)}
                >{line.content}
</span>{/each}</pre>
              {#if expansion.hasMore}
                <button
                  type="button"
                  class="mt-2 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
                  onclick={(event) => preservePaneScrollAnchor(pane, event, () => expansion.showFull())}
                  data-testid="tool-result-patch-show-full"
                >
                  Show more output ({formatPayloadSize(expansion.totalSize)}) ↓
                </button>
              {/if}
            {/if}
          </div>
          {#if !expansion.loading && !expansion.error && expansion.displayData}
            <CopyFooter text={expansion.displayData} label="Copy patch" />
          {/if}
        </div>
      {/if}
    </div>
  {/if}
</div>
