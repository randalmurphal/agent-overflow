<script lang="ts">
  // Consolidated activity rail at the top of the composer card. Owns
  // the "what's the agent doing right now" sub-states:
  //
  //  - Input requested chip: visible when `inputRequest` is set (a pending
  //    request_user_input the composer is showing). Clicking it toggles the
  //    popup body via `onToggleInput`; the chip is the minimize affordance.
  //    While present it suppresses the working segment (the agent is blocked
  //    on the user, so a running timer would be misleading).
  //  - Working timer: visible when a wire round is active or a queued
  //    user message is bridging the gap between rounds, and no input is
  //    pending. Information-only (the composer's Send button doubles as Stop).
  //  - Todos segment: visible when `pane.liveTodo` carries a snapshot.
  //    Toggle expands an inline body listing the steps. State of the
  //    toggle is `pane.activityRailTodosOpen` so it survives thread
  //    switches in-session.
  //  - Background segment: visible when there are tray-eligible
  //    background items. Toggle expands an inline body listing the
  //    rows with provider-aware stop affordances.
  //
  // Visibility is the HOST's call: Composer mounts the rail iff any
  // sub-state is active (`railVisible`) and renders its transparent
  // height-reservation spacer as the exact complement, so exactly one of
  // the two holds the row at all times and the composer's measured height
  // never changes across rail transitions. The background controller and
  // the shared clock are owned by Composer for the same reason (its
  // predicate reads `bg.count`) and arrive here as props.
  //
  // The row is single-line by contract: Composer.svelte reserves exactly
  // one row of height (`composer-activity-reserve`) while the rail is
  // hidden, so segments must never wrap. Every segment is shrink-0 except
  // the todos toggle, which shrinks so its preview can ellipsize when the
  // pane is too narrow to fit everything (see activityRailClasses.ts).

  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { UserInputRequest } from '../../types/events';
  import { getActiveTurn, isThreadWorking } from '../../stores/threadStatuses.svelte';
  import { isThreadCompacting } from '../../stores/compactingState.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import type { SharedNowClock } from '../chat/useRunningElapsed.svelte';
  import { activityRailChipClasses, activityRailRowClasses } from './activityRailClasses';
  import type { BackgroundController } from './activityRailBackground.svelte';
  import { stableTurnKey } from '../../spinners/pick';
  import WorkingChip from './WorkingChip.svelte';
  import ActivityRailTodosBody from './ActivityRailTodosBody.svelte';
  import ActivityRailBackgroundBody from './ActivityRailBackgroundBody.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';

  interface Props {
    pane: ThreadPane;
    /** Composer-owned background-tasks controller (mounted there). */
    bg: BackgroundController;
    /** Composer-owned shared 1Hz clock for elapsed labels + retention. */
    clock: SharedNowClock;
    /** Active pending user-input request, or null when none is pending or an
     *  approval is blocking. Drives the accent "Input requested" chip and,
     *  while present, suppresses the working timer (the agent is blocked on
     *  the user, so a running timer would be misleading). */
    inputRequest?: UserInputRequest | null;
    inputCollapsed?: boolean;
    onToggleInput?: () => void;
  }

  let {
    pane,
    bg,
    clock,
    inputRequest = null,
    inputCollapsed = false,
    onToggleInput = () => {},
  }: Props = $props();

  const PREVIEW_MAX_CHARS = 60;

  let activeTurn = $derived(getActiveTurn(pane.threadId));
  let isWorking = $derived(isThreadWorking(pane.threadId));

  // Per-turn spinner picks (docs: lib/spinners/) hash over this key in
  // WorkingChip, so a re-render or pane remount never rerolls mid-turn.
  // The key spans a WORKING SESSION: the queued-send bridge mints it and
  // the turn that follows adopts it, so the chip's verb and sprite hold
  // steady through the send handoff (see stableTurnKey). A pane with no
  // thread renders no working segment, so the empty-string fallback is
  // never a rendered pick — it only satisfies the picker's non-null key
  // contract.
  let spinnerThreadId = $derived(pane.threadId ?? '');
  let turnKey = $derived(stableTurnKey(spinnerThreadId, activeTurn?.turnId ?? null));

  let elapsedLabel = $derived.by(() => {
    if (!activeTurn) return '0s';
    const elapsedSeconds = Math.max(0, Math.floor((clock.now - activeTurn.startedAt) / 1_000));
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

  // First in-progress step for the collapsed segment preview. The char cap
  // bounds what a wide pane shows; narrow panes truncate further via CSS
  // (`truncate` on the preview span), keeping the rail to its single row.
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

  // While an input request is pending the agent is blocked on the user, so the
  // working hairline/LEDs/timer are suppressed and only the "Input requested"
  // chip shows. `showWorking` gates every working-segment render below.
  let showWorking = $derived(isWorking && inputRequest === null);
</script>

<div
  class="relative border-b border-border-subtle"
  role="region"
  aria-label="Activity"
  data-testid="activity-rail"
>
  {#if showWorking}
    <span
      class="working-hairline pointer-events-none absolute inset-x-0 top-0 z-10 block h-px"
      aria-hidden="true"
      data-testid="activity-rail-hairline"
    ></span>
  {/if}
  <div class={activityRailRowClasses}>
    {#if inputRequest}
      <button
        type="button"
        class="{activityRailChipClasses} shrink-0 font-bold uppercase tracking-[0.08em] text-accent transition-colors hover:bg-accent/10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35"
        onclick={onToggleInput}
        aria-controls="composer-pending-user-input"
        aria-expanded={!inputCollapsed}
        data-testid="activity-rail-input-toggle"
      >
        <Icon
          icon={inputCollapsed ? ChevronRight : ChevronDown}
          size={11}
          strokeWidth={2.25}
          class="shrink-0"
        />
        <span>Input requested</span>
      </button>
    {/if}

    {#if showWorking}
      <!-- Label-only swap to "Compacting" while the provider summarizes
           the thread's context; the elapsed timer keeps running off the
           turn start as usual. `showElapsed` reserves the timer's width
           across the pending-send handoff, before the provider supplies
           its authoritative start. -->
      <WorkingChip
        threadId={spinnerThreadId}
        pickKey={turnKey}
        compacting={isThreadCompacting(pane.threadId)}
        {elapsedLabel}
        showElapsed={activeTurn !== null}
      />
    {/if}

    {#if liveTodo}
      {#if inputRequest || showWorking}
        <span class="shrink-0 select-none text-fg-hint/60" aria-hidden="true">·</span>
      {/if}
      <!-- The one shrinkable segment (min-w-0): the truncate preview gives
           up width first; overflow-hidden additionally clips at the
           button's own edge in the degenerate case where even the fixed
           label/badge can't fit, instead of bleeding over the next chip. -->
      <button
        type="button"
        class="{activityRailChipClasses} min-w-0 overflow-hidden text-fg-muted transition-colors hover:bg-surface-2/45 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35 {todosOpen ? 'bg-accent/10 text-accent' : ''}"
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
        <span class="shrink-0">Todos</span>
        <span
          class="shrink-0 rounded-[var(--radius-field)] bg-accent/15 px-1 text-[0.625rem] font-medium text-accent"
          data-testid="activity-rail-todos-count"
        >{completedCount}/{liveTodo.steps.length}</span>
        {#if inProgressPreview}
          <span
            class="min-w-0 truncate text-fg-hint/70"
            data-testid="activity-rail-todos-preview"
          >{inProgressPreview}</span>
        {/if}
      </button>
    {/if}

    {#if bg.count > 0}
      {#if inputRequest || showWorking || liveTodo}
        <span class="shrink-0 select-none text-fg-hint/60" aria-hidden="true">·</span>
      {/if}
      <button
        type="button"
        class="{activityRailChipClasses} shrink-0 text-fg-muted transition-colors hover:bg-surface-2/45 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/35 {backgroundOpen ? 'bg-accent/10 text-accent' : ''}"
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
        {#if bg.runningCount > 0}
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
      {pane}
      runningCount={bg.runningCount}
    />
  {/if}
</div>
