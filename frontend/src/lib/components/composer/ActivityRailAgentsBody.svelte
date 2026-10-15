<script lang="ts">
  // Expanded body of the activity rail's Agents segment
  // (docs/specs/standing-agents.md, "Agents segment"): the threads this
  // thread is waiting on, nested by spawn, then the messages queued for it
  // by other agents and wakes. A row's Stop is StopThreadAgentRow, an
  // incoming row's StopThreadAgentIncoming, and Stop All one
  // StopThreadAgentRows call for both. Each Stop's report, every layer of
  // its cascade folded in, is one toast.
  import {
    StopThreadAgentIncoming,
    StopThreadAgentRow,
    StopThreadAgentRows,
    type ThreadAgentRow,
    type ThreadAgentStop,
  } from '../../stores/bindings';
  import { addErrorToast, addToast } from '../../stores/toast.svelte';
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import { navigateToThreadItem } from '../../stores/threadItemNavigation';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { errString } from '../../utils/errors';
  import { isSettledAgentRow } from '../../utils/agentTray';
  import { agentStopReport, agentStopAllReport } from '../../utils/agentTrayStops';
  import { agentThreadOriginChip } from '../chat/agentThreadOrigin';
  import AgentTrayRow from './AgentTrayRow.svelte';
  import AgentIncomingTrayRow from './AgentIncomingTrayRow.svelte';
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

  // Rows whose digest is open, rows whose Stop is in flight, and what a
  // Stop could not stop, each dropped when its row leaves the list.
  let expanded = $state<Set<string>>(new Set());
  let stopping = $state<Set<string>>(new Set());
  let stopErrors = $state<Map<string, string>>(new Map());
  let stopAllInFlight = $state(false);

  let liveKeys = $derived(new Set([
    ...agents.rows.map((row) => row.token),
    ...agents.incoming.map((message) => message.item.sendId ?? message.item.id),
  ]));
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

  function onToggleExpanded(row: ThreadAgentRow): void {
    const next = new Set(expanded);
    if (!next.delete(row.token)) next.add(row.token);
    expanded = next;
  }

  // An open row opens the thread it went to. A settled one leaves the tray
  // and shows where its answer landed: this thread's wake item when the
  // answer came here, otherwise that thread.
  function onActivate(row: ThreadAgentRow): void {
    if (!threadId) return;
    if (isSettledAgentRow(row)) {
      agents.dismiss(row.token);
      if (row.wakeItemId && row.callerThreadId === threadId) {
        void navigateToThreadItem(pane, threadId, row.wakeItemId);
        return;
      }
    }
    const target = row.openThreadId || row.threadId;
    if (!target) {
      addToast('info', `${row.title} has no thread yet.`);
      return;
    }
    const chip = agentThreadOriginChip(
      { threadId: target, computerId: row.computerId ?? '', computerName: row.computer ?? '', title: row.title, token: row.token },
      viewer,
    );
    if (!chip.open) {
      addToast('info', `${row.title} is on ${row.computer || 'a computer'} this window is not connected to.`);
      return;
    }
    chip.open();
  }

  function report(title: string, stop: ThreadAgentStop): void {
    const { tone, text, firstFailure } = agentStopReport(title, stop);
    if (firstFailure) setStopError(stop.token, firstFailure);
    addToast(tone, text);
  }

  async function onStop(row: ThreadAgentRow): Promise<void> {
    if (!threadId) return;
    mark(row.token, true);
    setStopError(row.token, null);
    try {
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
  <ul class="flex flex-col gap-1 overflow-y-auto {expanded.size > 0 ? 'max-h-[min(70vh,32rem)]' : 'max-h-56'}">
    {#each agents.rows as row (row.token)}
      <li>
        <AgentTrayRow
          {row}
          {now}
          isStopping={stopping.has(row.token) || (stopAllInFlight && !isSettledAgentRow(row))}
          stopError={stopErrors.get(row.token)}
          expanded={expanded.has(row.token)}
          {onToggleExpanded}
          {onActivate}
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
        />
      </li>
    {/each}
  </ul>
</div>
