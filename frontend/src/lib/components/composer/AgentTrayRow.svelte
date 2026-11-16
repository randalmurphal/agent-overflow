<script lang="ts">
  // One row of the activity rail's Agents body (docs/specs/standing-agents.md,
  // "Agents segment"): a thread this thread is waiting on, or under Past one
  // it waited on, with its provider, title, state, how long a spawned
  // thread's turns ran (agentRowRuntimeMs; any other row: how long since the
  // request; a past row: when it settled, the time it ran its tooltip), what
  // a spawned thread did (its tool calls and tokens, as a subagent's row
  // counts them), and what it is blocked on or did last. A row
  // is open while its request is, or its worker is not at rest; once its
  // request settled it shows its worker's state. The row's main button
  // opens the thread, or for a row no longer open the answer; the
  // chevron opens the row's digest beneath it; Show in Sidebar gives an
  // owned worker a sidebar row; Open in Agent Pane opens the thread beside;
  // Stop, on an open row, stops the request and, for a spawn, what the
  // spawned thread runs. The body owns dispatch.
  import { untrack } from 'svelte';
  import type { ThreadAgentRow } from '../../stores/bindings';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { formatElapsed } from '../../utils/backgroundTray';
  import { relativeTime } from '../../utils/format';
  import type { BackendKey } from '../../transport/backendKey';
  import {
    AGENT_ROW_MAX_INDENT,
    agentRowBlockedLabel,
    agentRowHasDigest,
    agentRowOpenTarget,
    agentRowRuntimeMs,
    agentRowShownState,
    agentRowStateLabel,
    isOpenAgentRow,
  } from '../../utils/agentTray';
  import Icon from '../primitives/Icon.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';
  import RowError from '../chat/RowError.svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import PanelLeftOpen from '@lucide/svelte/icons/panel-left-open';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import UsersRound from '@lucide/svelte/icons/users-round';
  import AgentTrayRowDigest from './AgentTrayRowDigest.svelte';
  import AgentMetrics from './AgentMetrics.svelte';
  import { activityRailIconActionClasses } from './activityRailClasses';

  interface Props {
    row: ThreadAgentRow;
    /** The rail's clock, Unix milliseconds. */
    now: number;
    isStopping: boolean;
    /** Why the last Stop could not stop all of it, until the row settles. */
    stopError?: string;
    expanded: boolean;
    /** A row under Past: settled, never stopped, dated by when it settled. */
    past?: boolean;
    /** The computer listing the row, whose clock dates a past row. */
    viewer?: BackendKey;
    /** Show in Sidebar is in flight. */
    isPlacing?: boolean;
    onToggleExpanded: (row: ThreadAgentRow) => void;
    onActivate: (row: ThreadAgentRow) => void;
    onOpenPane: (row: ThreadAgentRow) => void;
    onShowInSidebar: (row: ThreadAgentRow) => void;
    onStop: (row: ThreadAgentRow) => void;
  }

  let {
    row, now, isStopping, stopError, expanded, past = false, viewer, isPlacing = false,
    onToggleExpanded, onActivate, onOpenPane, onShowInSidebar, onStop,
  }: Props = $props();

  let open = $derived(isOpenAgentRow(row));
  let shownState = $derived(agentRowShownState(row));
  let stateLabel = $derived(agentRowStateLabel(row));
  let blockedLabel = $derived(shownState === 'blocked' ? agentRowBlockedLabel(row) : '');
  // The rail's clock when this row's runtime was read: a new read of the
  // row restarts the open turn's local count from it.
  let readAt = $derived.by(() => {
    void row.runtimeMs;
    void row.turnOpen;
    return untrack(() => now);
  });
  // The most this row has shown, so no read shows its runtime going back.
  let shownRuntime = 0;
  let runtime = $derived.by(() => {
    const ms = agentRowRuntimeMs(row, now, readAt);
    if (ms === null) return null;
    shownRuntime = Math.max(shownRuntime, ms);
    return shownRuntime;
  });
  let elapsed = $derived(formatElapsed(runtime ?? Math.max(0, (!open && row.settledAt ? row.settledAt : now) - row.createdAt)));
  // A past row says when it settled; reading `now` keeps the age current.
  let settledAge = $derived.by(() => {
    if (!past || !row.settledAt) return '';
    void now;
    return relativeTime(row.settledAt, 'locale', viewer);
  });
  // A worker this computer spawned that has no sidebar row.
  let showInSidebar = $derived(row.kind === 'spawn' && row.placement === 'owned' && !row.archived && !row.computerId && Boolean(row.threadId));
  let activity = $derived(open && !blockedLabel ? (row.activity ?? '') : '');
  // What the spawned thread did, live while it works and final once it
  // rests; unknown from a computer that does not report it.
  let toolUses = $derived(row.toolCalls ? row.toolCalls : null);
  let tokens = $derived(row.tokens ? row.tokens : null);
  let hasMetrics = $derived(toolUses !== null || tokens !== null);
  let stateTone = $derived.by(() => {
    switch (shownState) {
      case 'blocked':
        return 'text-warning';
      case 'errored':
      case 'refused':
      case 'expired':
      case 'interrupted':
        return 'text-error';
      case 'cancelled':
        return 'text-fg-hint';
      case 'running':
        return 'text-accent';
      default:
        return open ? 'text-fg-hint' : 'text-success';
    }
  });
  let indent = $derived(Math.min(Math.max(0, row.depth), AGENT_ROW_MAX_INDENT) * 0.75);
  let hasDigest = $derived(agentRowHasDigest(row));
  let digestId = $derived(`agent-tray-row-digest-${past ? 'past-' : ''}${row.token}`);
  // An ask's row is titled with the thread asked. While the ask runs it
  // opens the ask's copy, its worker; once the copy is gone it shows the
  // answer, and offers the thread asked only as that.
  let target = $derived(agentRowOpenTarget(row));
  let asked = $derived(target?.asked === true);
  let openLabel = $derived(
    !open && (row.wakeItemId || (asked && hasDigest)) ? `Show the answer from ${row.title}`
      : asked ? `Open ${row.title}, the thread asked`
      : row.kind === 'ask' ? `Open the copy asking ${row.title}`
      : `Open ${row.title}`,
  );
  let openPaneTitle = $derived(asked ? 'Open the thread asked in agent pane' : 'Open in agent pane');
  let openPaneLabel = $derived(asked ? `Open ${row.title}, the thread asked, in Agent Pane` : 'Open in Agent Pane');
  // A spawn on a computer that cannot list or stop what it runs says so
  // wherever the row is read: here, and in the Stop's toast.
  let degraded = $derived(row.nestedUnavailable ? `Its own agents and background work are not shown: ${row.nestedUnavailable}` : '');
  // A paired computer whose version settles a spawn when its turn ends.
  let restUnreported = $derived(row.restUnreported
    ? `${row.computer || 'That computer'} runs an older version: it reported this when the turn ended and cannot say whether the thread came to rest`
    : '');
  let compact = $derived(isCompactLayout());

  const actionClasses =
    'shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[0.6875rem] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50 compact:min-h-9 compact:select-none';
</script>

{#snippet facts()}
  <span class="shrink-0 font-mono text-[0.6875rem] {stateTone}" data-testid="agent-tray-row-state">{stateLabel}</span>
  {#if settledAge}
    <span class="shrink-0 font-mono text-[0.6875rem] tabular-nums text-fg-hint" title={`Ran ${elapsed}`} data-testid="agent-tray-row-elapsed">{settledAge}</span>
  {:else}
    <span class="shrink-0 font-mono text-[0.6875rem] tabular-nums text-fg-hint" data-testid="agent-tray-row-elapsed">{elapsed}</span>
  {/if}
{/snippet}

{#snippet actions()}
  <span class="flex shrink-0 items-center gap-1">
    {#if hasDigest}
      <button
        type="button"
        class="inline-flex shrink-0 rounded p-0.5 text-fg-subtle hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:min-w-9 compact:items-center compact:justify-center"
        onclick={() => onToggleExpanded(row)}
        aria-expanded={expanded}
        aria-controls={digestId}
        aria-label={`Details: ${row.title}`}
        title="Details"
        data-testid="agent-tray-row-details"
      >
        <span class="flex" class:rotate-90={expanded}><Icon icon={ChevronRight} size={12} strokeWidth={2} /></span>
      </button>
    {/if}
    {#if showInSidebar}
      <button
        type="button"
        class={activityRailIconActionClasses}
        onclick={() => onShowInSidebar(row)}
        disabled={isPlacing}
        title="Show in sidebar"
        aria-label="Show in Sidebar"
        data-testid="agent-tray-row-show-in-sidebar"
      >
        <Icon icon={PanelLeftOpen} size={12} />
      </button>
    {:else}
      <!-- Holds Show in Sidebar's place, so every row's facts line up. -->
      <span class="size-4 shrink-0 compact:hidden" aria-hidden="true"></span>
    {/if}
    <button
      type="button"
      class={activityRailIconActionClasses}
      onclick={() => onOpenPane(row)}
      title={openPaneTitle}
      aria-label={openPaneLabel}
      data-testid="agent-tray-row-open-pane"
    >
      <Icon icon={PanelRightOpen} size={12} />
    </button>
    {#if open && !past}
      <button
        type="button"
        class={actionClasses}
        onclick={() => onStop(row)}
        disabled={isStopping}
        title={degraded || undefined}
        aria-label={`Stop: ${row.title}`}
        data-testid="agent-tray-row-stop"
      >{isStopping ? 'Stopping…' : 'Stop'}</button>
    {/if}
  </span>
{/snippet}

<div
  class="rounded-[var(--radius-control)] border border-border-subtle bg-transparent px-1 py-1"
  style={indent > 0 ? `margin-left: ${indent}rem` : undefined}
  title={degraded || undefined}
  data-testid="agent-tray-row"
  data-token={row.token}
  data-kind={row.kind}
  data-state={row.state}
  data-depth={row.depth}
  data-settled={open ? undefined : 'true'}
  data-past={past ? 'true' : undefined}
>
  <div class="flex w-full items-center gap-2 compact:gap-1">
    <button
      type="button"
      class="flex min-w-0 flex-1 cursor-pointer items-center gap-2 rounded-[var(--radius-control)] bg-transparent px-1 py-0.5 text-left hover:bg-surface-2/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
      onclick={() => onActivate(row)}
      aria-label={openLabel}
      data-testid="agent-tray-row-open"
    >
      <span class="flex size-3.5 shrink-0 items-center justify-center text-fg-hint" data-testid="agent-tray-row-provider" data-provider={row.provider ?? ''}>
        {#if row.provider}
          <ProviderIcon provider={row.provider} size={13} />
        {:else}
          <Icon icon={UsersRound} size={13} strokeWidth={2} />
        {/if}
      </span>
      <span class="w-9 shrink-0 truncate text-[0.6875rem] text-fg-hint" data-testid="agent-tray-row-kind">{row.kind}</span>
      {#if compact}
        <span class="line-clamp-2 min-w-0 flex-1 text-[0.75rem] text-fg-muted [overflow-wrap:anywhere]" data-testid="agent-tray-row-title">{row.title}</span>
      {:else}
        <span class="min-w-8 flex-1 truncate text-[0.75rem] text-fg-muted" title={row.title} data-testid="agent-tray-row-title">{row.title}</span>
      {/if}
      {#if row.computer}
        <span class="max-w-32 shrink truncate text-[0.6875rem] text-fg-hint" title={`On ${row.computer}`} data-testid="agent-tray-row-computer">on {row.computer}</span>
      {/if}
      {#if !compact}{@render facts()}{/if}
    </button>
    {#if !compact}{@render actions()}{/if}
  </div>

  {#if compact}
    <div class="flex items-center gap-2 pb-1 pl-6 pr-1" data-testid="agent-tray-row-controls">
      {@render facts()}
      <span class="ml-auto">{@render actions()}</span>
    </div>
  {/if}

  {#if hasMetrics || blockedLabel || activity}
    <p class="flex min-w-0 items-center gap-1 pb-0.5 pl-6 pr-1 text-[0.6875rem] text-fg-hint/85" data-testid="agent-tray-row-details-line">
      {#if hasMetrics}
        <span class="flex shrink-0 items-center gap-1 tabular-nums" data-testid="agent-tray-row-metrics">
          <AgentMetrics {toolUses} {tokens} testIdPrefix="agent-tray-row" />
        </span>
        {#if blockedLabel || activity}<span aria-hidden="true">·</span>{/if}
      {/if}
      {#if blockedLabel}
        <span class="min-w-0 truncate text-warning" title={blockedLabel} data-testid="agent-tray-row-blocked">{blockedLabel}</span>
      {:else if activity}
        <span class="min-w-0 truncate" title={activity} data-testid="agent-tray-row-activity">{activity}</span>
      {/if}
    </p>
  {/if}
  {#if degraded}
    <p class="truncate pb-0.5 pl-6 pr-1 text-[0.6875rem] text-warning" title={degraded} data-testid="agent-tray-row-degraded">{degraded}</p>
  {:else if row.nestedHidden}
    <p class="pb-0.5 pl-6 pr-1 text-[0.6875rem] text-fg-hint" data-testid="agent-tray-row-nested-hidden">Its own agents nest deeper than the tray shows. Open it to see them.</p>
  {/if}
  {#if restUnreported}
    <p class="truncate pb-0.5 pl-6 pr-1 text-[0.6875rem] text-fg-hint" title={restUnreported} data-testid="agent-tray-row-rest-unreported">{restUnreported}</p>
  {/if}
  {#if stopError}
    <div class="px-1 pt-0.5" data-testid="agent-tray-row-stop-error">
      <RowError tone="error" msg={`Stop: ${stopError}`} />
    </div>
  {/if}
  {#if expanded && hasDigest}
    <AgentTrayRowDigest {row} id={digestId} />
  {/if}
</div>
