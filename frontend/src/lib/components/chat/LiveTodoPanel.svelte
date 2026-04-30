<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { TodoStep, TodoStepStatus } from '../../types/events';
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

  let liveTodo = $derived(pane.liveTodo);
  let isExpanded = $derived(pane.liveTodoExpanded);
  let isShowingAll = $derived(pane.liveTodoShowAll);

  // Status order for the sorted list. Numbers map to the buckets the
  // user asked for: in-progress on top so the active task is the first
  // thing seen; completed at the bottom so finished work doesn't
  // dominate the visible window. Within each bucket the original wire
  // order is preserved so the agent's intended sequence stays readable.
  const statusRank: Record<TodoStepStatus, number> = {
    inProgress: 0,
    pending: 1,
    completed: 2,
  };

  function rankFor(status: TodoStepStatus): number {
    return statusRank[status] ?? statusRank.pending;
  }

  // sortedSteps preserves the original wire index inside each entry so
  // the keyed-each below stays unique even when two steps share text +
  // status (legal on the wire — TodoWrite has no uniqueness constraint).
  type SortedEntry = { step: TodoStep; originalIndex: number };

  let sortedSteps = $derived.by<SortedEntry[]>(() => {
    if (!liveTodo) return [];
    return liveTodo.steps
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
    if (!liveTodo) return { inProgress, pending, completed };
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

  // Pulled into the collapsed header so the user can see what the
  // agent is doing right now without expanding. Picks the first
  // in-progress step in original wire order (matches the sort) so the
  // preview stays stable across re-emissions.
  const PREVIEW_MAX_CHARS = 60;
  let inProgressPreview = $derived.by<string | null>(() => {
    if (!liveTodo) return null;
    for (const step of liveTodo.steps) {
      if (step.status === 'inProgress') {
        const text = step.step;
        return text.length > PREVIEW_MAX_CHARS
          ? text.slice(0, PREVIEW_MAX_CHARS).trimEnd() + '…'
          : text;
      }
    }
    return null;
  });

  function statusIcon(status: TodoStepStatus) {
    if (status === 'completed') return CircleCheck;
    if (status === 'inProgress') return LoaderCircle;
    return Circle;
  }

  // Row text color. In-progress is bumped from accent (which the
  // earlier draft used) to fg-default + bold — accent on a long line
  // of text fights with the per-row accent bar; saving accent for the
  // bar + the loader spinner gives the row a single focal point.
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

  // 2px accent left bar on the in-progress row anchors the eye on the
  // active step. Other rows reserve the same 2px gutter (transparent)
  // so the row text stays vertically aligned across status changes.
  function rowBarClass(status: TodoStepStatus): string {
    if (status === 'inProgress') return 'border-l-2 border-accent';
    return 'border-l-2 border-transparent';
  }
</script>

{#if liveTodo}
  <!--
    The panel is anchored to the working indicator visually with a thin
    left-border guide running the height of the block. Same indent as
    the working indicator's text so the two read as one unit. The guide
    also extends through the dropdown body when expanded so the panel
    feels like a single hanging element rather than a stack.
  -->
  <div
    class="mb-4 ml-1.5 flex flex-col gap-1 border-l border-fg-hint/15 pl-3 text-[11px]"
    data-testid="live-todo-panel"
  >
    <button
      type="button"
      class="-ml-1 inline-flex items-start gap-1.5 self-start rounded px-1 py-0.5 text-left transition-colors hover:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35"
      onclick={() => pane.toggleLiveTodoExpanded()}
      aria-expanded={isExpanded}
      data-testid="live-todo-toggle"
    >
      <Icon
        icon={isExpanded ? ChevronDown : ChevronRight}
        size={11}
        strokeWidth={2.25}
        class="mt-[3px] shrink-0 text-fg-hint/70"
      />
      <span class="flex flex-col gap-0">
        <span class="inline-flex items-center gap-1.5">
          <span class="font-semibold text-fg-muted">Todos</span>
          {#if inProgressPreview}
            <span class="text-fg-hint/55">·</span>
            <span class="text-fg-default" data-testid="live-todo-in-progress-preview">
              {inProgressPreview}
            </span>
          {/if}
        </span>
        <span
          class="text-[10.5px] tabular-nums text-fg-hint/70"
          data-testid="live-todo-counts"
        >
          {summaryLabel}
        </span>
      </span>
    </button>

    {#if isExpanded}
      <ul class="mt-0.5 flex flex-col gap-0.5" data-testid="live-todo-list">
        {#each visibleSteps as entry (entry.originalIndex)}
          <li
            class={`flex items-start gap-1.5 py-px pl-1.5 leading-snug ${rowBarClass(entry.step.status)}`}
          >
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
          <li class="border-l-2 border-transparent">
            <button
              type="button"
              class="ml-[14px] mt-0.5 inline-flex rounded px-1 py-0.5 text-fg-hint/65 transition-colors hover:bg-surface-2/40 hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35"
              onclick={() => pane.toggleLiveTodoShowAll()}
              data-testid="live-todo-show-more"
            >
              Show {hiddenCount} more…
            </button>
          </li>
        {/if}
      </ul>
    {/if}
  </div>
{/if}
