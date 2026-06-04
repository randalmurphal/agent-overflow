<script lang="ts">
  // Bottom row of the composer card. Children still own their binding
  // calls; this file owns their layout and the measured compact mode that
  // hides low-value labels when the full toolbar would overflow.
  //
  // The toolbar now sits inside the composer card (no border-t, no
  // bg-surface-1 of its own) so the whole composer reads as one
  // continuous surface. Left-side controls share one compact spacing
  // rule so the model, effort, mode, access, and plan actions read as
  // one stable control cluster.

  import { onMount } from 'svelte';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import ModelProviderMenu from './ModelProviderMenu.svelte';
  import EffortMenu from './EffortMenu.svelte';
  import AgentModeToggle from './AgentModeToggle.svelte';
  import AccessToggle from './AccessToggle.svelte';
  import McpServersTrigger from './McpServersTrigger.svelte';
  import PlanSidebarToggleButton from './PlanSidebarToggleButton.svelte';
  import SendButton from './SendButton.svelte';
  import ContextWindowMeter from '../../chat/ContextWindowMeter.svelte';
  import RateLimitMeter from '../../chat/RateLimitMeter.svelte';
  import type { SendButtonAction } from './sendButtonTypes';
  import { asProviderID } from '../../../types/providers';
  import { measureComposerToolbarCompact } from './composerToolbarDensity';

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
    onSend: () => void;
    onSendWithoutPlanComments?: () => void;
    onSendInNewThread?: () => void;
    onInterrupt: () => void;
    hideSendButton?: boolean;
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
    onSend,
    onSendWithoutPlanComments,
    onSendInNewThread,
    onInterrupt,
    hideSendButton = false,
  }: Props = $props();

  // Mode-toggle slot rules (immutable thread type policy):
  //   - chat threads: AgentModeToggle (chat ↔ plan)
  //   - design threads: nothing — design is its own surface; the
  //     in-pane ThreadModePicker in the workspace strip already
  //     surfaces the thread's mode, no second indicator needed here
  //   - discussion threads: nothing — discussion has its own composer flow
  let isDesignThread = $derived(pane.thread?.mode === 'design');
  let isDiscussionThread = $derived(pane.thread?.mode === 'discussion');

  // Rate-limit rings appear once the thread is "locked in" — the
  // user has sent at least one message and the provider/model
  // selection is committed. The data itself lives in the global
  // `rateLimitsInfo.svelte.ts` store keyed by provider, so a freshly
  // locked thread immediately reflects the most recent observation
  // for that account (5h/7d limits don't reset on thread switch or
  // turn completion).
  let showLimitRings = $derived(pane.isLocked && !!pane.thread?.provider);
  let providerID = $derived(asProviderID(pane.thread?.provider));
  // Pickers render against either a persisted thread or a draft
  // placeholder. The pane carries a synthetic thread object in both
  // cases so the pickers can read its mode/provider/etc.; placeholder
  // actions update local state or new-thread defaults without creating
  // a row.
  let hasComposableSurface = $derived(pane.canCompose);
  let toolbarEl: HTMLDivElement | undefined = $state(undefined);
  let compactToolbar = $state(true);
  let measureFrame = 0;

  function measureToolbarDensity(): void {
    if (!toolbarEl) return;
    compactToolbar = measureComposerToolbarCompact(toolbarEl);
  }

  function scheduleToolbarDensityMeasure(): void {
    const el = toolbarEl;
    if (!el) return;
    if (typeof requestAnimationFrame === 'undefined') {
      measureToolbarDensity();
      return;
    }
    if (measureFrame) return;
    measureFrame = requestAnimationFrame(() => {
      measureFrame = 0;
      if (toolbarEl !== el) return;
      measureToolbarDensity();
    });
  }

  onMount(() => {
    const el = toolbarEl;
    if (!el) return;
    scheduleToolbarDensityMeasure();
    const resizeObserver = new ResizeObserver(() => scheduleToolbarDensityMeasure());
    resizeObserver.observe(el);
    const mutationObserver = typeof MutationObserver === 'undefined'
      ? undefined
      : new MutationObserver(() => scheduleToolbarDensityMeasure());
    mutationObserver?.observe(el, {
      childList: true,
      characterData: true,
      subtree: true,
    });
    return () => {
      resizeObserver.disconnect();
      mutationObserver?.disconnect();
      if (measureFrame) cancelAnimationFrame(measureFrame);
    };
  });
</script>

<div
  bind:this={toolbarEl}
  class="flex items-center gap-0.5 px-2.5 pb-2 pt-1"
  data-compact={compactToolbar ? 'true' : 'false'}
  data-composer-toolbar
  data-testid="composer-toolbar"
>
  {#if hasComposableSurface}
    <ModelProviderMenu {pane} />
    <EffortMenu {pane} />
    {#if !isDesignThread && !isDiscussionThread}
      <AgentModeToggle {pane} />
    {/if}
    <AccessToggle {pane} />
    <McpServersTrigger {pane} />
    <PlanSidebarToggleButton {pane} {hasCurrentPlan} />
  {/if}
  <div class="ml-auto flex items-center gap-1.5">
    {#if showLimitRings}
      <div class="shrink-0 flex items-center" data-testid="composer-rate-limit-5h">
        <RateLimitMeter
          windowMins={300}
          provider={providerID ?? undefined}
        />
      </div>
      <div class="shrink-0 flex items-center" data-testid="composer-rate-limit-7d">
        <RateLimitMeter
          windowMins={10080}
          provider={providerID ?? undefined}
        />
      </div>
    {/if}
    {#if pane.contextWindow}
      <div class="shrink-0 flex items-center" data-testid="composer-context-meter">
        <ContextWindowMeter data={pane.contextWindow} thread={pane.thread} />
      </div>
    {/if}
    {#if !hideSendButton}
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
    {/if}
  </div>
</div>

<style>
  :global([data-composer-toolbar][data-compact="true"] [data-composer-toolbar-label="collapsible"]) {
    display: none;
  }
</style>
