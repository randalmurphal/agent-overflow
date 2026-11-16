<script lang="ts">
  // Expanded body of the activity rail's Agents segment
  // (docs/specs/standing-agents.md, "Agents segment"): the threads this
  // thread is waiting on, nested by spawn, then the messages queued for it
  // by other agents and wakes, then Past: the thread's settled rows, paged
  // (activityRailAgentsPast.svelte.ts). A row's Stop is StopThreadAgentRow,
  // an incoming row's StopThreadAgentIncoming, and Stop All one
  // StopThreadAgentRows call for both. Each Stop's report, every layer of
  // its cascade folded in, is one toast. Show in Sidebar is
  // SetThreadPlacement standalone on the computer listing the row.
  import { onDestroy } from 'svelte';
  import {
    SetThreadPlacement,
    StopThreadAgentIncoming,
    StopThreadAgentRow,
    StopThreadAgentRows,
    type ThreadAgentRow,
    type ThreadAgentStop,
  } from '../../stores/bindings';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import { isEmptyDraftCleanupPending, settleEmptyDraftCleanup } from '../../stores/emptyDraftCleanup';
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import { withBackendTarget } from '../../transport/backends';
  import { navigateToThreadItem } from '../../stores/threadItemNavigation';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import { errString } from '../../utils/errors';
  import { agentRowHasDigest, agentRowOpenTarget, isOpenAgentRow } from '../../utils/agentTray';
  import { agentStopReport, agentStopAllReport } from '../../utils/agentTrayStops';
  import { agentThreadOpener } from '../chat/agentThreadOrigin';
  import { agentThreadCompanionPlacer } from '../../stores/agentThreadCompanion';
  import AgentTrayRow from './AgentTrayRow.svelte';
  import AgentIncomingTrayRow from './AgentIncomingTrayRow.svelte';
  import Icon from '../primitives/Icon.svelte';
  import RowError from '../chat/RowError.svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import { activityRailIconActionClasses, activityRailRevealClasses } from './activityRailClasses';
  import type { AgentController, IncomingAgentMessage } from './activityRailAgents.svelte';

  interface Props {
    agents: AgentController;
    pane: ThreadPane;
    /** The rail's clock, Unix milliseconds. */
    now: number;
  }

  let { agents, pane, now }: Props = $props();

  let threadId = $derived(agents.threadId);
  let viewer = $derived(threadMachine(threadId ?? '', pane.thread?.projectId));

  // Rows whose digest is open (a past row's key is `past:` and its token),
  // rows whose Stop is in flight, and what a Stop could not stop, each
  // dropped when its row leaves the list; threads whose Show in Sidebar is
  // in flight.
  let expanded = $state<Set<string>>(new Set());
  let placing = $state<Set<string>>(new Set());
  let past = $derived(agents.past);
  const pastKey = (row: ThreadAgentRow) => `past:${row.token}`;
  let stopping = $state<Set<string>>(new Set());
  let stopErrors = $state<Map<string, string>>(new Map());
  let stopAllInFlight = $state(false);

  let liveKeys = $derived(new Set([
    ...agents.rows.map((row) => row.token),
    ...agents.incoming.map((message) => message.item.sendId ?? message.item.id),
    ...past.rows.map(pastKey),
  ]));
  // Past holds its pages only while it is shown.
  onDestroy(() => agents.past.collapse());
  $effect(() => {
    const keys = liveKeys;
    if ([...expanded].some((key) => !keys.has(key))) expanded = new Set([...expanded].filter((key) => keys.has(key)));
    if ([...stopping].some((key) => !keys.has(key))) stopping = new Set([...stopping].filter((key) => keys.has(key)));
    if ([...stopErrors.keys()].some((key) => !keys.has(key))) {
      stopErrors = new Map([...stopErrors].filter(([key]) => keys.has(key)));
    }
  });

  function mark(key: string, on: boolean): void {
    const next = new Set(stopping);
    if (on) next.add(key);
    else next.delete(key);
    stopping = next;
  }

  function setStopError(key: string, error: string | null): void {
    const next = new Map(stopErrors);
    if (error) next.set(key, error);
    else next.delete(key);
    stopErrors = next;
  }

  function toggleExpanded(key: string): void {
    const next = new Set(expanded);
    if (!next.delete(key)) next.add(key);
    expanded = next;
  }
  const onToggleExpanded = (row: ThreadAgentRow) => toggleExpanded(row.token);
  const onTogglePastExpanded = (row: ThreadAgentRow) => toggleExpanded(pastKey(row));

  // Made on each click, so a thread read after the pane moved on opens
  // nowhere rather than beside another thread.
  const placeInCompanion = () => agentThreadCompanionPlacer(pane);

  // An open row opens the thread it went to in this pane's agent thread
  // companion, an ask its copy. One no longer open shows where its answer
  // landed: this thread's wake item when the answer came here, leaving the
  // tray; for an ask, whose copy is gone, its digest with the answer;
  // otherwise the thread it went to, in the companion, leaving the tray.
  function activate(row: ThreadAgentRow, key: string): void {
    if (!threadId) return;
    if (!isOpenAgentRow(row)) {
      if (row.wakeItemId && row.callerThreadId === threadId) {
        agents.dismiss(row.token);
        void navigateToThreadItem(pane, threadId, row.wakeItemId);
        return;
      }
      if (agentRowOpenTarget(row)?.asked && agentRowHasDigest(row)) {
        if (!expanded.has(key)) toggleExpanded(key);
        return;
      }
      agents.dismiss(row.token);
    }
    onOpenPane(row);
  }
  const onActivate = (row: ThreadAgentRow) => activate(row, row.token);
  const onActivatePast = (row: ThreadAgentRow) => activate(row, pastKey(row));

  // Open in Agent Pane: the thread the row went to beside this one, open or
  // settled, archived included; for an ask its copy while it runs, then the
  // thread asked (agentRowOpenTarget). A click that races the copy's
  // delete finds it gone and says so; the row's next read names no copy.
  function onOpenPane(row: ThreadAgentRow): void {
    const target = agentRowOpenTarget(row);
    if (!target) {
      addToast('info', `${row.title} has no thread yet.`);
      return;
    }
    const title = row.kind === 'ask' && !target.asked ? `the copy asking ${row.title}` : row.title;
    const opener = agentThreadOpener(
      { threadId: target.threadId, computerId: row.computerId ?? '', computerName: row.computer ?? '', title, token: row.token, kind: '' },
      viewer,
      placeInCompanion,
    );
    if (!opener.open) {
      addToast('info', `${row.title} is on ${row.computer || 'a computer'} this window is not connected to.`);
      return;
    }
    opener.open();
  }

  async function onShowInSidebar(row: ThreadAgentRow): Promise<void> {
    const id = row.threadId;
    if (!id || placing.has(id)) return;
    placing = new Set([...placing, id]);
    try {
      const rows = (await withBackendTarget(viewer, () => SetThreadPlacement([id], 'standalone'))) as Thread[] | null;
      agents.placed(id, rows?.find((thread) => thread.id === id)?.placement ?? 'standalone');
    } catch (err) {
      addErrorToast(`Could not show ${row.title} in the sidebar: ${errString(err)}`, err);
    } finally {
      const next = new Set(placing);
      next.delete(id);
      placing = next;
    }
  }

  function report(title: string, stop: ThreadAgentStop): void {
    const { tone, text, firstFailure } = agentStopReport(title, stop);
    if (firstFailure) setStopError(stop.token, firstFailure);
    addToast(tone, text);
  }

  // A Stop writes this thread's rows, so it first waits out an emptied
  // draft's in-flight delete of it, and does nothing if that deleted it.
  // Show in Sidebar writes a worker's row, which a delete of an empty draft
  // never takes.
  async function removedByDraftCleanup(id: string): Promise<boolean> {
    return isEmptyDraftCleanupPending(id) && await settleEmptyDraftCleanup(id);
  }

  async function onStop(row: ThreadAgentRow): Promise<void> {
    if (!threadId) return;
    mark(row.token, true);
    setStopError(row.token, null);
    try {
      if (await removedByDraftCleanup(threadId)) return;
      report(row.title, await StopThreadAgentRow(threadId, row.token));
    } catch (err) {
      addErrorToast(`Could not stop ${row.title}: ${errString(err)}`, err);
    } finally {
      mark(row.token, false);
    }
  }

  async function onStopIncoming(message: IncomingAgentMessage): Promise<void> {
    const sendId = message.item.sendId;
    if (!threadId || !sendId) return;
    mark(sendId, true);
    try {
      if (await removedByDraftCleanup(threadId)) return;
      report(`the incoming ${message.kind}`, await StopThreadAgentIncoming(threadId, sendId));
    } catch (err) {
      addErrorToast(`Could not withdraw the incoming ${message.kind}: ${errString(err)}`, err);
    } finally {
      mark(sendId, false);
    }
  }

  async function onStopAll(): Promise<void> {
    if (!threadId) return;
    const titles = new Map(agents.rows.map((row) => [row.token, row.title]));
    stopAllInFlight = true;
    try {
      if (await removedByDraftCleanup(threadId)) return;
      const stops = (await StopThreadAgentRows(threadId)) ?? [];
      for (const stop of stops) {
        const failure = agentStopReport(titles.get(stop.token) ?? '', stop).firstFailure;
        if (failure) setStopError(stop.token, failure);
      }
      const { tone, text } = agentStopAllReport(stops);
      addToast(tone, text);
    } catch (err) {
      addErrorToast(`Could not stop the agents: ${errString(err)}`, err);
    } finally {
      stopAllInFlight = false;
    }
  }

  let canStopAll = $derived(agents.count > 0);
</script>

{#snippet readError(testId: string, msg: string, retryLabel: string, onRetry: () => void)}
  <li class="flex items-center gap-1 px-1" data-testid={testId}>
    <RowError tone="error" msg={msg} class="flex-1" />
    <button
      type="button"
      class={activityRailIconActionClasses}
      onclick={onRetry}
      title="Retry"
      aria-label={retryLabel}
      data-testid="{testId}-retry"
    >
      <Icon icon={RefreshCw} size={12} />
    </button>
  </li>
{/snippet}

<div
  id="activity-rail-agents-body"
  class="border-t border-border-subtle px-3 py-2"
  data-testid="activity-rail-agents-body"
>
  <div class="mb-1.5 flex items-center gap-2 font-mono text-[0.65625rem] text-fg-hint/70">
    <span data-testid="activity-rail-agents-running-label">
      {agents.runningCount > 0 ? `${agents.runningCount} running` : agents.count > 0 ? 'waiting' : 'idle'}
    </span>
    {#if canStopAll}
      <button
        type="button"
        class="ml-auto rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/60 px-2 py-0.5 text-[0.6875rem] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50 compact:min-h-9 compact:select-none"
        onclick={onStopAll}
        disabled={stopAllInFlight}
        data-testid="activity-rail-agents-stop-all"
        aria-label="Stop All Agents"
      >
        {stopAllInFlight ? 'Stopping…' : 'Stop All'}
      </button>
    {/if}
  </div>
  <ul class="flex flex-col gap-1 overflow-y-auto {expanded.size > 0 || past.expanded ? 'max-h-[min(70vh,32rem)]' : 'max-h-56'}">
    {#if agents.error}
      {@render readError('activity-rail-agents-error', `Could not read agents: ${agents.error}`, 'Retry reading agents', () => agents.retry())}
    {/if}
    {#each agents.rows as row (row.token)}
      <li>
        <AgentTrayRow
          {row}
          {now}
          isStopping={stopping.has(row.token) || (stopAllInFlight && isOpenAgentRow(row))}
          stopError={stopErrors.get(row.token)}
          expanded={expanded.has(row.token)}
          isPlacing={!!row.threadId && placing.has(row.threadId)}
          {onToggleExpanded}
          {onActivate}
          {onOpenPane}
          {onShowInSidebar}
          {onStop}
        />
      </li>
    {/each}
    {#if agents.hidden}
      <li class="px-2 py-0.5 text-[0.6875rem] text-fg-hint" data-testid="activity-rail-agents-more">
        More agents than the tray lists. Open a thread to see its own.
      </li>
    {/if}
    {#each agents.incoming as message (message.item.id)}
      <li>
        <AgentIncomingTrayRow
          {message}
          {viewer}
          {now}
          isStopping={stopping.has(message.item.sendId ?? '') || stopAllInFlight}
          onStop={onStopIncoming}
          place={placeInCompanion}
        />
      </li>
    {/each}
    {#if past.total > 0}
      <li>
        <button
          type="button"
          class="flex w-full items-center gap-2 rounded-[var(--radius-control)] px-2 py-1 text-left text-[0.6875rem] text-fg-hint transition-colors hover:bg-surface-2/20 hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none"
          onclick={() => past.toggle()}
          aria-expanded={past.expanded}
          aria-controls="activity-rail-agents-past"
          data-testid="activity-rail-agents-past-toggle"
        >
          <span class="flex size-3.5 shrink-0 items-center justify-center" class:rotate-90={past.expanded}><Icon icon={ChevronRight} size={12} strokeWidth={2} /></span>
          <span class="flex-1">Past</span>
          <span class="shrink-0 min-w-5 text-right text-[0.625rem] tabular-nums text-fg-hint" data-testid="activity-rail-agents-past-count">{past.total}</span>
        </button>
      </li>
    {/if}
    {#if past.expanded}
      <li id="activity-rail-agents-past" data-testid="activity-rail-agents-past">
        <ul class="flex flex-col gap-1">
          {#each past.rows as row (row.token)}
            <li>
              <AgentTrayRow
                {row}
                {now}
                {viewer}
                past
                isStopping={false}
                expanded={expanded.has(pastKey(row))}
                isPlacing={!!row.threadId && placing.has(row.threadId)}
                onToggleExpanded={onTogglePastExpanded}
                onActivate={onActivatePast}
                {onOpenPane}
                {onShowInSidebar}
                {onStop}
              />
            </li>
          {/each}
        </ul>
        {#if past.moreCount > 0}
          <button
            type="button"
            class="ml-6 {activityRailRevealClasses}"
            onclick={() => past.showMore()}
            disabled={past.loadingMore}
            data-testid="activity-rail-agents-past-more"
          >
            Show {past.moreCount} more…
          </button>
        {/if}
      </li>
    {/if}
    {#if past.error}
      {@render readError('activity-rail-agents-past-error', `Could not read past agents: ${past.error}`, 'Retry reading past agents', () => past.retry())}
    {/if}
  </ul>
</div>
