<script lang="ts">
  import type { Command } from '../../stores/commandRegistry.svelte';

  let {
    command,
    shortcut,
    selected,
    matchIndices,
    onMouseEnter,
    onClick,
  }: {
    command: Command;
    shortcut: string | null;
    selected: boolean;
    matchIndices: number[];
    onMouseEnter: () => void;
    onClick: () => void;
  } = $props();

  // Build a highlighted label where matched characters are wrapped in <mark>.
  let labelSegments = $derived.by(() => {
    const label = command.label;
    if (matchIndices.length === 0) return [{ text: label, match: false }];
    const segs: { text: string; match: boolean }[] = [];
    let i = 0;
    for (let k = 0; k < matchIndices.length; k += 1) {
      const m = matchIndices[k];
      if (m >= label.length) break;
      if (m > i) segs.push({ text: label.slice(i, m), match: false });
      // Collapse consecutive runs into one <mark> for cleaner DOM.
      let end = m + 1;
      while (k + 1 < matchIndices.length && matchIndices[k + 1] === end && end < label.length) {
        end += 1;
        k += 1;
      }
      segs.push({ text: label.slice(m, end), match: true });
      i = end;
    }
    if (i < label.length) segs.push({ text: label.slice(i), match: false });
    return segs;
  });
</script>

<button
  type="button"
  role="option"
  aria-selected={selected}
  id="palette-option-{command.id}"
  onmouseenter={onMouseEnter}
  onclick={onClick}
  class="w-full flex items-center gap-3 px-4 py-2 text-left text-[0.8125rem] cursor-pointer transition-colors focus:outline-none
    {selected ? 'bg-accent/10 text-fg' : 'text-fg-muted hover:bg-surface-2/30 hover:text-fg'}"
>
  {#if command.icon}
    <span class="text-base leading-none w-4 text-center shrink-0 {selected ? 'text-accent' : 'text-fg-subtle'}">{command.icon}</span>
  {:else}
    <span class="w-4 shrink-0" aria-hidden="true"></span>
  {/if}
  <span class="flex-1 truncate">
    {#each labelSegments as seg}
      {#if seg.match}
        <mark class="bg-transparent font-semibold text-accent">{seg.text}</mark>
      {:else}
        {seg.text}
      {/if}
    {/each}
  </span>
  {#if shortcut}
    <kbd class="ml-2 shrink-0 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/60 px-1.5 py-0.5 font-mono text-[0.625rem] uppercase tracking-[0.08em] text-fg-subtle">{shortcut}</kbd>
  {/if}
</button>
