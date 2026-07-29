<script lang="ts">
  // Slash-command completion popover, anchored to the textarea. Same shell as
  // the @-mention popover (the Popover primitive portals to document.body so
  // it escapes the composer card's `overflow-hidden` clip) — the two menus
  // differ only in what they list.

  import type { SlashCommand } from './slashCommands';
  import { slashCommandWord } from './slashCommands';
  import Popover from '../primitives/Popover.svelte';

  interface Props {
    anchor?: HTMLElement | undefined;
    open: boolean;
    results: SlashCommand[];
    activeIndex: number;
    onSelect: (command: SlashCommand) => void;
    onHover?: (index: number) => void;
    onClose?: () => void;
  }

  let {
    anchor,
    open,
    results,
    activeIndex,
    onSelect,
    onHover,
    onClose = () => {},
  }: Props = $props();
</script>

<Popover {anchor} {open} {onClose} placement="top-start" role="listbox" ariaLabel="Composer Commands">
  {#snippet children()}
    <div
      class="w-[22rem] max-h-72 overflow-y-auto rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 shadow-menu"
      data-testid="slash-popover"
    >
      <ul class="py-1">
        {#each results as command, index (command.name)}
          {@const active = index === activeIndex}
          <li>
            <button
              type="button"
              role="option"
              aria-selected={active}
              class="flex w-full items-baseline gap-3 px-3 py-1.5 text-left hover:bg-surface-2/40 focus-visible:outline-none transition-colors"
              class:bg-accent={active}
              class:text-surface-0={active}
              data-testid="slash-option"
              data-command={command.name}
              onclick={() => onSelect(command)}
              onmouseenter={() => onHover?.(index)}
            >
              <span class="text-[0.8125rem] font-medium">{slashCommandWord(command)}</span>
              <span class="truncate text-[0.6875rem] {active ? '' : 'text-fg-subtle'}">
                {command.description}
              </span>
            </button>
          </li>
        {/each}
      </ul>
    </div>
  {/snippet}
</Popover>
