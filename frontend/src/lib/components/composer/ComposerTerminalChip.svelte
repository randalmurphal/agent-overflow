<script lang="ts">
  import type { TerminalChip } from '../../types/draft';

  interface Props {
    chip: TerminalChip;
    expanded?: boolean;
    onRemove: (id: string) => void;
    onToggle?: (id: string) => void;
  }

  let { chip, expanded = false, onRemove, onToggle }: Props = $props();
</script>

<div
  class="rounded-md border border-border bg-surface-1 px-2 py-1 text-xs"
  data-testid="terminal-chip"
>
  <div class="flex items-center gap-2">
    <button
      type="button"
      class="flex-1 text-left truncate font-mono text-text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      title={chip.preview}
      aria-expanded={expanded}
      aria-controls={`chip-body-${chip.id}`}
      onclick={() => onToggle?.(chip.id)}
    >
      <span class="text-text-secondary">{chip.label}:</span>
      <span>{chip.preview}</span>
    </button>
    <button
      type="button"
      aria-label="Remove Terminal Context"
      class="rounded p-0.5 text-text-secondary hover:text-text-primary hover:bg-surface-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      onclick={() => onRemove(chip.id)}
    >
      <span aria-hidden="true">x</span>
    </button>
  </div>
  {#if expanded}
    <pre
      id={`chip-body-${chip.id}`}
      class="mt-1 max-h-40 overflow-auto whitespace-pre-wrap rounded bg-surface-0 p-2 font-mono text-[11px] text-text-primary"
    >{chip.content}</pre>
  {/if}
</div>
