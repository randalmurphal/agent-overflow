<script lang="ts">
  // Consolidated activity rail at the top of the composer card. Owns
  // the three "what's the agent doing right now" sub-states:
  //
  //  - Working timer: visible when a wire round is active or a queued
  //    user message is bridging the gap between rounds. Information-
  //    only (the composer's Send button doubles as Stop).
  //  - Todos segment: visible when `pane.liveTodo` carries a snapshot.
  //    Toggle expands an inline body listing the steps. State of the
  //    toggle is `pane.activityRailTodosOpen` so it survives thread
  //    switches in-session.
  //  - Background segment: visible when there are tray-eligible
  //    background items. Toggle expands an inline body listing the
  //    rows with provider-aware stop affordances.
  //
  // The whole rail collapses out completely when none of those are
  // active (idle thread, no todos, no background tasks).

  import { onDestroy, onMount } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getActiveTurn, isThreadWorking } from '../../stores/threadStatuses.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import { createBackgroundController } from './activityRailBackground.svelte';
  import ActivityRailTodosBody from './ActivityRailTodosBody.svelte';
  import ActivityRailBackgroundBody from './ActivityRailBackgroundBody.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  const PREVIEW_MAX_CHARS = 60;

  let now = $state(Date.now());

  let activeTurn = $derived(getActiveTurn(pane.threadId));
  let isWorking = $derived(isThreadWorking(pane.threadId));

  let elapsedLabel = $derived.by(() => {
    if (!activeTurn) return '0s';
    const elapsedSeconds = Math.max(0, Math.floor((now - activeTurn.startedAt) / 1_000));
    return formatElapsedSeconds(elapsedSeconds);
  });

  let liveTodo = $derived(pane.liveTodo);
  let todosOpen = $derived(pane.activityRailTodosOpen);
  let backgroundOpen = $derived(pane.activityRailBackgroundOpen);

  let completedCount = $derived.by(() => {
    if (!liveTodo) return 0;
    let n = 0;
    for (const step of liveTodo.steps) if (step.status === 'completed') n++;
    return n;
  });

  // First in-progress step, truncated, for the collapsed segment preview.
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

  const bg = createBackgroundController(() => pane, () => now);
  let bgDispose: (() => void) | null = null;
  onMount(() => { bgDispose = bg.mount(); });
  onDestroy(() => { bgDispose?.(); });

  // Rail visible when ANY of the three sub-states is non-empty. Each
  // segment within the row has its own predicate so e.g. a
  // background-only render doesn't include working chrome.
  let railVisible = $derived(isWorking || liveTodo !== null || bg.count > 0);

  // 1Hz clock for the working elapsed label and the background body's
  // elapsed labels + retention prune. Idle when nothing wants it.
  $effect(() => {
    const wantsClockForWorking = activeTurn !== null;
    const wantsClockForBackground = backgroundOpen || bg.hasPendingCompletion;
    if (!wantsClockForWorking && !wantsClockForBackground) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

</script>

{#if railVisible}
  <div
    class="relative border-b border-border-subtle"
    role="region"
    aria-label="Activity"
    data-testid="activity-rail"
  >
    {#if isWorking}
      <span
        class="activity-shimmer pointer-events-none absolute inset-x-0 top-0 z-10 block h-px overflow-hidden"
        aria-hidden="true"
        data-testid="activity-rail-shimmer"
      ></span>
    {/if}
    <div class="flex flex-wrap items-center gap-1.5 px-3 py-2 text-[0.6875rem] leading-tight">
      {#if isWorking}
        <span
          class="inline-flex items-center gap-1.5 rounded px-1.5 py-0.5"
          role="status"
          aria-live="polite"
          data-testid="activity-rail-working"
        >
          <span class="inline-flex items-center gap-[3px]" aria-hidden="true">
            <span class="h-1 w-1 rounded-full bg-fg-hint/65 animate-pulse"></span>
            <span class="h-1 w-1 rounded-full bg-fg-hint/65 animate-pulse [animation-delay:200ms]"></span>
            <span class="h-1 w-1 rounded-full bg-fg-hint/65 animate-pulse [animation-delay:400ms]"></span>
          </span>
          {#if activeTurn}
            <span class="text-fg-muted">
              Working <span
                class="tabular-nums text-fg-default"
                data-testid="activity-rail-working-elapsed"
              >{elapsedLabel}</span>
            </span>
          {:else}
            <span class="text-fg-muted" data-testid="activity-rail-working-bridge">Working</span>
          {/if}
        </span>
      {/if}

      {#if liveTodo}
        {#if isWorking}
          <span class="select-none text-fg-hint/60" aria-hidden="true">·</span>
        {/if}
        <button
          type="button"
          class="inline-flex items-center gap-1.5 rounded px-1.5 py-0.5 text-fg-muted transition-colors hover:bg-surface-2/45 hover:text-fg-default focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35 {todosOpen ? 'bg-accent/10 text-accent' : ''}"
          onclick={() => pane.toggleActivityRailTodos()}
          aria-controls="activity-rail-todos-body"
          aria-expanded={todosOpen}
          data-testid="activity-rail-todos-toggle"
        >
          <Icon
            icon={todosOpen ? ChevronDown : ChevronRight}
            size={11}
            strokeWidth={2.25}
            class="shrink-0 text-fg-hint/70"
          />
          <span>Todos</span>
          <span
            class="rounded-[var(--radius-field)] bg-accent/15 px-1 text-[0.625rem] font-medium text-accent"
            data-testid="activity-rail-todos-count"
          >{completedCount}/{liveTodo.steps.length}</span>
          {#if inProgressPreview}
            <span
              class="hidden text-fg-hint/70 sm:inline"
              data-testid="activity-rail-todos-preview"
            >{inProgressPreview}</span>
          {/if}
        </button>
      {/if}

      {#if bg.count > 0}
        {#if isWorking || liveTodo}
          <span class="select-none text-fg-hint/60" aria-hidden="true">·</span>
        {/if}
        <button
          type="button"
          class="inline-flex items-center gap-1.5 rounded px-1.5 py-0.5 text-fg-muted transition-colors hover:bg-surface-2/45 hover:text-fg-default focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35 {backgroundOpen ? 'bg-accent/10 text-accent' : ''}"
          onclick={() => pane.toggleActivityRailBackground()}
          aria-controls="activity-rail-background-body"
          aria-expanded={backgroundOpen}
          data-testid="activity-rail-background-toggle"
        >
          <Icon
            icon={backgroundOpen ? ChevronDown : ChevronRight}
            size={11}
            strokeWidth={2.25}
            class="shrink-0 text-fg-hint/70"
          />
          <span>Background</span>
          <span
            class="rounded-[var(--radius-field)] bg-accent/15 px-1 text-[0.625rem] font-medium text-accent"
            data-testid="activity-rail-background-count"
          >{bg.count}</span>
          {#if bg.anyRunning}
            <span
              class="h-1.5 w-1.5 rounded-full bg-accent animate-pulse"
              aria-hidden="true"
              data-testid="activity-rail-background-pulse"
            ></span>
          {/if}
        </button>
      {/if}
    </div>

    {#if todosOpen && liveTodo}
      <ActivityRailTodosBody {liveTodo} {pane} />
    {/if}

    {#if backgroundOpen && bg.count > 0}
      <ActivityRailBackgroundBody
        tasks={bg.tasks}
        provider={bg.provider}
        threadId={bg.threadId}
        runningCount={bg.runningCount}
        anyRunning={bg.anyRunning}
      />
    {/if}
  </div>
{/if}
