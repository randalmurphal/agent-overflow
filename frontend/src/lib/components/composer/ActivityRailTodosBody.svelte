<script lang="ts">
  // Expanded body for the activity rail's Todos segment. Lists each
  // step with status icon + label. Status order: in-progress first
  // (so the active task is visible at a glance), pending next,
  // completed last. Wire order is preserved within each bucket so the
  // user can match the list against what the agent is doing.

  import type { LiveTodo, ThreadPane } from '../../stores/thread.svelte';
  import type { TodoStep, TodoStepStatus } from '../../types/events';
  import Icon from '../primitives/Icon.svelte';
  import Circle from 'lucide-svelte/icons/circle';
  import CircleCheck from 'lucide-svelte/icons/circle-check';
  import LoaderCircle from 'lucide-svelte/icons/loader-circle';

  interface Props {
    liveTodo: LiveTodo;
    pane: ThreadPane;
  }

  let { liveTodo, pane }: Props = $props();

  const TODO_TRUNCATION_LIMIT = 5;
  const statusRank: Record<TodoStepStatus, number> = {
    inProgress: 0,
    pending: 1,
    completed: 2,
  };
  const revealButtonClass = 'ml-[18px] mt-0.5 inline-flex rounded px-1 py-0.5 text-[11px] text-fg-hint/65 transition-colors hover:bg-surface-2/40 hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35';

  type SortedEntry = { step: TodoStep; originalIndex: number };

  let showAll = $derived(pane.liveTodoShowAll);

  let sortedSteps = $derived.by<SortedEntry[]>(() =>
    liveTodo.steps
      .map((step, originalIndex) => ({ step, originalIndex }))
      .sort((a, b) => {
        const r = (statusRank[a.step.status] ?? 1) - (statusRank[b.step.status] ?? 1);
        return r !== 0 ? r : a.originalIndex - b.originalIndex;
      }),
  );

  let visibleSteps = $derived(
    showAll ? sortedSteps : sortedSteps.slice(0, TODO_TRUNCATION_LIMIT),
  );
  let hiddenCount = $derived(Math.max(0, sortedSteps.length - visibleSteps.length));
  let hasOverflow = $derived(sortedSteps.length > TODO_TRUNCATION_LIMIT);

  let counts = $derived.by(() => {
    let inProgress = 0;
    let pending = 0;
    let completed = 0;
    for (const step of liveTodo.steps) {
      if (step.status === 'inProgress') inProgress++;
      else if (step.status === 'completed') completed++;
      else pending++;
    }
    return { inProgress, pending, completed };
  });

  let summaryLabel = $derived(
    `${counts.inProgress} in progress, ${counts.pending} pending, ${counts.completed} completed`,
  );

  function statusIcon(status: TodoStepStatus) {
    if (status === 'completed') return CircleCheck;
    if (status === 'inProgress') return LoaderCircle;
    return Circle;
  }

  function statusClass(status: TodoStepStatus): string {
    if (status === 'completed') return 'text-fg-hint/55 line-through decoration-fg-hint/40';
    if (status === 'inProgress') return 'font-medium text-fg-default';
    return 'text-fg-muted';
  }

  function statusIconClass(status: TodoStepStatus): string {
    if (status === 'inProgress') return 'animate-spin text-accent';
    if (status === 'completed') return 'text-fg-hint/55';
    return 'text-fg-hint/65';
  }
</script>

<div
  id="activity-rail-todos-body"
  class="border-t border-border-subtle px-3 py-2.5"
  data-testid="activity-rail-todos-body"
>
  <div class="mb-1.5 font-mono text-[10.5px] text-fg-hint/70">
    {summaryLabel}
  </div>
  <ul class="flex flex-col gap-0.5 pl-1" data-testid="activity-rail-todos-list">
    {#each visibleSteps as entry (entry.originalIndex)}
      <li class="flex items-start gap-1.5 py-px text-[12px] leading-snug">
        <Icon
          icon={statusIcon(entry.step.status)}
          size={11}
          strokeWidth={2}
          class={`mt-[3px] shrink-0 ${statusIconClass(entry.step.status)}`}
        />
        <span class={statusClass(entry.step.status)}>{entry.step.step}</span>
      </li>
    {/each}
    {#if !showAll && hiddenCount > 0}
      <li>
        <button
          type="button"
          class={revealButtonClass}
          onclick={() => pane.toggleLiveTodoShowAll()}
          data-testid="activity-rail-todos-show-more"
        >
          Show {hiddenCount} more…
        </button>
      </li>
    {/if}
    {#if showAll && hasOverflow}
      <li>
        <button
          type="button"
          class={revealButtonClass}
          onclick={() => pane.toggleLiveTodoShowAll()}
          data-testid="activity-rail-todos-show-less"
        >
          Show less
        </button>
      </li>
    {/if}
  </ul>
</div>
