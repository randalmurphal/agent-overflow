<script lang="ts">
  import type { Component } from 'svelte';
  import type { LucideProps } from '@lucide/svelte';
  import Icon from '../primitives/Icon.svelte';
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';

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
    /** A write waiting on its answer: the stepped spinner stands in
     * for the icon and the button is disabled. */
    pending?: boolean;
    testid?: string;
  }

  let {
    icon,
    label,
    onclick,
    disabled = false,
    disabledLabel,
    spinning = false,
    pending = false,
    testid,
  }: Props = $props();
</script>

<button
  type="button"
  class="inline-flex size-6 shrink-0 items-center justify-center rounded-[var(--radius-control)] text-fg-muted compact:size-8 hover:bg-surface-2 hover:text-fg disabled:opacity-45 disabled:hover:bg-transparent disabled:hover:text-fg-muted data-[pending]:opacity-100"
  aria-label={label}
  title={(disabled || pending) && disabledLabel ? disabledLabel : label}
  data-testid={testid}
  data-pending={pending || undefined}
  disabled={disabled || pending}
  onclick={() => onclick()}
>
  {#if pending}
    <SteppedSpinner size={13} />
  {:else}
    <Icon {icon} size={13} class={spinning ? 'animate-spin' : ''} />
  {/if}
</button>
