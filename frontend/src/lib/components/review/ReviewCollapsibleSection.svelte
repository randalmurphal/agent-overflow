<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';

  // An overview section's chrome (Description, Conversation): a chevron
  // header over a bordered body. Controlled: both sections' open state
  // lives in the review store, because the overview row unmounts while
  // the reader is deep in the diff and the Conversation's ordering
  // freezes while it is open.

  interface Props {
    label: string;
    open: boolean;
    onToggle: () => void;
    /** Count chip after the label. */
    badge?: Snippet;
    /** Right-aligned chrome outside the toggle (the "N new" chip, the
     * unresolved stepper). */
    trailing?: Snippet;
    children: Snippet;
    testid?: string;
  }

  let { label, open, onToggle, badge, trailing, children, testid }: Props = $props();
</script>

<section
  class="overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/70"
  data-testid={testid}
  data-open={open ? 'true' : 'false'}
>
  <!-- The header wraps rather than clips: the toggle asks for a row's
       worth of width, so on a narrow pane the trailing controls (buttons,
       the stepper) drop to their own line under the label and pill, and
       the label never truncates. -->
  <div class="flex min-h-9 flex-wrap items-center gap-x-2 gap-y-1 pr-2">
    <button
      type="button"
      class="flex h-9 min-w-0 flex-1 basis-[14rem] items-center gap-2 px-2.5 text-left text-xs font-semibold text-fg compact:select-none"
      aria-expanded={open}
      onclick={onToggle}
    >
      <Icon icon={ChevronRight} size={13} class="shrink-0 text-fg-muted transition-transform duration-100 {open ? 'rotate-90' : ''}" />
      <span class="shrink-0 whitespace-nowrap">{label}</span>
      {#if badge}{@render badge()}{/if}
    </button>
    {#if trailing}
      <div class="ml-auto flex shrink-0 flex-wrap items-center justify-end gap-1.5 py-1 pl-2.5">
        {@render trailing()}
      </div>
    {/if}
  </div>
  {#if open}
    <div class="border-t border-border-subtle">
      {@render children()}
    </div>
  {/if}
</section>
