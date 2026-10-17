// The open thread's Agents for the activity rail (docs/specs/standing-agents.md,
// "Agents segment"): the other threads it is waiting on, each spawn, send or
// ask it made, with the threads its spawns made nested under them. It reads
// ListThreadAgentRows when its thread opens, after its computer reconnects or
// loses a `thread:agent_rows_changed` frame, and on a frame that names its
// thread or a thread its list nests, which the backend sends after each
// change to a request or to the thread it went to. Frames about rows it
// lists that run here read those rows alone, in one read of at most
// MAX_ROW_READ (GetThreadAgentRows), and keep their places; more rows than
// that, any other frame, and a row the read says left the listing read the
// list. Nothing polls. Reads go through one rate-bounded refresh scheduler,
// so a burst of frames is one read and a row read never lands over a newer
// list.
//
// A settled row stays AGENT_ROW_SETTLED_LINGER_MS from when this pane first
// saw it settled, then leaves; the thread's wake item is the durable record.
// Clicking it dismisses it early. A settled row with an open row nested under
// it stays while that row does. Both are kept per thread for the last
// REMEMBERED_THREADS threads the pane showed, so a row that left does not
// come back when the pane returns to its thread.
//
// It also lists the incoming messages: what other threads' agents, wakes
// and workers queued for this thread and still held there, read from the
// send queue's store: the items the backend marks an agent's
// (QueueItem.agentMessage), named by their origin's kind. Each leaves when
// it is dispatched.
//
// Owned beside the background and wake controllers by the composer's rail
// host, which counts it in the rail's visibility. One controller per
// composer; it holds one list, at most the backend's 128 rows.

import { untrack } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import { GetThreadAgentRows, ListThreadAgentRows, type ThreadAgentRow } from '../../stores/bindings';
import { getQueueForThread, type QueueItem } from '../../stores/sendQueue.svelte';
import { userMessageOriginThread, userMessageOriginWake } from '../../utils/userMessageMeta';
import { onThreadAgentRowsChanged } from '../../stores/eventsThreadAgentRows';
import { getTransportStatusFor, onBackendStatusChange } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { createRefreshScheduler, type RefreshToken } from '../../utils/refreshScheduler';
import {
  agentRowCallers,
  createAgentRowMemo,
  isSettledAgentRow,
  visibleAgentRows,
} from '../../utils/agentTray';
import { AGENT_ROW_SETTLED_LINGER_MS } from './activityRailBackground.svelte';

const REFRESH_DELAY_MS = 100;
// The backend's bound on one row read (maxThreadAgentRowReads). A read of
// up to this many rows is as many statements as a read of the list, over
// those rows alone; past it the list is read.
const MAX_ROW_READ = 32;
const REFRESH_MAX_WAIT_MS = 400;
// Threads whose settled rows' first sight and dismissals the pane keeps
// after it moves to another thread; the least recently shown goes first.
const REMEMBERED_THREADS = 64;

interface SettledMemory {
  readonly seen: ReadonlyMap<string, number>;
  readonly dismissed: ReadonlySet<string>;
}

/**
 * What an incoming message is: a request from another thread (a spawn's
 * first message, a send or an ask), the reply to one this thread made, a
 * worker's question or approval, or a wake (a loop, a watch, an outside
 * send, a remote job's completion, a workflow run's wake, this thread's
 * own reminder).
 */
export type IncomingAgentKind = 'request' | 'reply' | 'question' | 'wake';

/** A message queued for the thread by an agent or a wake, with its kind. */
export interface IncomingAgentMessage {
  readonly item: QueueItem;
  readonly kind: IncomingAgentKind;
}

/** The kind of an agent's queued message, from its origin; null for the person's. */
export function incomingAgentKind(item: QueueItem): IncomingAgentKind | null {
  if (!item.agentMessage) return null;
  const thread = userMessageOriginThread(item);
  // A reminder is this thread's own wake: it names no other thread.
  if (thread) return thread.kind === 'wake' ? 'reply' : 'request';
  const wake = userMessageOriginWake(item);
  return wake?.kind === 'question' || wake?.kind === 'approval' ? 'question' : 'wake';
}

export interface AgentController {
  /** The rows the tray shows, in tree order, identity kept per token. */
  readonly rows: ThreadAgentRow[];
  /** Incoming messages, in queue order. */
  readonly incoming: IncomingAgentMessage[];
  /** Whether the backend left rows out at its bound. */
  readonly hidden: boolean;
  /** Open rows and incoming messages: the segment's count. */
  readonly count: number;
  /** Rows and incoming messages shown at all: whether the segment renders. */
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
  // What the next read reads: the list, or the rows named by frames since.
  let listDue = true;
  const rowsDue = new Set<string>();
  // The other threads' settledSeen and dismissed, in least recently shown
  // order, and the thread the state above is for.
  const remembered = new Map<string, SettledMemory>();
  let shownFor: string | null = null;

  function remember(id: string): void {
    remembered.delete(id);
    remembered.set(id, { seen: settledSeen, dismissed });
    if (remembered.size > REMEMBERED_THREADS) {
      const oldest = remembered.keys().next();
      if (!oldest.done) remembered.delete(oldest.value);
    }
  }

  // A draft pane's placeholder thread exists on no computer.
  const threadId = $derived.by(() => {
    const pane = getPane();
    return !pane || pane.hasDraftPlaceholder ? null : pane.thread?.id ?? null;
  });

  function replace(forThreadId: string | null, next: ThreadAgentRow[], more: boolean): void {
    // The wall clock, not the rail's: the rail's clock stops while nothing
    // it shows needs it, and a stamp from its last tick would end the
    // linger on the first tick after it starts again.
    const now = Date.now();
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

  // A row runs here when this computer's ledger holds its request and its
  // target: the row read answers for it. A paired computer's rows, and a
  // spawn on one with what it nests, are the list's.
  function patchable(token: string): boolean {
    const found = listed.find((row) => row.token === token);
    return !!found && !found.computerId && !found.ownerComputerId;
  }

  /**
   * Reads the rows frames named and puts each in its place, keeping what the
   * list says of its nesting. False when one left the listing, which only a
   * read of the list can place.
   */
  async function readRows(id: string, tokens: string[], token: RefreshToken): Promise<boolean> {
    if (tokens.length > MAX_ROW_READ) return false;
    let reads;
    try {
      reads = await GetThreadAgentRows(id, tokens);
    } catch (err) {
      if (token.isCurrent() && id === threadId) console.error('ActivityRail: GetThreadAgentRows failed:', err);
      return false;
    }
    if (!token.isCurrent() || id !== threadId) return true;
    const byToken = new Map<string, ThreadAgentRow>();
    for (const read of reads ?? []) {
      if (!read?.listed || !read.row) return false;
      byToken.set(read.row.token, read.row);
    }
    if (![...byToken.keys()].every(patchable)) return false;
    replace(id, listed.map((row) => {
      const read = byToken.get(row.token);
      return read ? {
        ...read, depth: row.depth, nestedHidden: row.nestedHidden,
        nestedUnavailable: row.nestedUnavailable, ownerComputerId: row.ownerComputerId,
      } : row;
    }), hidden);
    return true;
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
      if (!listDue) {
        const tokens = [...rowsDue];
        rowsDue.clear();
        if (tokens.length === 0 || await readRows(id, tokens, token)) return;
        if (!token.isCurrent() || id !== threadId) return;
      }
      listDue = false;
      rowsDue.clear();
      try {
        const agents = await ListThreadAgentRows(id);
        if (!token.isCurrent() || id !== threadId) return;
        replace(id, agents?.rows ?? [], agents?.hidden ?? false);
      } catch (err) {
        if (!token.isCurrent() || id !== threadId) return;
        // A failed read says nothing about the requests. Keep the last list
        // until a read succeeds or the thread changes; the next frame reads
        // the list.
        listDue = true;
        console.error('ActivityRail: ListThreadAgentRows failed:', err);
      }
    },
  });

  $effect(() => {
    const id = threadId;
    untrack(() => {
      if (shownFor) remember(shownFor);
      const kept = id ? remembered.get(id) : undefined;
      if (id) remembered.delete(id);
      shownFor = id;
      listDue = true;
      rowsDue.clear();
      settledSeen = kept?.seen ?? new Map();
      dismissed = kept?.dismissed ?? new Set();
      // Not replace(): an empty list would forget what kept holds.
      listed = [];
      hidden = false;
      callers = new Set();
      refresh.reset();
      refresh.request({ immediate: true });
    });
  });

  const keepRows = createAgentRowMemo();
  const rows = $derived(
    keepRows(visibleAgentRows(listed, settledSeen, dismissed, getNow(), AGENT_ROW_SETTLED_LINGER_MS)),
  );
  // Identity per queue item comes from the queue store, which replaces an
  // entry only when its snapshot does.
  const incoming = $derived.by<IncomingAgentMessage[]>(() => {
    const out: IncomingAgentMessage[] = [];
    for (const item of getQueueForThread(threadId)) {
      const kind = incomingAgentKind(item);
      if (kind) out.push({ item, kind });
    }
    return out;
  });
  const count = $derived(rows.filter((row) => !isSettledAgentRow(row)).length + incoming.length);
  const runningCount = $derived(rows.filter((row) => row.state === 'running').length);
  const hasPendingSettled = $derived(rows.some(isSettledAgentRow));

  return {
    get rows() { return rows; },
    get incoming() { return incoming; },
    get hidden() { return hidden; },
    get count() { return count; },
    get shown() { return rows.length + incoming.length; },
    get runningCount() { return runningCount; },
    get hasPendingSettled() { return hasPendingSettled; },
    get threadId() { return threadId; },

    dismiss(token: string): void {
      if (dismissed.has(token)) return;
      dismissed = new Set([...dismissed, token]);
    },

    mount(): () => void {
      // A frame about a row the list holds that runs here reads that row;
      // any other reads the list: a new row's place and the settled window
      // are the backend's.
      const cancelChanged = onThreadAgentRowsChanged((change, backend) => {
        if (!threadId) return;
        if (change === null ? threadBackend(threadId) !== backend : !callers.has(change.threadId)) return;
        if (change?.id && !listDue && patchable(change.id)) rowsDue.add(change.id);
        else listDue = true;
        if (rowsDue.size > MAX_ROW_READ) {
          listDue = true;
          rowsDue.clear();
        }
        refresh.request({ immediate: change === null });
      });
      const cancelStatus = onBackendStatusChange((backend, status) => {
        if (!threadId || threadBackend(threadId) !== backend) return;
        refresh.reset();
        listDue = true;
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
