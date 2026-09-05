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
  import ComposerPickersRollup from './ComposerPickersRollup.svelte';
  import SendButton from './SendButton.svelte';
  import ContextWindowMeter from '../../chat/ContextWindowMeter.svelte';
  import RateLimitMeter from '../../chat/RateLimitMeter.svelte';
  import type { SendButtonAction } from './sendButtonTypes';
  import { asProviderID } from '../../../types/providers';
  import { providerSupports } from '../../../providers/catalog';
  import {
    measureComposerToolbarDensity,
    type ComposerToolbarDensity,
  } from './composerToolbarDensity';
  import { getProviderAccount } from '../../../stores/accountInfo.svelte';

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
    sendDisabledReason?: string;
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
    sendDisabledReason,
    hasCurrentPlan = false,
    planCommentCount = 0,
    onSend,
    onSendWithoutPlanComments,
    onSendInNewThread,
    onInterrupt,
    hideSendButton = false,
  }: Props = $props();

  // Discussion has its own composer flow; ordinary threads expose the
  // chat ↔ plan agent-mode toggle here.
  let isDiscussionThread = $derived(pane.thread?.mode === 'discussion');

  // Rate-limit rings appear once the thread is "locked in" — the
  // user has sent at least one message and the provider/model
  // selection is committed. The data itself lives in the global
  // `rateLimitsInfo.svelte.ts` store keyed by provider and account, so
  // a freshly locked thread immediately reflects the most recent
  // observation for the account serving its live session (5h/7d limits
  // don't reset on thread switch or turn completion).
  let showLimitRings = $derived(pane.isLocked && !!pane.thread?.provider);
  let providerID = $derived(asProviderID(pane.thread?.provider));
  let selectedAccount = $derived.by(() =>
    providerID ? getProviderAccount(providerID) : null,
  );
  let sessionAccount = $derived(pane.providerSessionAccount);
  let sessionUsesSelectedAccount = $derived(
    !!sessionAccount?.connected
      && !!selectedAccount?.accountId
      && sessionAccount.accountId === selectedAccount.accountId,
  );
  let currentAccountID = $derived(
    sessionAccount?.connected ? sessionAccount.accountId : selectedAccount?.accountId,
  );
  let currentAccountEmail = $derived(
    sessionAccount?.connected
      ? sessionUsesSelectedAccount
        ? (selectedAccount?.email || sessionAccount.account.email || '')
        : (sessionAccount.account.email ?? '')
      : (selectedAccount?.email ?? ''),
  );
  let currentAccountPlan = $derived(
    sessionAccount?.connected
      ? sessionUsesSelectedAccount
        ? (selectedAccount?.subscriptionType
          || sessionAccount.account.subscriptionType
          || '')
        : (sessionAccount.account.subscriptionType ?? '')
      : (selectedAccount?.subscriptionType ?? ''),
  );
  // Provider capability gates. claude-tui drives the real TUI, so the
  // AO-mediated runtime-mode, plan, and MCP affordances are omitted — the
  // human reaches them inside the terminal via take-control.
  let supportsPlanMode = $derived(providerSupports(providerID, 'planMode'));
  let supportsRuntimeModes = $derived(providerSupports(providerID, 'runtimeModes'));
  let supportsMcp = $derived(providerSupports(providerID, 'mcp'));
  // Pickers render against either a persisted thread or a draft
  // placeholder. The pane carries a synthetic thread object in both
  // cases so the pickers can read its mode/provider/etc.; placeholder
  // actions update local state or new-thread defaults without creating
  // a row.
  let hasComposableSurface = $derived(pane.canCompose);
  let toolbarEl: HTMLDivElement | undefined = $state(undefined);
  let toolbarDensity = $state<ComposerToolbarDensity>('compact');
  let measureFrame = 0;

  function measureToolbarDensity(): void {
    if (!toolbarEl) return;
    toolbarDensity = measureComposerToolbarDensity(toolbarEl);
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
    // Re-measure only when a width can have moved. Queue the read and the
    // data-compact write for the next frame rather than mutating layout from
    // inside the ResizeObserver delivery. A three-to-four-pane transition
    // otherwise changes the toolbar height while ancestor observers are
    // being delivered and WebView2 drops the remaining notifications as a
    // ResizeObserver loop. The
    // toolbar's own box covers pane resizes; one observed entry per
    // direct child covers every control whose rendered width moves (the
    // context meter growing a digit, the send label flipping) — a text
    // beat that moves no width delivers nothing at all. The old subtree
    // MutationObserver (childList + characterData) scheduled a rAF
    // measure on EVERY streaming beat — the token text mutates per beat
    // whether or not any width changed — and rAF runs BEFORE layout, so
    // each measure forced a full pass against the flush-dirty tree
    // (19-21 forced passes per 3-pane storm run, 2026-08-26).
    const sizes = new ResizeObserver(() => scheduleToolbarDensityMeasure());
    const observeChildren = () => {
      sizes.observe(el);
      for (const child of el.children) sizes.observe(child);
    };
    observeChildren();
    // Controls mount and unmount with provider/mode changes, not with
    // streaming beats. A new child needs observing (its initial RO
    // delivery then runs the measure); a removal delivers nothing, so
    // schedule a measure directly.
    const mutationObserver = typeof MutationObserver === 'undefined'
      ? undefined
      : new MutationObserver(() => {
          observeChildren();
          scheduleToolbarDensityMeasure();
        });
    mutationObserver?.observe(el, { childList: true });
    return () => {
      sizes.disconnect();
      mutationObserver?.disconnect();
      if (measureFrame) cancelAnimationFrame(measureFrame);
    };
  });
</script>

<div
  bind:this={toolbarEl}
  class="flex items-center gap-0.5 px-2.5 pb-2 pt-1"
  data-density={toolbarDensity}
  data-composer-toolbar
  data-testid="composer-toolbar"
>
  {#if hasComposableSurface}
    <!-- The model stays a control at every rung: which model answers is
         the one picker a phone user reads before sending (owner ruling,
         2026-09-04). -->
    <ModelProviderMenu {pane} />
    <!-- One real box around the other pickers (not display:contents): it
         is the child the density observer watches, so any picker's width
         move still re-measures, and it is what the minimal rung hides as
         a unit. It must NOT shrink (no min-w-0): a flex item allowed
         below its content width lets the pickers overflow it and overlap
         the meters while the toolbar's scrollWidth still reads as
         fitting, which is exactly the overlap the first phone session
         showed at the compact rung. Only an unshrinkable box pushes the
         right cluster past the edge and lets the ladder see the
         overflow. The pickers stay mounted under the roll-up, keeping
         their registry handles, so a roll-up row opens the same sheet a
         chord would. -->
    <div class="flex items-center gap-0.5 shrink-0" data-composer-toolbar-pickers>
      <EffortMenu {pane} />
      {#if !isDiscussionThread && supportsPlanMode}
        <AgentModeToggle {pane} />
      {/if}
      {#if supportsRuntimeModes}
        <AccessToggle {pane} />
      {/if}
      {#if supportsMcp}
        <McpServersTrigger {pane} />
      {/if}
      <PlanSidebarToggleButton {pane} {hasCurrentPlan} />
    </div>
    <div class="flex shrink-0 items-center" data-composer-toolbar-rollup>
      <ComposerPickersRollup
        {pane}
        showMode={!isDiscussionThread && supportsPlanMode}
        showAccess={supportsRuntimeModes}
        showMcp={supportsMcp}
        showPlan={hasCurrentPlan}
      />
    </div>
  {/if}
  <div class="ml-auto flex shrink-0 items-center gap-1.5">
    {#if showLimitRings}
      <div
        class="shrink-0 flex items-center"
        data-composer-toolbar-meter
        data-testid="composer-rate-limit-5h"
      >
        <RateLimitMeter
          windowMins={300}
          provider={providerID ?? undefined}
          accountId={currentAccountID}
          accountEmail={currentAccountEmail}
          subscriptionType={currentAccountPlan}
        />
      </div>
      <div
        class="shrink-0 flex items-center"
        data-composer-toolbar-meter
        data-testid="composer-rate-limit-7d"
      >
        <RateLimitMeter
          windowMins={10080}
          provider={providerID ?? undefined}
          accountId={currentAccountID}
          accountEmail={currentAccountEmail}
          subscriptionType={currentAccountPlan}
        />
      </div>
    {/if}
    {#if pane.contextWindow}
      <div
        class="shrink-0 flex items-center"
        data-composer-toolbar-meter
        data-testid="composer-context-meter"
      >
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
        disabledReason={sendDisabledReason}
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
  /* Reserve readable model text while measuring the expanded pickers.
     Only after they roll up may the model ellipsize into the remaining
     space. Otherwise flex shrink hides overflow by erasing the label. */
  :global([data-composer-toolbar]:not([data-density='minimal']) [data-composer-toolbar-model]) {
    flex-shrink: 0;
  }
  :global(
    [data-composer-toolbar]:not([data-density='full'])
      [data-composer-toolbar-label='collapsible']
  ) {
    display: none;
  }
  /* The minimal rung: every picker but the model folds into one roll-up
     trigger so the model, Send and the meters stay on screen at phone
     widths. The roll-up exists only there; everywhere else the pickers
     are the controls. */
  :global([data-composer-toolbar][data-density='minimal'] [data-composer-toolbar-pickers]) {
    display: none;
  }
  :global([data-composer-toolbar]:not([data-density='minimal']) [data-composer-toolbar-rollup]) {
    display: none;
  }
</style>
