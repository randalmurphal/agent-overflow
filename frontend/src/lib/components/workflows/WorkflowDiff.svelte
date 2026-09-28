<script lang="ts">
  import { untrack } from 'svelte';
  import WorkflowDiffLines from './WorkflowDiffLines.svelte';
  import type { ReviewFile } from '../../utils/patchStore';
  interface Props {
    files: readonly ReviewFile[];
    expandFirst?: boolean;
  }
  let { files, expandFirst = false }: Props = $props();
  let expanded = $state(new Set<string>());
  $effect(() => {
    const first = files[0];
    const shouldExpand = expandFirst;
    if (!first) return;
    untrack(() => {
      if (shouldExpand) expand(first.path);
      else collapse(first.path);
    });
  });

  function collapse(path: string): void {
    if (!expanded.has(path)) return;
    const next = new Set(expanded);
    next.delete(path);
    expanded = next;
  }

  function expand(path: string): void {
    if (!expanded.has(path)) expanded = new Set(expanded).add(path);
  }

  function toggle(file: ReviewFile): void {
    if (expanded.has(file.path)) collapse(file.path);
    else expand(file.path);
  }
</script>

{#if files.length > 0}
  <section class="space-y-1.5" data-testid="wf-diff">
    <h3 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Changes</h3>
    {#each files as file (file.path)}
      <div class="overflow-hidden rounded-md border border-border-subtle" data-testid="wf-diff-file">
        <button class="flex w-full items-center gap-2 px-2.5 py-2 text-left text-xs hover:bg-surface-2" onclick={() => toggle(file)} data-testid="wf-diff-file-toggle">
          <span class="text-fg-muted">{expanded.has(file.path) ? '▼' : '▶'}</span>
          <span class="min-w-0 flex-1 truncate font-mono">{file.path}</span>
          <span class="text-success">+{file.additions}</span><span class="text-error">−{file.deletions}</span>
        </button>
        {#if expanded.has(file.path)}
          <WorkflowDiffLines {file} />
        {/if}
      </div>
    {/each}
  </section>
{/if}
