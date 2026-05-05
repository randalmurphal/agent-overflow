<script lang="ts">
  // Bottom row of the composer card. Pure layout — every child owns its
  // own binding calls, so this file's only job is to arrange them
  // left-to-right and surface the send button on the right edge.
  //
  // The toolbar now sits inside the composer card (no border-t, no
  // bg-surface-1 of its own) so the whole composer reads as one
  // continuous surface. Left-side controls share one compact spacing
  // rule so the model, effort, mode, access, and plan actions read as
  // one stable control cluster.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import ModelProviderMenu from './ModelProviderMenu.svelte';
  import EffortMenu from './EffortMenu.svelte';
  import AgentModeToggle from './AgentModeToggle.svelte';
  import DesignLockPill from './DesignLockPill.svelte';
  import AccessToggle from './AccessToggle.svelte';
  import PlanSidebarToggleButton from './PlanSidebarToggleButton.svelte';
  import SendButton from './SendButton.svelte';
  import SendNowButton from './SendNowButton.svelte';
  import ContextWindowMeter from '../../chat/ContextWindowMeter.svelte';
  import type { SendButtonAction } from './sendButtonTypes';

  interface Props {
    pane: ThreadPane;
    canSend: boolean;
    isTurnActive: boolean;
    /**
     * Optimistic stop-button gate: true while the SendMessage RPC is
     * pending, before `provider:turn_started` arrives. Composer flips
     * it via `pane.setSendInFlight(true)` at the top of send() and
     * clears it in finally.
     */
    sendInFlight?: boolean;
    sendAction?: SendButtonAction;
    sendLabel?: string;
    hasCurrentPlan?: boolean;
    planCommentCount?: number;
    /**
     * True when the per-thread queue (Zone 1) has at least one
     * retractable item. Drives the "Send Now" affordance: while a turn
     * is active AND queued items exist, the user can ship the queue
     * immediately by interrupting (same wire result as Stop).
     */
    hasQueuedItems?: boolean;
    onSend: () => void;
    onSendWithoutPlanComments?: () => void;
    onSendInNewThread?: () => void;
    onInterrupt: () => void;
  }

  let {
    pane,
    canSend,
    isTurnActive,
    sendInFlight = false,
    sendAction,
    sendLabel,
    hasCurrentPlan = false,
    planCommentCount = 0,
    hasQueuedItems = false,
    onSend,
    onSendWithoutPlanComments,
    onSendInNewThread,
    onInterrupt,
  }: Props = $props();

  // Mode-toggle slot rules (immutable thread type policy):
  //   - chat threads: AgentModeToggle (chat ↔ plan)
  //   - design threads: DesignLockPill (display-only)
  //   - discussion threads: nothing — discussion has its own composer flow
  let isDesignThread = $derived(pane.thread?.mode === 'design');
  let isDiscussionThread = $derived(pane.thread?.mode === 'discussion');
</script>

<div
  class="@container flex items-center gap-0.5 px-2.5 pb-2 pt-1"
  data-testid="composer-toolbar"
>
  <ModelProviderMenu {pane} />
  <EffortMenu {pane} />
  {#if isDesignThread}
    <DesignLockPill />
  {:else if !isDiscussionThread}
    <AgentModeToggle {pane} />
  {/if}
  <AccessToggle {pane} />
  <PlanSidebarToggleButton {pane} {hasCurrentPlan} />
  <div class="ml-auto flex items-center gap-1.5">
    {#if pane.contextWindow}
      <div class="shrink-0 flex items-center" data-testid="composer-context-meter">
        <ContextWindowMeter data={pane.contextWindow} thread={pane.thread} />
      </div>
    {/if}
    <SendNowButton
      {isTurnActive}
      {hasQueuedItems}
      {onInterrupt}
    />
    <SendButton
      {canSend}
      {isTurnActive}
      {sendInFlight}
      action={sendAction}
      label={sendLabel}
      {planCommentCount}
      {onSend}
      {onSendWithoutPlanComments}
      {onSendInNewThread}
      {onInterrupt}
    />
  </div>
</div>
