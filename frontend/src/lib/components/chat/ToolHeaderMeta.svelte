<script lang="ts">
  import type { Snippet } from 'svelte';

  interface DurationSlot {
    testId: string;
    label: string;
  }

  interface TimestampSlot {
    testId: string;
    value: number;
    label: string;
  }

  interface Props {
    statusSlotTestId: string;
    duration?: DurationSlot;
    timestamp?: TimestampSlot;
    class?: string;
    status?: Snippet;
    trailingActions?: Snippet;
  }

  let {
    statusSlotTestId,
    duration,
    timestamp,
    class: className = '',
    status,
    trailingActions,
  }: Props = $props();

  let timestampDate = $derived(timestamp === undefined ? null : new Date(timestamp.value));
</script>

<span class="flex shrink-0 items-center gap-2 {className}">
  <span
    class="inline-flex min-w-5 shrink-0 items-center justify-center"
    data-testid={statusSlotTestId}
  >
    {#if status}
      {@render status()}
    {/if}
  </span>

  {#if duration}
    <span
      class="shrink-0 inline-block min-w-[3rem] text-right tabular-nums text-[10px] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100"
      data-testid={duration.testId}
    >
      {duration.label}
    </span>
  {/if}

  {#if timestamp && timestampDate}
    <time
      class="shrink-0 tabular-nums text-[10px] text-fg-hint"
      datetime={timestampDate.toISOString()}
      data-testid={timestamp.testId}
    >
      {timestamp.label}
    </time>
  {/if}

  {#if trailingActions}
    {@render trailingActions()}
  {/if}
</span>
