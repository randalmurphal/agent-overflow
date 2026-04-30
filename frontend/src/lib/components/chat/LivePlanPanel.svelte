<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { PlanStep, PlanStepStatus } from '../../types/events';
  import Icon from '../primitives/Icon.svelte';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import Circle from 'lucide-svelte/icons/circle';
  import CircleCheck from 'lucide-svelte/icons/circle-check';
  import LoaderCircle from 'lucide-svelte/icons/loader-circle';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let livePlan = $derived(pane.livePlan);
  let isExpanded = $derived(pane.livePlanExpanded);
  let isShowingAll = $derived(pane.livePlanShowAll);

  // Status order for the sorted list. Numbers map to the buckets the
  // user asked for: in-progress on top so the active task is the first
  // thing seen; completed at the bottom so finished work doesn't
  // dominate the visible window. Within each bucket the original wire
  // order is preserved so the agent's intended sequence stays readable.
  const statusRank: Record<PlanStepStatus, number> = {
    inProgress: 0,
    pending: 1,
    completed: 2,
  };

  function rankFor(status: PlanStepStatus): number {
    return statusRank[status] ?? statusRank.pending;
  }

  // sortedSteps preserves the original wire index inside each entry so
  // the keyed-each below stays unique even when two steps share text +
  // status (legal on the wire — TodoWrite has no uniqueness constraint).
  type SortedEntry = { step: PlanStep; originalIndex: number };

  let sortedSteps = $derived.by<SortedEntry[]>(() => {
    if (!livePlan) return [];
    return livePlan.steps
      .map((step, originalIndex) => ({ step, originalIndex }))
      .sort((a, b) => {
        const r = rankFor(a.step.status) - rankFor(b.step.status);
        return r !== 0 ? r : a.originalIndex - b.originalIndex;
      });
  });

  const TRUNCATION_LIMIT = 5;

  let visibleSteps = $derived(
    isShowingAll ? sortedSteps : sortedSteps.slice(0, TRUNCATION_LIMIT),
  );

  let hiddenCount = $derived(Math.max(0, sortedSteps.length - visibleSteps.length));

  let counts = $derived.by(() => {
    let inProgress = 0;
    let pending = 0;
    let completed = 0;
    if (!livePlan) return { inProgress, pending, completed };
    for (const step of livePlan.steps) {
      if (step.status === 'inProgress') inProgress++;
      else if (step.status === 'completed') completed++;
      else pending++;
    }
    return { inProgress, pending, completed };
  });

  let summaryLabel = $derived(
    `${counts.inProgress} in progress, ${counts.pending} pending, ${counts.completed} completed`,
  );

  function statusIcon(status: PlanStepStatus) {
    if (status === 'completed') return CircleCheck;
    if (status === 'inProgress') return LoaderCircle;
    return Circle;
  }

  function statusClass(status: PlanStepStatus): string {
    if (status === 'completed') return 'text-fg-hint/55 line-through decoration-fg-hint/40';
    if (status === 'inProgress') return 'text-accent';
    return 'text-fg-muted';
  }

  function statusIconClass(status: PlanStepStatus): string {
    if (status === 'inProgress') return 'animate-spin text-accent';
    if (status === 'completed') return 'text-fg-hint/55';
    return 'text-fg-hint/65';
  }
</script>

{#if livePlan}
  <div
    class="mb-4 flex flex-col gap-1 pl-1.5 text-[11px] text-fg-hint/80"
    data-testid="live-plan-panel"
  >
    <button
      type="button"
      class="inline-flex items-center gap-1.5 self-start rounded px-1 py-0.5 text-left text-fg-hint/80 transition-colors hover:bg-surface-2/40 hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35"
      onclick={() => pane.toggleLivePlanExpanded()}
      aria-expanded={isExpanded}
      data-testid="live-plan-toggle"
    >
      <Icon
        icon={isExpanded ? ChevronDown : ChevronRight}
        size={11}
        strokeWidth={2.25}
        class="opacity-70"
      />
      <span class="font-medium">Plan</span>
      <span class="text-fg-hint/65">·</span>
      <span class="tabular-nums" data-testid="live-plan-counts">{summaryLabel}</span>
    </button>

    {#if isExpanded}
      <ul class="ml-5 mt-0.5 flex flex-col gap-0.5" data-testid="live-plan-list">
        {#each visibleSteps as entry (entry.originalIndex)}
          <li class="flex items-start gap-1.5 leading-snug">
            <Icon
              icon={statusIcon(entry.step.status)}
              size={11}
              strokeWidth={2}
              class={`mt-[3px] shrink-0 ${statusIconClass(entry.step.status)}`}
            />
            <span class={statusClass(entry.step.status)}>{entry.step.step}</span>
          </li>
        {/each}
        {#if hiddenCount > 0}
          <li>
            <button
              type="button"
              class="ml-[18px] mt-0.5 inline-flex rounded px-1 py-0.5 text-fg-hint/65 transition-colors hover:bg-surface-2/40 hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35"
              onclick={() => pane.toggleLivePlanShowAll()}
              data-testid="live-plan-show-more"
            >
              Show {hiddenCount} more…
            </button>
          </li>
        {/if}
      </ul>
    {/if}
  </div>
{/if}
