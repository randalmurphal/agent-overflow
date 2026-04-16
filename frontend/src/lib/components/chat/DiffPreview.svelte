<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { DiffMeta } from '../../types/models';
  import { GetPayloadData } from '../../stores/bindings';
  import { parseDiffLines, type DiffLine } from '../../utils/diff';
  import { getSettings } from '../../stores/settings.svelte';

  let { meta, payloadId }: { meta: DiffMeta; payloadId: string } = $props();

  let expanded = $state(false);
  let loading = $state(false);
  let fullLines = $state<DiffLine[] | null>(null);
  let loadError = $state<string | null>(null);

  let previewLines = $derived(parseDiffLines(meta.preview));

  async function toggle() {
    if (expanded) {
      expanded = false;
      return;
    }

    expanded = true;

    if (fullLines !== null) return;

    loading = true;
    loadError = null;
    try {
      const text = await GetPayloadData(payloadId);
      fullLines = parseDiffLines(text);
    } catch (err) {
      loadError = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }

  let displayLines = $derived(expanded && fullLines ? fullLines : previewLines);

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

<div class="bg-surface-1 rounded border border-border overflow-hidden mb-2">
  <!-- Header -->
  <button
    class="w-full px-3 py-2 flex items-center gap-2 text-sm cursor-pointer hover:bg-surface-2/40"
    onclick={toggle}
  >
    <span class="text-xs text-text-secondary select-none">{expanded ? '▼' : '▶'}</span>
    <span class="font-mono text-xs text-text-primary truncate">{meta.filePath}</span>
    <span class="px-1.5 py-0.5 rounded-full text-xs {badgeClasses}">{meta.changeKind}</span>
    <span class="ml-auto flex gap-2 text-xs shrink-0">
      {#if meta.insertions > 0}
        <span class="text-success">+{meta.insertions}</span>
      {/if}
      {#if meta.deletions > 0}
        <span class="text-error">-{meta.deletions}</span>
      {/if}
    </span>
  </button>

  <!-- Diff content -->
  {#if displayLines.length > 0}
    <div transition:slide={{ duration: 150 }} class="border-t border-border bg-surface-0 px-3 py-2 overflow-x-auto">
      {#if loading}
        <p class="text-xs text-text-secondary">Loading full diff…</p>
      {:else if loadError}
        <p class="text-xs text-error">Failed to load diff: {loadError}</p>
      {/if}

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
    </div>
  {/if}
</div>
