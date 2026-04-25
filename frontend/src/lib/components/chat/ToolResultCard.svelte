<script lang="ts">
  import { slide } from 'svelte/transition';
  import { getSettings } from '../../stores/settings.svelte';
  import type { Item, ToolInlineDiffFile, ToolResultMeta } from '../../types/models';
  import { parseDiffLines, type DiffLine } from '../../utils/diff';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import CompletionBadge from './CompletionBadge.svelte';
  import LazyContentBlock from './LazyContentBlock.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import EditorLink from '../common/EditorLink.svelte';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';

  let { item, meta, payloadId }: { item: Item; meta: ToolResultMeta; payloadId?: string } = $props();

  // detail/preview are unbounded provider text; LazyContentBlock caps
  // display length. The stored payload is the diff (Exact patch toggle),
  // so detailText doesn't get a payloadId — it's truncate-only.
  const detailText = $derived(meta.detail || meta.preview || '');

  const expansion = createPayloadExpansion(() => payloadId, () => item.threadId);

  const hasInlineDiff = $derived(Boolean(meta.inlineDiff && meta.inlineDiff.files.length > 0));
  const hasExactPatch = $derived(meta.inlineDiff?.availability === 'exact_patch' && Boolean(payloadId));
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

  $effect(() => {
    item.threadId;
    payloadId;
    expansion.reset();
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
          <LazyContentBlock payloadId={undefined} preview={detailText} />
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
            <span class="inline-flex items-center gap-2 rounded-full px-2 py-1 text-[11px] {kindClasses(file)}">
              <span class="font-mono">{file.path}</span>
              {#if file.insertions || file.deletions}
                <span class="text-text-secondary">{fileStats(file)}</span>
              {/if}
              <EditorLink
                path={file.path}
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
    <div class="border-t border-border">
      <button
        class="flex w-full items-center gap-2 px-3 py-2 text-left text-xs text-text-secondary hover:bg-surface-2/40"
        onclick={() => expansion.toggle()}
        aria-expanded={expansion.expanded}
        aria-controls="tool-result-patch-{item.id}"
      >
        <span>{expansion.expanded ? '▼' : '▶'}</span>
        <span>Exact patch</span>
        {#if meta.inlineDiff?.insertions || meta.inlineDiff?.deletions}
          <span class="ml-auto">
            {#if meta.inlineDiff?.insertions}<span class="text-success">+{meta.inlineDiff.insertions}</span>{/if}
            {#if meta.inlineDiff?.insertions && meta.inlineDiff?.deletions}<span> </span>{/if}
            {#if meta.inlineDiff?.deletions}<span class="text-error">-{meta.inlineDiff.deletions}</span>{/if}
          </span>
        {/if}
      </button>

      {#if expansion.expanded}
        <div id="tool-result-patch-{item.id}" transition:slide={{ duration: 150 }} class="overflow-x-auto border-t border-border bg-surface-0 px-3 py-2">
          {#if expansion.loading}
            <p class="text-xs text-text-secondary" role="status" aria-live="polite">Loading patch…</p>
          {:else if expansion.error}
            <p class="text-xs text-error" role="alert">Failed to load patch: {expansion.error}</p>
          {:else if patchLines}
            <pre class="font-mono text-xs leading-tight {wrapClass}">{#each patchLines as line}<span
                class={line.type === 'added'
                  ? 'bg-success/10 text-success'
                  : line.type === 'removed'
                    ? 'bg-error/10 text-error'
                    : line.type === 'header'
                      ? 'text-accent/70'
                      : 'text-text-secondary'}
              >{line.content}
</span>{/each}</pre>
            {#if expansion.hasMore}
              <button
                type="button"
                class="mt-2 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
                onclick={() => expansion.showFull()}
                data-testid="tool-result-patch-show-full"
              >
                Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
              </button>
            {/if}
          {/if}
        </div>
      {/if}
    </div>
  {/if}
</div>
