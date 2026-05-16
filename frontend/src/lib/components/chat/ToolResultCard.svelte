<script lang="ts">
  import { untrack } from 'svelte';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import { getSettings } from '../../stores/settings.svelte';
  import type { Item, ToolInlineDiffFile, ToolResultMeta } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { parseDiffLines, type DiffLine } from '../../utils/diff';
  import { lineTintClass } from '../../utils/diffLineTint';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import CompletionBadge from './CompletionBadge.svelte';
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
  const wrapClass = $derived(getSettings().diffWordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre');
  // Re-parse payloadMeta inside the helper rather than reusing the
  // `meta: ToolResultMeta` prop: ToolResultMeta does not declare
  // `is_error` or `exit_code`, so the typed view is an incomplete
  // signal source. payloadMeta is the canonical record.
  const completionStatus = $derived(deriveCompletionStatus(item));
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

<div class="mb-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-card/25">
  <div class="flex items-start gap-2.5 px-2.5 py-2">
    <span class="font-mono text-[10px] text-fg-subtle mt-0.5">[F]</span>
    <div class="min-w-0 flex-1">
      <div class="flex items-center gap-2">
        <p class="truncate text-[13px] font-medium text-fg">{meta.title || item.summary}</p>
        <ToolDecisionChip decision={item.decision} />
        {#if completionStatus !== null}
          <CompletionBadge status={completionStatus} class="ml-auto opacity-80" />
        {/if}
      </div>
      {#if detailText}
        <div class="mt-1">
          <LazyContentBlock {pane} payloadId={undefined} preview={detailText} />
        </div>
      {/if}
      {#if hasInlineDiff}
        <div class="mt-2 flex flex-wrap gap-2" data-testid="tool-result-inline-diffs">
          {#each meta.inlineDiff?.files ?? [] as file (file.path)}
            <!--
              Each chip is a span with a sibling EditorLink. The chip
              itself isn't a clickable target (the parent card has its
              own toggle below), so the EditorLink doesn't need
              stopPropagation here — but we keep the icon visible at
              rest because the chip otherwise has no affordance for
              opening the file.
            -->
            <span class="group/chip inline-flex items-center gap-2 rounded-full px-2 py-1 text-[11px] {kindClasses(file)}">
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
        </div>
      {/if}
    </div>
  </div>

  {#if hasExactPatch}
    <div class="group/patch border-t border-border">
      <TranscriptDisclosureHeader
        expanded={expansion.expanded}
        expandable={canExpandExactPatch}
        controls={canExpandExactPatch ? `tool-result-patch-${item.id}` : undefined}
        ariaLabel="Toggle Exact Patch"
        testId="tool-result-patch-toggle"
        class="px-3 py-2 text-xs text-text-secondary {canExpandExactPatch ? 'hover:bg-surface-2/40' : ''}"
        onToggle={() => expansion.toggle()}
      >
        <span>Exact patch</span>
        {#if meta.inlineDiff?.insertions || meta.inlineDiff?.deletions}
          <span class="ml-auto">
            {#if meta.inlineDiff?.insertions}<span class="text-success">+{meta.inlineDiff.insertions}</span>{/if}
            {#if meta.inlineDiff?.insertions && meta.inlineDiff?.deletions}<span> </span>{/if}
            {#if meta.inlineDiff?.deletions}<span class="text-error">-{meta.inlineDiff.deletions}</span>{/if}
          </span>
        {/if}
        {#snippet actions()}
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
        <div id="tool-result-patch-{item.id}" class="border-t border-border bg-surface-0">
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
                  onclick={() => expansion.showFull()}
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
