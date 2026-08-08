<script lang="ts" generics="T extends string | number">
  // Segmented: a compact button-group toggle. Used by the context-window
  // picker (200k / 1m, 272k / 1m) and any other "pick one of N" toggle
  // where a select dropdown would feel heavy.
  //
  // Single-source — keeps the active-state styling consistent across
  // the app instead of every caller re-styling the row.

  let {
    options,
    value,
    onChange,
    ariaLabel,
    size = 'sm',
    disabled = false,
  }: {
    options: Array<{ value: T; label: string }>;
    value: T;
    onChange: (next: T) => void;
    ariaLabel?: string;
    size?: 'sm' | 'md';
    /**
     * Freeze the whole group. Set on every segment (rather than dimming the
     * container) so the buttons leave the tab order and refuse activation —
     * a `pointer-events-none` wrapper would still be keyboard-reachable.
     */
    disabled?: boolean;
  } = $props();

  const sizeClass = $derived(
    size === 'md'
      ? 'text-[0.78125rem] px-3 py-1.5'
      : 'text-[0.75rem] px-2.5 py-1',
  );
</script>

<div
  role="radiogroup"
  aria-label={ariaLabel}
  class="inline-flex rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 p-0.5"
>
  {#each options as option (option.value)}
    {@const active = option.value === value}
    <button
      type="button"
      role="radio"
      aria-checked={active}
      {disabled}
      onclick={() => onChange(option.value)}
      class="rounded-[calc(var(--radius-field)-2px)] cursor-pointer transition-colors {sizeClass}
        {active
          ? 'bg-accent/15 text-fg shadow-[var(--shadow-sheet)]'
          : 'text-fg-muted hover:text-fg'}
        disabled:cursor-not-allowed disabled:opacity-50 disabled:hover:text-fg-muted
        focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    >
      {option.label}
    </button>
  {/each}
</div>
