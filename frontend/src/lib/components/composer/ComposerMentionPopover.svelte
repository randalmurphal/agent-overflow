<script lang="ts">
  // Mention-search popover anchored to the textarea. Uses the Popover
  // primitive (which portals to document.body) so it escapes the
  // composer card's `backdrop-blur-sm overflow-hidden` containing-block
  // trap — see Popover.svelte for the CSS-spec explanation.

  import type { WorkspaceFile } from '../../types/workspaceFile';
  import Popover from '../primitives/Popover.svelte';

  interface Props {
    anchor?: HTMLElement | undefined;
    open: boolean;
    query: string;
    results: WorkspaceFile[];
    activeIndex: number;
    loading?: boolean;
    onSelect: (file: WorkspaceFile) => void;
    onHover?: (index: number) => void;
    onClose?: () => void;
  }

  let {
    anchor,
    open,
    query,
    results,
    activeIndex,
    loading = false,
    onSelect,
    onHover,
    onClose = () => {},
  }: Props = $props();
</script>

<Popover {anchor} {open} {onClose} placement="top-start" role="listbox" ariaLabel="Workspace File Mentions">
  {#snippet children()}
    <div
      class="w-[22rem] max-h-72 overflow-y-auto rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 shadow-menu"
      data-testid="mention-popover"
    >
      <div class="border-b border-border-subtle px-3 py-1.5 text-[11px] text-fg-subtle">
        {#if loading}
          Searching…
        {:else if query}
          Files matching "{query}" · {results.length} result{results.length === 1 ? '' : 's'}
        {:else}
          Start typing to filter workspace files
        {/if}
      </div>

      {#if results.length === 0 && !loading}
        <div class="px-3 py-3 text-[12px] text-fg-subtle">No matches. Escape to close.</div>
      {:else}
        <ul class="py-1">
          {#each results as file, index (file.path)}
            {@const active = index === activeIndex}
            <li>
              <button
                type="button"
                role="option"
                aria-selected={active}
                class="flex w-full items-center justify-between gap-3 px-3 py-1.5 text-left text-[13px] hover:bg-surface-2/40 focus-visible:outline-none transition-colors"
                class:bg-accent={active}
                class:text-surface-0={active}
                data-testid="mention-option"
                onclick={() => onSelect(file)}
                onmouseenter={() => onHover?.(index)}
              >
                <span class="truncate" title={file.path}>{file.path}</span>
                <span class="text-[10px] text-fg-hint shrink-0">
                  {file.kind === 'directory' ? 'dir' : 'file'}
                </span>
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/snippet}
</Popover>
