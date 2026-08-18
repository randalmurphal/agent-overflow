<script lang="ts">
  // Composer command completion popover, anchored to the textarea.
  //
  // Same shell as the @-mention popover (the Popover primitive portals to
  // document.body so it escapes the composer card's `overflow-hidden` clip),
  // and deliberately a listbox rather than the Menu primitive: Menu takes DOM
  // focus for its roving tabindex, and this menu is driven from the textarea
  // the user is still typing into.
  //
  // Rows arrive already grouped and already filtered; the flat index the
  // keyboard walks is reconstructed here by counting rows across sections, so
  // the section headers cost nothing in the navigation model.

  import type {
    ComposerCommandEntry,
    ComposerCommandSection,
  } from './composerCommandEntries';
  import { createActiveOptionReveal } from './popoverOptionReveal';
  import Popover from '../primitives/Popover.svelte';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';

  interface Props {
    anchor?: HTMLElement | undefined;
    open: boolean;
    sections: ComposerCommandSection[];
    activeIndex: number;
    onSelect: (entry: ComposerCommandEntry) => void;
    onHover?: (index: number) => void;
    onClose?: (reason?: PopoverCloseReason) => void;
  }

  let {
    anchor,
    open,
    sections,
    activeIndex,
    onSelect,
    onHover,
    onClose = () => {},
  }: Props = $props();

  // Row offset of each section's first entry in the flat list.
  let sectionOffsets = $derived.by(() => {
    const offsets: number[] = [];
    let cursor = 0;
    for (const section of sections) {
      offsets.push(cursor);
      cursor += section.entries.length;
    }
    return offsets;
  });

  let listEl = $state<HTMLElement | undefined>();
  const reveal = createActiveOptionReveal();
  $effect(() => {
    // `sections` is a dependency on purpose: filtering can leave the index
    // unchanged while a different row becomes the active one.
    void sections;
    reveal.sync(activeIndex, listEl);
  });
</script>

<Popover {anchor} {open} {onClose} placement="top-start" role="listbox" ariaLabel="Composer Commands">
  {#snippet children()}
    <div
      bind:this={listEl}
      class="w-[26rem] max-h-80 overflow-y-auto rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 shadow-menu"
      data-testid="slash-popover"
    >
      {#each sections as section, sectionIndex (section.id)}
        <div
          class="px-3 pt-2 pb-1 text-[0.625rem] font-medium uppercase tracking-wide text-fg-subtle"
          data-testid="slash-section-header"
          data-section={section.id}
        >
          {section.header}
        </div>
        <ul class="pb-1">
          {#each section.entries as entry, entryIndex (entry.label)}
            {@const index = sectionOffsets[sectionIndex] + entryIndex}
            {@const active = index === activeIndex}
            <li>
              <button
                type="button"
                role="option"
                aria-selected={active}
                aria-disabled={entry.disabled === true}
                disabled={entry.disabled === true}
                class="flex w-full items-baseline gap-2 px-3 py-1.5 text-left focus-visible:outline-none transition-colors disabled:cursor-not-allowed disabled:opacity-50"
                class:bg-accent={active}
                class:text-surface-0={active}
                data-testid="slash-option"
                data-command={entry.name}
                data-kind={entry.kind}
                data-disabled={entry.disabled === true}
                title={entry.disabledReason}
                onclick={() => onSelect(entry)}
                onmousemove={() => {
                  reveal.hovered(index);
                  onHover?.(index);
                }}
              >
                <span class="shrink-0 text-[0.8125rem] font-medium">{entry.label}</span>
                {#if entry.argumentHint}
                  <span class="shrink-0 text-[0.6875rem] {active ? 'opacity-80' : 'text-fg-hint'}">
                    {entry.argumentHint}
                  </span>
                {/if}
                {#if entry.description}
                  <span class="truncate text-[0.6875rem] {active ? '' : 'text-fg-subtle'}">
                    {entry.description}
                  </span>
                {/if}
              </button>
            </li>
          {/each}
        </ul>
      {/each}
    </div>
  {/snippet}
</Popover>
