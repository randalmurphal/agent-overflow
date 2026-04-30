<script lang="ts">
  import Check from 'lucide-svelte/icons/check';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    label: string;
    description: string;
    optionIndex: number;
    selected: boolean;
    focused: boolean;
    disabled: boolean;
    onSelect: () => void;
    onFocus: () => void;
  }

  let {
    label,
    description,
    optionIndex,
    selected,
    focused,
    disabled,
    onSelect,
    onFocus,
  }: Props = $props();
</script>

<button
  type="button"
  class={[
    'flex w-full items-start gap-2 rounded-[var(--radius-control)] border px-2.5 py-2 text-left',
    'transition-[background-color,border-color,color] duration-150',
    'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    selected
      ? 'border-accent/50 bg-accent/10 text-fg'
      : focused
        ? 'border-accent/30 bg-surface-1 text-fg'
        : 'border-border-subtle bg-surface-0/40 text-fg-muted hover:border-border hover:text-fg',
  ].join(' ')}
  data-testid={`user-input-option-${optionIndex + 1}`}
  aria-pressed={selected ? 'true' : 'false'}
  {disabled}
  onclick={onSelect}
  onmouseenter={onFocus}
  onfocus={onFocus}
>
  <span class="mt-0.5 inline-flex h-4 min-w-4 items-center justify-center rounded border border-border-subtle text-[10px] text-fg-muted">
    {optionIndex + 1}
  </span>
  <span class="min-w-0 flex-1">
    <span class="block text-xs font-medium">{label}</span>
    {#if description}
      <span class="mt-0.5 block text-[11px] leading-4 text-fg-muted">{description}</span>
    {/if}
  </span>
  {#if selected}
    <Icon icon={Check} size={14} class="mt-0.5 text-accent" />
  {/if}
</button>
