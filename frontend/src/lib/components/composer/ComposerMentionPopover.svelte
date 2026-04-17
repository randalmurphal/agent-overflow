<script lang="ts">
  import type { WorkspaceFile } from '../../types/workspaceFile';

  interface Props {
    open: boolean;
    query: string;
    results: WorkspaceFile[];
    activeIndex: number;
    loading?: boolean;
    onSelect: (file: WorkspaceFile) => void;
    onHover?: (index: number) => void;
  }

  let {
    open,
    query,
    results,
    activeIndex,
    loading = false,
    onSelect,
    onHover,
  }: Props = $props();
</script>

{#if open}
  <div
    class="absolute left-4 bottom-full mb-2 z-20 w-[22rem] max-h-72 overflow-y-auto rounded-lg border border-border bg-surface-0 shadow-lg"
    role="listbox"
    aria-label="Workspace file mentions"
    data-testid="mention-popover"
  >
    <div class="border-b border-border px-3 py-2 text-xs text-text-secondary">
      {#if loading}
        Searching...
      {:else if query}
        Files matching "{query}" · {results.length} result{results.length === 1 ? '' : 's'}
      {:else}
        Start typing to filter workspace files
      {/if}
    </div>

    {#if results.length === 0 && !loading}
      <div class="px-3 py-3 text-xs text-text-secondary">No matches. Escape to close.</div>
    {:else}
      <ul class="py-1">
        {#each results as file, index (file.path)}
          {@const active = index === activeIndex}
          <li>
            <button
              type="button"
              role="option"
              aria-selected={active}
              class="flex w-full items-center justify-between gap-3 px-3 py-1.5 text-left text-sm hover:bg-surface-1 focus-visible:outline-none"
              class:bg-accent={active}
              class:text-surface-0={active}
              data-testid="mention-option"
              onclick={() => onSelect(file)}
              onmouseenter={() => onHover?.(index)}
            >
              <span class="truncate" title={file.path}>{file.path}</span>
              <span class="shrink-0 text-xs opacity-70">
                {file.kind === 'directory' ? 'dir' : 'file'}
              </span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
{/if}
