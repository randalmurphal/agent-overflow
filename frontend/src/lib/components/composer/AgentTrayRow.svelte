<script lang="ts">
  // One row of the activity rail's Agents body (docs/specs/standing-agents.md,
  // "Agents segment"): a thread this thread is waiting on, with its provider,
  // title, state, how long since the request, and what it is blocked on. The
  // row's main button opens the thread, or for a settled row the answer; the
  // chevron opens the row's digest beneath it; Stop stops the request and,
  // for a spawn, what the spawned thread runs. The body owns dispatch.
  import type { ThreadAgentRow } from '../../stores/bindings';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { formatElapsed } from '../../utils/backgroundTray';
  import {
    AGENT_ROW_MAX_INDENT,
    agentRowBlockedLabel,
    agentRowStateLabel,
    isSettledAgentRow,
  } from '../../utils/agentTray';
  import Icon from '../primitives/Icon.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';
  import RowError from '../chat/RowError.svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import UsersRound from '@lucide/svelte/icons/users-round';
  import AgentTrayRowDigest from './AgentTrayRowDigest.svelte';

  interface Props {
    row: ThreadAgentRow;
    /** The rail's clock, Unix milliseconds. */
    now: number;
    isStopping: boolean;
    /** Why the last Stop could not stop all of it, until the row settles. */
    stopError?: string;
    expanded: boolean;
    onToggleExpanded: (row: ThreadAgentRow) => void;
    onActivate: (row: ThreadAgentRow) => void;
    onStop: (row: ThreadAgentRow) => void;
  }

  let { row, now, isStopping, stopError, expanded, onToggleExpanded, onActivate, onStop }: Props = $props();

  let settled = $derived(isSettledAgentRow(row));
  let stateLabel = $derived(agentRowStateLabel(row));
  let blockedLabel = $derived(row.state === 'blocked' ? agentRowBlockedLabel(row) : '');
  let elapsed = $derived(formatElapsed(Math.max(0, (settled && row.settledAt ? row.settledAt : now) - row.createdAt)));
  let activity = $derived(!settled && !blockedLabel ? (row.activity ?? '') : '');
  let stateTone = $derived.by(() => {
    switch (row.state) {
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
        return settled ? 'text-success' : 'text-fg-hint';
    }
  });
  let indent = $derived(Math.min(Math.max(0, row.depth), AGENT_ROW_MAX_INDENT) * 0.75);
  let hasDigest = $derived(Boolean(row.activity || row.blockedDetail || row.lastMessage || row.answer || row.nestedUnavailable));
  let digestId = $derived(`agent-tray-row-digest-${row.token}`);
  let openLabel = $derived(settled && row.wakeItemId ? `Show the answer from ${row.title}` : `Open ${row.title}`);
  // A spawn on a computer that cannot list or stop what it runs says so
  // wherever the row is read: here, and in the Stop's toast.
  let degraded = $derived(row.nestedUnavailable ? `Its own agents and background work are not shown: ${row.nestedUnavailable}` : '');
  let compact = $derived(isCompactLayout());

  const actionClasses =
    'shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[0.6875rem] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50 compact:min-h-9 compact:select-none';
</script>

{#snippet facts()}
  <span class="shrink-0 font-mono text-[0.6875rem] {stateTone}" data-testid="agent-tray-row-state">{stateLabel}</span>
  <span class="shrink-0 font-mono text-[0.6875rem] tabular-nums text-fg-hint" data-testid="agent-tray-row-elapsed">{elapsed}</span>
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
    {#if !settled}
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
  data-settled={settled ? 'true' : undefined}
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

  {#if blockedLabel}
    <p class="truncate pb-0.5 pl-6 pr-1 text-[0.6875rem] text-warning" title={blockedLabel} data-testid="agent-tray-row-blocked">{blockedLabel}</p>
  {:else if activity}
    <p class="truncate pb-0.5 pl-6 pr-1 text-[0.6875rem] text-fg-hint/85" title={activity} data-testid="agent-tray-row-activity">{activity}</p>
  {/if}
  {#if degraded}
    <p class="truncate pb-0.5 pl-6 pr-1 text-[0.6875rem] text-warning" title={degraded} data-testid="agent-tray-row-degraded">{degraded}</p>
  {:else if row.nestedHidden}
    <p class="pb-0.5 pl-6 pr-1 text-[0.6875rem] text-fg-hint" data-testid="agent-tray-row-nested-hidden">Its own agents nest deeper than the tray shows. Open it to see them.</p>
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
