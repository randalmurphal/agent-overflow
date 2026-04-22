<script lang="ts">
  import { slide } from 'svelte/transition';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import type { DiffMeta, Item } from '../../types/models';
  import { parseDiffLines, type DiffLine } from '../../utils/diff';
  import { getSettings } from '../../stores/settings.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';

  let { item, meta, payloadId }: { item?: Item; meta: DiffMeta; payloadId: string } = $props();

  const expansion = createPayloadExpansion(() => payloadId);

  $effect(() => {
    payloadId;
    expansion.reset();
  });

  let previewLines = $derived(parseDiffLines(meta.preview));
  let displayLines = $derived.by<DiffLine[]>(() => {
    const text = expansion.displayData;
    if (text !== null) return parseDiffLines(text);
    return previewLines;
  });

  let wrapClass = $derived(getSettings().diffWordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre');

  let badgeClasses = $derived.by(() => {
    switch (meta.changeKind) {
      case 'added': return 'bg-success/20 text-success';
      case 'modified': return 'bg-warning/20 text-warning';
      case 'deleted': return 'bg-error/20 text-error';
      case 'renamed': return 'bg-accent/30 text-accent';
    }
  });
</script>

<div class="mb-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-card/25 overflow-hidden">
  <!-- Header -->
  <button
    class="w-full px-2.5 py-1.5 flex items-center gap-2 text-[13px] cursor-pointer hover:bg-surface-2/25 transition-colors"
    onclick={() => expansion.toggle()}
    aria-expanded={expansion.expanded}
    aria-controls="diff-content-{payloadId}"
    aria-label="Toggle diff: {meta.filePath}"
  >
    <span
      class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150"
      class:rotate-90={expansion.expanded}
      aria-hidden="true"
    >
      <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
    </span>
    <span class="font-mono text-[12px] text-fg-muted truncate">{meta.filePath}</span>
    <span class="px-1.5 py-0.5 rounded-[var(--radius-field)] text-[10px] font-medium {badgeClasses}">{meta.changeKind}</span>
    <ToolDecisionChip decision={item?.decision} />
    <span class="ml-auto flex gap-2 text-[11px] shrink-0 tabular-nums">
      {#if meta.insertions > 0}
        <span class="text-success">+{meta.insertions}</span>
      {/if}
      {#if meta.deletions > 0}
        <span class="text-error">-{meta.deletions}</span>
      {/if}
    </span>
  </button>

  <!-- Diff content -->
  {#if expansion.expanded}
    <div id="diff-content-{payloadId}" transition:slide={{ duration: 150 }} class="border-t border-border-subtle bg-surface-0/50 px-3 py-2 overflow-x-auto">
      {#if expansion.loading}
        <p class="text-xs text-text-secondary" role="status" aria-live="polite">Loading full diff…</p>
      {:else if expansion.error}
        <p class="text-xs text-error" role="alert">Failed to load diff: {expansion.error}</p>
      {:else}
        <pre class="font-mono text-xs leading-tight {wrapClass}">{#each displayLines as line}<span
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
            data-testid="diff-preview-show-full"
          >
            Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
          </button>
        {/if}
      {/if}
    </div>
  {/if}
</div>
