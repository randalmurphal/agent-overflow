<script lang="ts">
  // The right-hand meta columns of a tool row header: status slot,
  // duration, timestamp — in that order, every row, so the columns line
  // up down the transcript. Row ACTIONS (open-in-pane, background, stop)
  // render FIRST, before the status slot: a control appended after the
  // timestamp shifts that column on exactly the rows that carry it
  // (user ruling 2026-08-23; see AGENTS.md "Row Contract").
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
    /** Row actions. Rendered before the meta columns — never after the timestamp. */
    actions?: Snippet;
  }

  let {
    statusSlotTestId,
    duration,
    timestamp,
    class: className = '',
    status,
    actions,
  }: Props = $props();

  // Non-finite values produce an Invalid Date, which is truthy but whose
  // toISOString() throws mid-render — drop the <time> element instead of
  // crashing the row over one corrupt timestamp.
  let timestampDate = $derived(
    timestamp === undefined || !Number.isFinite(timestamp.value)
      ? null
      : new Date(timestamp.value),
  );
</script>

<span class="flex shrink-0 items-center gap-2 compact:gap-1 {className}" data-tool-header-meta>
  {#if actions}
    {@render actions()}
  {/if}

  <span
    class="inline-flex min-w-5 compact:min-w-3 shrink-0 items-center justify-center"
    data-testid={statusSlotTestId}
  >
    {#if status}
      {@render status()}
    {/if}
  </span>

  {#if duration}
    <span
      class="shrink-0 inline-block min-w-[3rem] compact:min-w-[2rem] text-right tabular-nums text-[0.625rem] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100"
      data-testid={duration.testId}
    >
      {duration.label}
    </span>
  {/if}

  {#if timestamp && timestampDate}
    <time
      class="shrink-0 tabular-nums text-[0.625rem] text-fg-hint"
      datetime={timestampDate.toISOString()}
      data-testid={timestamp.testId}
    >
      {timestamp.label}
    </time>
  {/if}
</span>
