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
// settling or reopening, and a frame about a settled row the next read did
// not supply. A row of the thread's own that a read supplies is patched into
// Past, settled, or out of it, open again; while Past lists a row the open
// rows leave it out, so no row shows in both. The segment
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
import { onThreadPlacementFrame } from '../../stores/threadPlacementFrames';
import type { BackendKey } from '../../transport/backendKey';
import { getTransportStatusFor, onBackendStatusChange } from '../../stores/transportStatus.svelte';
import { threadBackend } from '../../transport/entityIndex';
import { createRefreshScheduler, type RefreshToken } from '../../utils/refreshScheduler';
import {
  agentRequestSettled,
  agentRowCallers,
  agentRowShownState,
  agentWorkerCount,
  createAgentRowMemo,
  isOpenAgentRow,
  visibleAgentRows,
} from '../../utils/agentTray';
import { AGENT_ROW_SETTLED_LINGER_MS } from './activityRailBackground.svelte';
import { harnessInterval } from '../../transport/harnessMode';
import { createPastAgentRows, placedRow, type AgentReadStatus, type PastAgentRows } from './activityRailAgentsPast.svelte';
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
  /** The workers open rows go to, each once, and incoming messages: the
   *  segment's count. */
  readonly count: number;
  /** Rows, incoming messages and the Past row shown at all: whether the
   *  segment renders. */
  readonly shown: number;
  /** The thread's settled rows, under Past. */
  readonly past: PastAgentRows;
  /** Workers running, each once: the toggle's pulse and the body's `N running`. */
  readonly runningCount: number;
  /** A row that is not open is shown and waits on the clock to leave. */
  readonly hasPendingSettled: boolean;
  readonly threadId: string | null;
  /** Whether a list read answered for the thread, and whether the last failed. */
  readonly status: AgentReadStatus;
  /** Why the last list read failed, until one succeeds. */
  readonly error: string;
  /** Read the list again after a failure. */
  retry(): void;
  /** Take a row that is not open out now, as its click does. */
  dismiss(token: string): void;
  /**
   * The sidebar placement `backend`, the computer holding a row's thread,
   * answered Show in Sidebar with; its frames say the same.
   */
  placed(threadId: string, placement: string, backend: BackendKey): void;
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
  // Rows whose requests settled that frames named, which a read of the
  // open list answers for: Past takes each from that read, or reads it when
  // the read does not.
  const pastNamed = new Set<string>();
  // When this pane first saw each row that is not open, and those a click
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

  function placed(placedThreadId: string, placement: string, backend: BackendKey): void {
    past.placed(placedThreadId, placement, backend);
    const next = listed.map((row) => placedRow(row, placedThreadId, placement));
    if (next.some((row, i) => row !== listed[i])) listed = next;
  }

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
   * supplied (all of `next` for a list read): Past takes its rows of this
   * thread's own from them, ordered by `at`, the read's start, and reads
   * again when one of them closed or opened again since the list held it, or
   * for a settled row a frame named (`named`) that the read did not supply.
   */
  function replace(
    forThreadId: string | null,
    next: ThreadAgentRow[],
    more: boolean,
    at: number,
    read: readonly ThreadAgentRow[] = next,
    named: readonly string[] = [],
  ): void {
    // The placement the thread's own computer said stands over the one
    // this read relays (activityRailAgentsPast.svelte.ts, Placement).
    next = next.map((row) => past.withPlacement(row));
    // The wall clock, not the rail's: the rail's clock stops while nothing
    // it shows needs it, and a stamp from its last tick would end the
    // linger on the first tick after it starts again.
    const now = Date.now();
    const seen = new Map<string, number>();
    for (const row of next) {
      if (isOpenAgentRow(row)) continue;
      seen.set(row.token, settledSeen.get(row.token) ?? now);
    }
    const tokens = new Set(next.map((row) => row.token));
    // A row of this thread's own that is no longer open is Past's newest,
    // and one open again leaves it. One the list never held came with a
    // frame that already said so.
    const wasOpen = new Map(listed.map((row) => [row.token, isOpenAgentRow(row)]));
    const own = read.filter((row) => row.callerThreadId === forThreadId);
    past.patch(own, at);
    const supplied = new Set(read.map((row) => row.token));
    const turned = own.some((row) => {
      const before = wasOpen.get(row.token);
      return before !== undefined && before !== isOpenAgentRow(row);
    });
    if (turned || named.some((token) => !supplied.has(token))) past.due();
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
    // Past orders this read's rows by when it started (activityRailAgentsPast).
    const at = past.readStarted();
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
    replace(id, next, hidden, at, next.filter((row) => byToken.has(row.token)), named);
    return true;
  }

  const refresh = createRefreshScheduler({
    name: 'activityRailAgents',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: async (token) => {
      const id = threadId;
      if (!id) {
        replace(null, [], false, past.readStarted());
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
        const at = past.readStarted();
        const agents = await ListThreadAgentRows(id);
        if (!token.isCurrent() || id !== threadId) return;
        const next = agents?.rows ?? [];
        replace(id, next, agents?.hidden ?? false, at, next, named);
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
    keepRows(visibleAgentRows(listed, settledSeen, leftOpen, getNow(), harnessInterval('agent-row-linger', AGENT_ROW_SETTLED_LINGER_MS))),
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
  const count = $derived(agentWorkerCount(rows) + incoming.length);
  // A failed read is shown, with its Retry, wherever it leaves the segment.
  const shown = $derived(
    rows.length + incoming.length + (listError ? 1 : 0) + (past.total > 0 || past.error ? 1 : 0),
  );
  const runningCount = $derived(agentWorkerCount(rows, (row) => agentRowShownState(row) === 'running'));
  const hasPendingSettled = $derived(rows.some((row) => !isOpenAgentRow(row)));

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

    placed,

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
          // which the read below sees. One it holds settled is in Past, or
          // goes there when its worker rests: the read below supplies it, or
          // leaves it out. Any other, or every request (a past row's late
          // reply, one that settled out of the list), is Past's to read.
          const held = change.id ? listed.find((row) => row.token === change.id) : undefined;
          if (!held) past.due();
          else if (agentRequestSettled(held)) pastNamed.add(held.token);
        }
        if (change?.id && !listDue && patchable(change.id)) rowsDue.add(change.id);
        else listDue = true;
        if (rowsDue.size > MAX_ROW_READ) {
          listDue = true;
          rowsDue.clear();
        }
        refresh.request({ immediate: change === null });
      });
      // A placement the computer holding a shown row's thread announced.
      const cancelPlacement = onThreadPlacementFrame((placedThreadId, placement, backend) => {
        const shownHere = (row: ThreadAgentRow) => row.threadId === placedThreadId;
        if (listed.some(shownHere) || past.rows.some(shownHere)) placed(placedThreadId, placement, backend);
      });
      const cancelStatus = onBackendStatusChange((backend, status) => {
        if (status.status !== 'connected') past.forgetPlacements(backend);
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
        cancelPlacement();
        cancelStatus();
        refresh.dispose();
        past.dispose();
      };
    },
  };
}
