<script lang="ts">
  // Slash-command popover anchored to the textarea. Uses the Popover
  // primitive (which portals to document.body) so it escapes the
  // composer card's `backdrop-blur-sm overflow-hidden` containing-block
  // trap — see Popover.svelte for the CSS-spec explanation.

  import Popover from '../primitives/Popover.svelte';

  interface Props {
    anchor?: HTMLElement | undefined;
    open: boolean;
    query: string;
    commands: string[];
    activeIndex: number;
    onSelect: (command: string) => void;
    onHover?: (index: number) => void;
    onClose?: () => void;
  }

  let {
    anchor,
    open,
    query,
    commands,
    activeIndex,
    onSelect,
    onHover,
    onClose = () => {},
  }: Props = $props();
</script>

<Popover {anchor} {open} {onClose} placement="top-start" role="listbox" ariaLabel="Slash commands">
  {#snippet children()}
    <div
      class="w-[22rem] max-h-72 overflow-y-auto rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 shadow-menu"
      data-testid="slash-popover"
    >
      <div class="border-b border-border-subtle px-3 py-1.5 text-[11px] text-fg-subtle">
        {#if query}
          Commands matching "/{query}" · {commands.length} result{commands.length === 1 ? '' : 's'}
        {:else}
          Start typing to filter slash commands
        {/if}
      </div>

      {#if commands.length === 0}
        <div class="px-3 py-3 text-xs text-fg-subtle">
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
                class="flex w-full items-center gap-3 px-3 py-1.5 text-left text-sm hover:bg-surface-2/40 focus-visible:outline-none transition-colors"
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
  {/snippet}
</Popover>
