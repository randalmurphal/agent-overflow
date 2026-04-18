<script lang="ts">
  import { slide } from 'svelte/transition';
  import { GetPayloadData } from '../../stores/bindings';
  import { getSettings } from '../../stores/settings.svelte';
  import type { Item, ToolInlineDiffFile, ToolResultMeta } from '../../types/models';
  import { parseDiffLines, type DiffLine } from '../../utils/diff';
  import LazyContentBlock from './LazyContentBlock.svelte';

  let { item, meta, payloadId }: { item: Item; meta: ToolResultMeta; payloadId?: string } = $props();

  // detail/preview are unbounded provider text; LazyContentBlock caps
  // display length. The stored payload is the diff (Exact patch toggle),
  // so detailText doesn't get a payloadId — it's truncate-only.
  const detailText = $derived(meta.detail || meta.preview || '');

  let expanded = $state(false);
  let loading = $state(false);
  let loadError = $state<string | null>(null);
  let fullLines = $state<DiffLine[] | null>(null);

  const hasInlineDiff = $derived(Boolean(meta.inlineDiff && meta.inlineDiff.files.length > 0));
  const hasExactPatch = $derived(meta.inlineDiff?.availability === 'exact_patch' && Boolean(payloadId));
  const wrapClass = $derived(getSettings().diffWordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre');

  async function togglePatch() {
    if (!hasExactPatch) return;
    if (expanded) {
      expanded = false;
      return;
    }

    expanded = true;
    if (fullLines !== null || !payloadId) return;

    loading = true;
    loadError = null;
    try {
      fullLines = parseDiffLines(await GetPayloadData(payloadId));
    } catch (err) {
      loadError = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }

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

<div class="mb-3 rounded border border-border bg-surface-1">
  <div class="flex items-start gap-3 px-3 py-2.5">
    <span class="font-mono text-xs text-text-secondary">[F]</span>
    <div class="min-w-0 flex-1">
      <div class="flex items-center gap-2">
        <p class="truncate text-sm font-medium text-text-primary">{meta.title || item.summary}</p>
        <span class="ml-auto text-xs text-success">done</span>
      </div>
      {#if detailText}
        <div class="mt-1">
          <LazyContentBlock payloadId={undefined} preview={detailText} />
        </div>
      {/if}
      {#if hasInlineDiff}
        <div class="mt-2 flex flex-wrap gap-2">
          {#each meta.inlineDiff?.files ?? [] as file (file.path)}
            <span class="inline-flex items-center gap-2 rounded-full px-2 py-1 text-[11px] {kindClasses(file)}">
              <span class="font-mono">{file.path}</span>
              {#if file.insertions || file.deletions}
                <span class="text-text-secondary">{fileStats(file)}</span>
              {/if}
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
        onclick={togglePatch}
        aria-expanded={expanded}
        aria-controls="tool-result-patch-{item.id}"
      >
        <span>{expanded ? '▼' : '▶'}</span>
        <span>Exact patch</span>
        {#if meta.inlineDiff?.insertions || meta.inlineDiff?.deletions}
          <span class="ml-auto">
            {#if meta.inlineDiff?.insertions}<span class="text-success">+{meta.inlineDiff.insertions}</span>{/if}
            {#if meta.inlineDiff?.insertions && meta.inlineDiff?.deletions}<span> </span>{/if}
            {#if meta.inlineDiff?.deletions}<span class="text-error">-{meta.inlineDiff.deletions}</span>{/if}
          </span>
        {/if}
      </button>

      {#if expanded}
        <div id="tool-result-patch-{item.id}" transition:slide={{ duration: 150 }} class="overflow-x-auto border-t border-border bg-surface-0 px-3 py-2">
          {#if loading}
            <p class="text-xs text-text-secondary" role="status" aria-live="polite">Loading patch…</p>
          {:else if loadError}
            <p class="text-xs text-error" role="alert">Failed to load patch: {loadError}</p>
          {:else if fullLines}
            <pre class="font-mono text-xs leading-tight {wrapClass}">{#each fullLines as line}<span
                class={line.type === 'added'
                  ? 'bg-success/10 text-success'
                  : line.type === 'removed'
                    ? 'bg-error/10 text-error'
                    : line.type === 'header'
                      ? 'text-accent/70'
                      : 'text-text-secondary'}
              >{line.content}
</span>{/each}</pre>
          {/if}
        </div>
      {/if}
    </div>
  {/if}
</div>
