<script lang="ts">
  import { untrack } from 'svelte';
  import type { PatchFile } from '../../utils/patchFiles';
  interface Props { files: readonly PatchFile[]; expandFirst?: boolean }
  let { files, expandFirst = false }: Props = $props();
  let expanded = $state(new Set<string>());
  $effect(() => {
    const first = files[0];
    if (!first) return;
    const next = new Set(untrack(() => expanded));
    if (expandFirst) next.add(first.path); else next.delete(first.path);
    expanded = next;
  });
  function toggle(path: string): void {
    const next = new Set(expanded);
    if (next.has(path)) next.delete(path); else next.add(path);
    expanded = next;
  }
</script>

{#if files.length > 0}
  <section class="space-y-1.5" data-testid="wf-diff">
    <h3 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Changes</h3>
    {#each files as file, index (file.path)}
      <div class="overflow-hidden rounded-md border border-border-subtle" data-testid="wf-diff-file">
        <button class="flex w-full items-center gap-2 px-2.5 py-2 text-left text-xs hover:bg-surface-2" onclick={() => toggle(file.path)} data-testid="wf-diff-file-toggle">
          <span class="text-fg-muted">{expanded.has(file.path) ? '▼' : '▶'}</span>
          <span class="min-w-0 flex-1 truncate font-mono">{file.path}</span>
          <span class="text-success">+{file.additions}</span><span class="text-error">−{file.deletions}</span>
        </button>
        {#if expanded.has(file.path)}
          <pre class="max-h-72 overflow-auto border-t border-border-subtle bg-surface-0 p-2 text-[11px] leading-5" data-testid="wf-diff-hunks">{file.lines.map((line) => line.content).join('\n')}</pre>
        {/if}
      </div>
    {/each}
  </section>
{/if}
