<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';

  // The PR header's section chrome (Description, Conversation): a bordered
  // card with a chevron header, replacing the bare browser <details>
  // rendering. Controlled — the Conversation section's open state lives in
  // the review store because ordering freezes while it is open.

  interface Props {
    label: string;
    open: boolean;
    onToggle: () => void;
    /** Count chip after the label. */
    badge?: Snippet;
    /** Right-aligned chrome outside the toggle (the "N new" chip). */
    trailing?: Snippet;
    children: Snippet;
    testid?: string;
  }

  let { label, open, onToggle, badge, trailing, children, testid }: Props = $props();
</script>

<section
  class="mt-2.5 overflow-hidden rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/60"
  data-testid={testid}
>
  <div class="flex items-center gap-2 pr-2">
    <button
      type="button"
      class="flex min-w-0 flex-1 items-center gap-1.5 px-2 py-1.5 text-left text-xs font-medium text-fg-muted hover:text-fg"
      aria-expanded={open}
      onclick={onToggle}
    >
      <Icon icon={ChevronRight} size={12} class="shrink-0 transition-transform duration-100 {open ? 'rotate-90' : ''}" />
      <span class="truncate">{label}</span>
      {#if badge}{@render badge()}{/if}
    </button>
    {#if trailing}{@render trailing()}{/if}
  </div>
  {#if open}
    <div class="border-t border-border-subtle">
      {@render children()}
    </div>
  {/if}
</section>
