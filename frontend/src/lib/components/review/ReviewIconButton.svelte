<script lang="ts">
  import type { Component } from 'svelte';
  import type { LucideProps } from '@lucide/svelte';
  import Icon from '../primitives/Icon.svelte';

  // The review surfaces' compact icon action: aria-label and title carry
  // the same text, so every one of these is a button with hovertext.

  interface Props {
    icon: Component<LucideProps>;
    label: string;
    onclick: () => void;
    disabled?: boolean;
    /** Disabled-state tooltip override (why it is disabled). */
    disabledLabel?: string;
    spinning?: boolean;
    testid?: string;
  }

  let {
    icon,
    label,
    onclick,
    disabled = false,
    disabledLabel,
    spinning = false,
    testid,
  }: Props = $props();
</script>

<button
  type="button"
  class="inline-flex size-6 shrink-0 items-center justify-center rounded-[var(--radius-control)] text-fg-muted hover:bg-surface-2 hover:text-fg disabled:opacity-45 disabled:hover:bg-transparent disabled:hover:text-fg-muted"
  aria-label={label}
  title={disabled && disabledLabel ? disabledLabel : label}
  data-testid={testid}
  {disabled}
  onclick={() => onclick()}
>
  <Icon {icon} size={13} class={spinning ? 'animate-spin' : ''} />
</button>
