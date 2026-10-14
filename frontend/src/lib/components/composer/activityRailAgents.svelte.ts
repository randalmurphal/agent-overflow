// The open thread's Agents for the activity rail (docs/specs/standing-agents.md,
// "Agents segment"): the other threads it is waiting on, each spawn, send or
// ask it made, with the threads its spawns made nested under them. It reads
// ListThreadAgentRows when its thread opens, after its computer reconnects or
// loses a `thread:agent_rows_changed` frame, and on every frame that names its
// thread or a thread its list nests, which the backend sends after each
// change to a request or to the thread it went to. Nothing polls. Reads go
// through the rate-bounded refresh scheduler.
//
// A settled row stays AGENT_ROW_SETTLED_LINGER_MS from when this pane first
// saw it settled, then leaves; the thread's wake item is the durable record.
// Clicking it dismisses it early. A settled row with an open row nested under
// it stays while that row does.
//
// Owned beside the background and wake controllers by the composer's rail
// host, which counts it in the rail's visibility. One controller per
// composer; it holds one list, at most the backend's 128 rows.

import { untrack } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import { ListThreadAgentRows, type ThreadAgentRow } from '../../stores/bindings';
import { onThreadAgentRowsChanged } from '../../stores/eventsThreadAgentRows';
import { getTransportStatusFor, onBackendStatusChange } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import {
  agentRowCallers,
  createAgentRowMemo,
  isSettledAgentRow,
  visibleAgentRows,
} from '../../utils/agentTray';
import { AGENT_ROW_SETTLED_LINGER_MS } from './activityRailBackground.svelte';

const REFRESH_DELAY_MS = 100;
const REFRESH_MAX_WAIT_MS = 400;

export interface AgentController {
  /** The rows the tray shows, in tree order, identity kept per token. */
  readonly rows: ThreadAgentRow[];
  /** Whether the backend left rows out at its bound. */
  readonly hidden: boolean;
  /** Open rows: the segment's count. */
  readonly count: number;
  /** Rows shown at all: whether the segment renders. */
  readonly shown: number;
  /** Rows running: the toggle's pulse and the body's `N running`. */
  readonly runningCount: number;
  /** A settled row is shown and waits on the clock to leave. */
  readonly hasPendingSettled: boolean;
  readonly threadId: string | null;
  /** Take a settled row out now, as its click does. */
  dismiss(token: string): void;
  /** Subscribe to events; returns a disposer. Call once from onMount. */
  mount(): () => void;
}

export function createAgentController(
  getPane: () => Pick<ThreadPane, 'thread' | 'hasDraftPlaceholder'> | undefined,
  getNow: () => number,
): AgentController {
  let listed: ThreadAgentRow[] = $state.raw([]);
  let hidden = $state(false);
  // When this pane first saw each settled row, and the settled rows a click
  // took out. Plain maps rewritten whole, so the rows derivation reruns.
  let settledSeen: ReadonlyMap<string, number> = $state.raw(new Map());
  let dismissed: ReadonlySet<string> = $state.raw(new Set());
  // The threads whose frames can change this list; read by the frame
  // filter, not rendered.
  let callers = new Set<string>();

  // A draft pane's placeholder thread exists on no computer.
  const threadId = $derived.by(() => {
    const pane = getPane();
    return !pane || pane.hasDraftPlaceholder ? null : pane.thread?.id ?? null;
  });

  function replace(forThreadId: string | null, next: ThreadAgentRow[], more: boolean): void {
    const now = getNow();
    const seen = new Map<string, number>();
    for (const row of next) {
      if (!isSettledAgentRow(row)) continue;
      seen.set(row.token, settledSeen.get(row.token) ?? now);
    }
    const tokens = new Set(next.map((row) => row.token));
    settledSeen = seen;
    dismissed = new Set([...dismissed].filter((token) => tokens.has(token)));
    listed = next;
    hidden = more;
    callers = forThreadId ? agentRowCallers(forThreadId, next) : new Set();
  }

  const refresh = createRefreshScheduler({
    name: 'activityRailAgents',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: async (token) => {
      const id = threadId;
      if (!id) {
        replace(null, [], false);
        return;
      }
      const owner = threadBackend(id);
      if (owner !== undefined && getTransportStatusFor(owner).status !== 'connected') return;
      try {
        const agents = await ListThreadAgentRows(id);
        if (!token.isCurrent() || id !== threadId) return;
        replace(id, agents?.rows ?? [], agents?.hidden ?? false);
      } catch (err) {
        if (!token.isCurrent() || id !== threadId) return;
        // A failed read says nothing about the requests. Keep the last list
        // until a read succeeds or the thread changes.
        console.error('ActivityRail: ListThreadAgentRows failed:', err);
      }
    },
  });

  $effect(() => {
    const id = threadId;
    untrack(() => {
      settledSeen = new Map();
      dismissed = new Set();
      replace(id, [], false);
      refresh.reset();
      refresh.request({ immediate: true });
    });
  });

  const keepRows = createAgentRowMemo();
  const rows = $derived(
    keepRows(visibleAgentRows(listed, settledSeen, dismissed, getNow(), AGENT_ROW_SETTLED_LINGER_MS)),
  );
  const count = $derived(rows.filter((row) => !isSettledAgentRow(row)).length);
  const runningCount = $derived(rows.filter((row) => row.state === 'running').length);
  const hasPendingSettled = $derived(rows.some(isSettledAgentRow));

  return {
    get rows() { return rows; },
    get hidden() { return hidden; },
    get count() { return count; },
    get shown() { return rows.length; },
    get runningCount() { return runningCount; },
    get hasPendingSettled() { return hasPendingSettled; },
    get threadId() { return threadId; },

    dismiss(token: string): void {
      if (dismissed.has(token)) return;
      dismissed = new Set([...dismissed, token]);
    },

    mount(): () => void {
      // The list is read again rather than patched with the frame's row: a
      // nested row's place and the settled window are the backend's.
      const cancelChanged = onThreadAgentRowsChanged((change, backend) => {
        if (!threadId) return;
        if (change === null ? threadBackend(threadId) !== backend : !callers.has(change.threadId)) return;
        refresh.request({ immediate: change === null });
      });
      const cancelStatus = onBackendStatusChange((backend, status) => {
        if (!threadId || threadBackend(threadId) !== backend) return;
        refresh.reset();
        if (status.status === 'connected') refresh.request({ immediate: true });
      });
      return () => {
        cancelChanged();
        cancelStatus();
        refresh.dispose();
      };
    },
  };
}
