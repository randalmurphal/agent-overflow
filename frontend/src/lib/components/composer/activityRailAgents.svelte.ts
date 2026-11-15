// The open thread's Agents for the activity rail (docs/specs/standing-agents.md,
// "Agents segment"): the other threads it is waiting on, each spawn, send or
// ask it made, with the threads its spawns made nested under them. It reads
// ListThreadAgentRows when its thread opens, after its computer reconnects or
// loses a `thread:agent_rows_changed` frame, and on a frame that names its
// thread or a thread its list nests, which the backend sends after each
// change to a request or to the thread it went to; a frame that lands during
// a list read about a thread the answer nests reads the list again, since
// the answer may predate it. Frames about rows it
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
// Below the open rows is Past (activityRailAgentsPast.svelte.ts), the
// thread's settled rows from the ledger. This controller says when it may
// have changed: on the reads above, a frame naming the thread and no row the
// list holds open or settled, a row of the thread's own that the list shows
// settling, and a frame about a settled row the next read did not supply.
// A settled row of the thread's own that a read supplies is patched into
// Past, and while Past lists it the open rows leave it out. The segment
// shows while Past holds any row, so it stays reachable.
//
// A failed list read keeps the last list and is the controller's `error`
// until a read succeeds; the segment shows while it stands, with a Retry.
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
import { createPastAgentRows, type AgentReadStatus, type PastAgentRows } from './activityRailAgentsPast.svelte';
import { errString } from '../../utils/errors';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';

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
  /** Rows, incoming messages and the Past row shown at all: whether the
   *  segment renders. */
  readonly shown: number;
  /** The thread's settled rows, under Past. */
  readonly past: PastAgentRows;
  /** Rows running: the toggle's pulse and the body's `N running`. */
  readonly runningCount: number;
  /** A settled row is shown and waits on the clock to leave. */
  readonly hasPendingSettled: boolean;
  readonly threadId: string | null;
  /** Whether a list read answered for the thread, and whether the last failed. */
  readonly status: AgentReadStatus;
  /** Why the last list read failed, until one succeeds. */
  readonly error: string;
  /** Read the list again after a failure. */
  retry(): void;
  /** Take a settled row out now, as its click does. */
  dismiss(token: string): void;
  /** A row's thread got a new sidebar placement (Show in Sidebar). */
  placed(threadId: string, placement: string): void;
  /** Subscribe to events; returns a disposer. Call once from onMount. */
  mount(): () => void;
}

export function createAgentController(
  getPane: () => Pick<ThreadPane, 'thread' | 'hasDraftPlaceholder'> | undefined,
  getNow: () => number,
): AgentController {
  let listed: ThreadAgentRow[] = $state.raw([]);
  let hidden = $state(false);
  // Whether a list read answered for this thread, and why the last one
  // failed, until one succeeds.
  let listLoaded = $state(false);
  let listError = $state('');
  // Settled rows frames named that a read of the open list answers for:
  // Past takes each from that read, or reads it when the read does not.
  const pastNamed = new Set<string>();
  // When this pane first saw each settled row, and the settled rows a click
  // took out. Plain maps rewritten whole, so the rows derivation reruns.
  let settledSeen: ReadonlyMap<string, number> = $state.raw(new Map());
  let dismissed: ReadonlySet<string> = $state.raw(new Set());
  // The threads whose frames can change this list; read by the frame
  // filter, not rendered. Always the tray's own thread.
  let callers = new Set<string>();
  // The threads frames named while a list read was in flight that the
  // list then held did not: the read's answer may nest one of them, and
  // may predate its change.
  let heardDuringList: Set<string> | null = null;
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

  const past = createPastAgentRows(() => threadId);

  /**
   * Puts a read's rows in place. `read` is the rows the read itself
   * supplied (all of `next` for a list read): Past takes its settled rows
   * of this thread's own from them, and reads again for a settled row a
   * frame named (`named`) that the read did not supply.
   */
  function replace(
    forThreadId: string | null,
    next: ThreadAgentRow[],
    more: boolean,
    read: readonly ThreadAgentRow[] = next,
    named: readonly string[] = [],
  ): void {
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
    // A row of this thread's own that settled is Past's newest. One the
    // list never held came with a frame that already said so.
    const wasOpen = new Set(listed.filter((row) => !isSettledAgentRow(row)).map((row) => row.token));
    const own = read.filter((row) => row.callerThreadId === forThreadId && isSettledAgentRow(row));
    past.patch(own);
    const supplied = new Set(read.map((row) => row.token));
    if (own.some((row) => wasOpen.has(row.token)) || named.some((token) => !supplied.has(token))) past.due();
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
  async function readRows(id: string, tokens: string[], token: RefreshToken, named: readonly string[]): Promise<boolean> {
    if (tokens.length > MAX_ROW_READ) return false;
    let reads;
    try {
      reads = await GetThreadAgentRows(id, tokens);
    } catch (err) {
      // The list read that follows says whether the requests can be read.
      if (token.isCurrent() && id === threadId) reportFrontendDiagnostic('Agents segment: agent row read failed', errString(err));
      return false;
    }
    if (!token.isCurrent() || id !== threadId) return true;
    const byToken = new Map<string, ThreadAgentRow>();
    for (const read of reads ?? []) {
      if (!read?.listed || !read.row) return false;
      byToken.set(read.row.token, read.row);
    }
    if (![...byToken.keys()].every(patchable)) return false;
    const next = listed.map((row) => {
      const read = byToken.get(row.token);
      return read ? {
        ...read, depth: row.depth, nestedHidden: row.nestedHidden,
        nestedUnavailable: row.nestedUnavailable, ownerComputerId: row.ownerComputerId,
      } : row;
    });
    replace(id, next, hidden, next.filter((row) => byToken.has(row.token)), named);
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
      const named = [...pastNamed];
      pastNamed.clear();
      if (!listDue) {
        const tokens = [...rowsDue];
        rowsDue.clear();
        if (tokens.length === 0 || await readRows(id, tokens, token, named)) return;
        if (!token.isCurrent() || id !== threadId) return;
      }
      listDue = false;
      rowsDue.clear();
      const heard = new Set<string>();
      heardDuringList = heard;
      try {
        const agents = await ListThreadAgentRows(id);
        if (!token.isCurrent() || id !== threadId) return;
        const next = agents?.rows ?? [];
        replace(id, next, agents?.hidden ?? false, next, named);
        listError = '';
        listLoaded = true;
        if ([...heard].some((caller) => callers.has(caller))) {
          listDue = true;
          refresh.request();
        }
      } catch (err) {
        if (!token.isCurrent() || id !== threadId) return;
        // A failed read says nothing about the requests. Keep the last list
        // until a read succeeds or the thread changes; the next frame or a
        // Retry reads the list, and Past reads the rows it was told of.
        listDue = true;
        if (named.length > 0) past.due();
        listError = errString(err);
        reportFrontendDiagnostic('Agents segment: agent rows read failed', listError);
      } finally {
        if (heardDuringList === heard) heardDuringList = null;
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
      listLoaded = false;
      listError = '';
      pastNamed.clear();
      callers = id ? new Set([id]) : new Set();
      heardDuringList = null;
      refresh.reset();
      refresh.request({ immediate: true });
      past.reset();
    });
  });

  // A settled row Past lists is listed there alone: it leaves the open rows
  // as a click takes it out, kept while a row open under it is shown.
  const leftOpen = $derived.by<ReadonlySet<string>>(() => {
    if (past.rows.length === 0) return dismissed;
    const out = new Set(dismissed);
    for (const row of past.rows) out.add(row.token);
    return out;
  });
  const keepRows = createAgentRowMemo();
  const rows = $derived(
    keepRows(visibleAgentRows(listed, settledSeen, leftOpen, getNow(), AGENT_ROW_SETTLED_LINGER_MS)),
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
  // A failed read is shown, with its Retry, wherever it leaves the segment.
  const shown = $derived(
    rows.length + incoming.length + (listError ? 1 : 0) + (past.total > 0 || past.error ? 1 : 0),
  );
  const runningCount = $derived(rows.filter((row) => row.state === 'running').length);
  const hasPendingSettled = $derived(rows.some(isSettledAgentRow));

  return {
    get rows() { return rows; },
    get incoming() { return incoming; },
    get hidden() { return hidden; },
    get count() { return count; },
    get shown() { return shown; },
    get past() { return past; },
    get runningCount() { return runningCount; },
    get hasPendingSettled() { return hasPendingSettled; },
    get threadId() { return threadId; },
    get status() { return listError ? 'error' : listLoaded ? 'loaded' : 'loading'; },
    get error() { return listError; },

    retry(): void {
      listDue = true;
      refresh.request({ immediate: true });
    },

    dismiss(token: string): void {
      if (dismissed.has(token)) return;
      dismissed = new Set([...dismissed, token]);
    },

    placed(placedThreadId: string, placement: string): void {
      past.placed(placedThreadId, placement);
      if (!listed.some((row) => row.threadId === placedThreadId)) return;
      listed = listed.map((row) => (row.threadId === placedThreadId ? { ...row, placement } : row));
    },

    mount(): () => void {
      // A frame about a row the list holds that runs here reads that row;
      // any other reads the list: a new row's place and the settled window
      // are the backend's.
      const cancelChanged = onThreadAgentRowsChanged((change, backend) => {
        if (!threadId) return;
        if (change === null) {
          if (threadBackend(threadId) !== backend) return;
          past.due({ immediate: true });
        } else if (!callers.has(change.threadId)) {
          heardDuringList?.add(change.threadId);
          return;
        } else if (change.threadId === threadId) {
          // A request the list holds open changes Past when it settles,
          // which the read below sees. One it holds settled is in Past too:
          // the read below supplies it. Any other, or every request (a past
          // row's late reply, one that settled out of the list), is Past's
          // to read.
          const held = change.id ? listed.find((row) => row.token === change.id) : undefined;
          if (!held) past.due();
          else if (isSettledAgentRow(held)) pastNamed.add(held.token);
        }
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
        if (status.status === 'connected') {
          refresh.request({ immediate: true });
          past.due({ immediate: true });
        }
      });
      return () => {
        cancelChanged();
        cancelStatus();
        refresh.dispose();
        past.dispose();
      };
    },
  };
}
