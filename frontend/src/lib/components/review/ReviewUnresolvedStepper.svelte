<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronUp from '@lucide/svelte/icons/chevron-up';
  import Icon from '../primitives/Icon.svelte';

  // The unresolved-thread stepper: one segmented control. The label
  // segment (the count, or the title bar's Conversation peek carrying it)
  // is joined to the previous/next chevrons under ONE border, which the
  // wrapper owns. The segments stretch to the wrapper's content box, so a
  // hover tint fills a segment edge to edge and never crosses the border,
  // and the end segments round to the border's inner radius so the tint
  // follows the corners. Each chevron carries the divider before it.

  interface Props {
    label: Snippet;
    /** The label segment is a button with this action (the title bar's
     * peek to the Conversation section); absent, it is text. */
    onLabel?: () => void;
    onStep: (direction: 1 | -1) => void;
    testid?: string;
    labelTestid?: string;
    prevTestid?: string;
    nextTestid?: string;
  }

  let { label, onLabel, onStep, testid, labelTestid, prevTestid, nextTestid }: Props = $props();

  const labelClass = 'inline-flex items-center gap-1.5 rounded-l-[calc(var(--radius-field)-1px)] px-2 tabular-nums';
  const stepClass = 'inline-flex w-6 items-center justify-center border-l border-warning/40 hover:bg-warning/10 compact:w-8';
</script>

<span
  class="inline-flex h-6 shrink-0 items-stretch rounded-[var(--radius-field)] border border-warning/40 text-[0.6875rem] font-medium text-warning compact:h-8 compact:select-none"
  data-testid={testid}
>
  {#if onLabel}
    <button type="button" class="{labelClass} hover:bg-warning/10" data-testid={labelTestid} onclick={onLabel}>
      {@render label()}
    </button>
  {:else}
    <span class={labelClass} data-testid={labelTestid}>{@render label()}</span>
  {/if}
  <button
    type="button"
    class={stepClass}
    aria-label="Previous unresolved thread"
    title="Previous unresolved thread"
    data-testid={prevTestid}
    onclick={() => onStep(-1)}
  >
    <Icon icon={ChevronUp} size={13} />
  </button>
  <button
    type="button"
    class="{stepClass} rounded-r-[calc(var(--radius-field)-1px)]"
    aria-label="Next unresolved thread"
    title="Next unresolved thread"
    data-testid={nextTestid}
    onclick={() => onStep(1)}
  >
    <Icon icon={ChevronDown} size={13} />
  </button>
</span>
