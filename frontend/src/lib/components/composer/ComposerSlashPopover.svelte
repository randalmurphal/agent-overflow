<script lang="ts">
  interface Props {
    open: boolean;
    query: string;
    commands: string[];
    activeIndex: number;
    onSelect: (command: string) => void;
    onHover?: (index: number) => void;
  }

  let {
    open,
    query,
    commands,
    activeIndex,
    onSelect,
    onHover,
  }: Props = $props();
</script>

{#if open}
  <div
    class="absolute left-4 bottom-full mb-2 z-20 w-[22rem] max-h-72 overflow-y-auto rounded-lg border border-border bg-surface-0 shadow-lg"
    role="listbox"
    aria-label="Slash commands"
    data-testid="slash-popover"
  >
    <div class="border-b border-border px-3 py-2 text-xs text-text-secondary">
      {#if query}
        Commands matching "/{query}" · {commands.length} result{commands.length === 1 ? '' : 's'}
      {:else}
        Start typing to filter slash commands
      {/if}
    </div>

    {#if commands.length === 0}
      <div class="px-3 py-3 text-xs text-text-secondary">
        No commands available. Escape to close.
      </div>
    {:else}
      <ul class="py-1">
        {#each commands as command, index (command)}
          {@const active = index === activeIndex}
          <li>
            <button
              type="button"
              role="option"
              aria-selected={active}
              class="flex w-full items-center gap-3 px-3 py-1.5 text-left text-sm hover:bg-surface-1 focus-visible:outline-none"
              class:bg-accent={active}
              class:text-surface-0={active}
              data-testid="slash-option"
              onclick={() => onSelect(command)}
              onmouseenter={() => onHover?.(index)}
            >
              <span class="truncate" title={`/${command}`}>/{command}</span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </div>
{/if}
