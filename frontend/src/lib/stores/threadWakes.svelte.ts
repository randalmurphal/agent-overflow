// The loops, watches and reminders of the threads a surface shows, by thread
// id (docs/specs/standing-agents.md, "Sidebar" and "Activity rail"). One
// model feeds both readers: a standing agent's sidebar row reads its live
// loops and watches for the state line, and a composer's background tray
// reads its open thread's whole listing, reminders included.
//
// Loops and watches are kept for every standing agent the sidebar knows and
// every thread a tray retains; reminders only while a tray retains the
// thread. One bulk read per computer at boot and on resync
// (ListStandingAgentWakes, which lists standing agents alone), one
// ListThreadWakes for an agent row the bulk missed (a thread promoted since)
// and for a tray's thread when it opens, after its computer reconnects and
// after a lost frame. `thread:wakes_changed` frames are applied in place, in
// the listing's own order, so a frame costs no read except where the frame
// alone cannot settle the listing: a frame with neither row nor removed, and
// a reminder change past the listing's bound while the thread has more
// reminders than it lists. A frame that lands while a read is out is applied
// again over that read's answer.
//
// Each thread is its own key, and a known thread keeps its key with no
// rows, so a frame reruns only the readers of the thread it names.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import { ListStandingAgentWakes, ListThreadWakes, type ThreadWakeRow, type ThreadWakes, type StandingAgentWakes } from './bindings';
import { onThreadWakesChanged, type ThreadWakeChange } from './eventsThreadWakes';
import { readComputerRows } from './computerRows';
import { getTransportStatusFor, onBackendStatusChange } from './transportStatus.svelte';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { threadBackend } from '../transport/entityIndex';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import type { BackendKey } from '../transport/backendKey';
import { createRefreshScheduler, type RefreshScheduler } from '../utils/refreshScheduler';
import { getThreadById } from './threads.svelte';
import { onStandingAgentEnded } from './standingAgentEnded';

/** The backend's bound on the reminders one listing carries (threadWakeReminderLimit). */
const REMINDER_LIMIT = 64;
const REFRESH_DELAY_MS = 100;
const REFRESH_MAX_WAIT_MS = 400;

interface LoopEntry {
  backend: BackendKey;
  /** Live loops and watches, by label as the backend lists them. */
  rows: readonly ThreadWakeRow[];
}

interface ReminderEntry {
  /** The soonest reminders that have not fired, at most REMINDER_LIMIT. */
  rows: readonly ThreadWakeRow[];
  /** Reminders past the bound, counted but not listed. */
  more: number;
}

/** A tray's view of its thread: the backend's listing, in its order. */
export interface ThreadWakeListing {
  loops: readonly ThreadWakeRow[];
  reminders: readonly ThreadWakeRow[];
  moreReminders: number;
}

const NO_ROWS: readonly ThreadWakeRow[] = Object.freeze([]);

const loops = new SvelteMap<string, LoopEntry>();
const reminders = new SvelteMap<string, ReminderEntry>();
/**
 * Standing agents whose loops the sidebar holds, read or listed, with or
 * without live rows, and those whose own read is out. Plain sets: a row's
 * mount effect consults them, and a reactive read there would rerun every
 * mounted row's effect on any thread's change.
 */
const agents = new Set<string>();
/** Each thread's read in flight, by a ticket only that read holds. */
const pending = new Map<string, object>();

interface Retention {
  count: number;
  refresh: RefreshScheduler;
}
/** Threads a tray shows, with the scheduler of their reads. */
const retained = new Map<string, Retention>();

/**
 * The frames that landed while a read was out, latest per row, replayed
 * over its answer. A frame is the row as it stands, so the latest per row
 * is all a replay needs, and the log is bounded by the rows themselves.
 */
interface FrameLog {
  covers: (threadId: string) => boolean;
  changes: Map<string, { change: ThreadWakeChange; backend: string }>;
}
const reads = new Set<FrameLog>();

function openLog(covers: (threadId: string) => boolean): FrameLog {
  const log: FrameLog = { covers, changes: new Map() };
  reads.add(log);
  return log;
}

function replay(log: FrameLog): void {
  reads.delete(log);
  for (const { change, backend } of log.changes.values()) applyChange(change, backend);
}

/** The agent's live loops and watches; the shared empty array when none. */
export function getAgentWakeRows(threadId: string): readonly ThreadWakeRow[] {
  return loops.get(threadId)?.rows ?? NO_ROWS;
}

/**
 * The thread's whole listing, or null until a tray retaining it has read
 * it. Loops come first, then the soonest reminders, as ListThreadWakes
 * orders them.
 */
export function getThreadWakeListing(threadId: string): ThreadWakeListing | null {
  const listed = reminders.get(threadId);
  if (!listed) return null;
  return { loops: loops.get(threadId)?.rows ?? NO_ROWS, reminders: listed.rows, moreReminders: listed.more };
}

/**
 * Orders as SQLite's BINARY collation orders UTF-8: by code point, so a
 * character past U+FFFF, a surrogate pair here, sorts after every other.
 */
function compareCodePoints(a: string, b: string): number {
  if (a === b) return 0;
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i++) {
    const x = a.charCodeAt(i);
    const y = b.charCodeAt(i);
    if (x === y) continue;
    const xPair = x >= 0xd800 && x <= 0xdfff;
    const yPair = y >= 0xd800 && y <= 0xdfff;
    if (xPair !== yPair) return xPair ? 1 : -1;
    return x - y;
  }
  return a.length - b.length;
}

/** ListAgentWakes: ORDER BY label. */
function compareLoops(a: ThreadWakeRow, b: ThreadWakeRow): number {
  return compareCodePoints(a.label, b.label) || compareCodePoints(a.id, b.id);
}

/** ListOpenThreadReminders: ORDER BY due_at, token. */
function compareReminders(a: ThreadWakeRow, b: ThreadWakeRow): number {
  return (a.nextAt ?? 0) - (b.nextAt ?? 0) || compareCodePoints(a.id, b.id);
}

function isLoop(row: ThreadWakeRow): boolean {
  return row.kind === 'loop' || row.kind === 'watch';
}

function sameRow(a: ThreadWakeRow, b: ThreadWakeRow): boolean {
  const keys = Object.keys(a) as Array<keyof ThreadWakeRow>;
  return keys.length === Object.keys(b).length && keys.every((key) => a[key] === b[key]);
}

/**
 * Writes the thread's loops unless they are what it holds, so a resync or a
 * replayed frame that changes nothing reruns no reader. A known thread with
 * no rows keeps its key: a reader of a missing key reruns on every other
 * thread's arrival and departure.
 */
function setLoops(threadId: string, backend: BackendKey, rows: readonly ThreadWakeRow[]): void {
  const current = untrack(() => loops.get(threadId));
  if (current && current.backend === backend && sameRows(current.rows, rows)) return;
  loops.set(threadId, { backend, rows: rows.length === 0 ? NO_ROWS : rows });
}

function sameRows(a: readonly ThreadWakeRow[], b: readonly ThreadWakeRow[]): boolean {
  return a.length === b.length && a.every((row, i) => sameRow(row, b[i]));
}

/** Writes a tray's reminders unless they are what it holds, as setLoops does. */
function setReminders(threadId: string, rows: readonly ThreadWakeRow[], more: number): void {
  const current = untrack(() => reminders.get(threadId));
  if (current && current.more === more && sameRows(current.rows, rows)) return;
  reminders.set(threadId, { rows, more });
}

/** Drops the thread's loops unless the sidebar or a tray still shows them. */
function dropLoopsIfUnshown(threadId: string): void {
  if (agents.has(threadId) || pending.has(threadId) || retained.has(threadId)) return;
  loops.delete(threadId);
}

/** Writes a ListThreadWakes answer: the loops always, the reminders while a tray retains the thread. */
function applyListing(threadId: string, backend: BackendKey, wakes: ThreadWakes | null | undefined): void {
  const rows = wakes?.rows ?? [];
  setLoops(threadId, backend, rows.filter(isLoop));
  if (!retained.has(threadId)) return;
  setReminders(threadId, rows.filter((row) => row.kind === 'reminder'), wakes?.moreReminders ?? 0);
}

/** Boot-time / resync wholesale load. Errors surface to the console only: the line is decoration. */
export async function refreshAgentWakes(): Promise<void> {
  const log = openLog(() => true);
  try {
    // Each row's computer is the one that answered it: the read may land
    // before the thread list has indexed the agent.
    const owners = new Map<string, BackendKey>();
    await readComputerRows<StandingAgentWakes>(
      async () => await ListStandingAgentWakes(),
      (row, backend) => owners.set(row.threadId, backend),
      (result) => {
        const listed = new Set<string>();
        for (const agent of result.rows) {
          listed.add(agent.threadId);
          agents.add(agent.threadId);
          const backend = owners.get(agent.threadId) ?? threadBackend(agent.threadId) ?? HOME_BACKEND;
          setLoops(agent.threadId, backend, agent.rows.filter(isLoop));
        }
        for (const [threadId, entry] of untrack(() => [...loops])) {
          // A thread only a tray shows is not the bulk read's to answer.
          if (listed.has(threadId) || !(agents.has(threadId) || getThreadById(threadId)?.agentId)) continue;
          // An agent the computer answered for without it has no live
          // rows; one on a computer that no longer answers keeps the last
          // it had while that computer stays attached.
          if (result.answered.has(entry.backend)) setLoops(threadId, entry.backend, NO_ROWS);
          else if (!result.attached.has(entry.backend)) {
            agents.delete(threadId);
            dropLoopsIfUnshown(threadId);
          }
        }
      },
    );
    replay(log);
  } catch (err) {
    reads.delete(log);
    if (isPassiveConnectionFailure(err)) return;
    console.error('Failed to load standing agent wakes:', err);
  }
}

/**
 * Fill one agent's rows when the bulk read did not carry them: a thread
 * promoted after boot keeps the loops it registered before, and an idle
 * agent is absent from the bulk read altogether. Asked by the row on
 * mount; a thread already read, with rows or without, or whose read is
 * out, is left alone. Reads nothing reactive: see `agents`.
 */
export function ensureAgentWakes(threadId: string): void {
  if (agents.has(threadId) || pending.has(threadId)) return;
  const backend = threadBackend(threadId) ?? HOME_BACKEND;
  const ticket = {};
  pending.set(threadId, ticket);
  const log = openLog((id) => id === threadId);
  void withBackendTarget(backend, () => ListThreadWakes(threadId)).then((wakes) => {
    // Forgotten while the read was out (the thread stopped being an
    // agent): its answer is not state the sidebar shows.
    if (pending.get(threadId) !== ticket) {
      reads.delete(log);
      return;
    }
    // Settled before the replay, which may ask for the next read.
    pending.delete(threadId);
    agents.add(threadId);
    applyListing(threadId, backend, wakes);
    replay(log);
  }).catch((err: unknown) => {
    reads.delete(log);
    if (pending.get(threadId) === ticket) pending.delete(threadId);
    if (!isPassiveConnectionFailure(err)) console.error(`Failed to load wakes of ${threadId}:`, err);
  });
}

/**
 * Drop what the store holds for a thread that stopped being a standing
 * agent here, so a later promotion reads its rows afresh.
 */
export function forgetAgentWakes(threadId: string): void {
  agents.delete(threadId);
  pending.delete(threadId);
  dropLoopsIfUnshown(threadId);
}

/**
 * Keep the thread's whole listing while a tray shows it: read it now, again
 * after its computer reconnects or loses a frame, and keep it with each
 * frame. Returns the release; the last release drops the reminders, and the
 * loops of a thread the sidebar does not hold.
 */
export function retainThreadWakes(threadId: string): () => void {
  let held = retained.get(threadId);
  if (!held) {
    held = { count: 0, refresh: createListingRefresh(threadId) };
    retained.set(threadId, held);
    // The scheduler runs untracked: the retaining effect does not depend
    // on what the read consults.
    held.refresh.request({ immediate: true });
  }
  held.count += 1;
  let released = false;
  return () => {
    if (released) return;
    released = true;
    held.count -= 1;
    if (held.count > 0 || retained.get(threadId) !== held) return;
    held.refresh.dispose();
    retained.delete(threadId);
    reminders.delete(threadId);
    dropLoopsIfUnshown(threadId);
  };
}

function createListingRefresh(threadId: string): RefreshScheduler {
  return createRefreshScheduler({
    name: 'threadWakes',
    delayMs: REFRESH_DELAY_MS,
    maxWaitMs: REFRESH_MAX_WAIT_MS,
    run: async (token) => {
      const owner = threadBackend(threadId);
      if (owner !== undefined && getTransportStatusFor(owner).status !== 'connected') return;
      const log = openLog((id) => id === threadId);
      try {
        const wakes = await ListThreadWakes(threadId);
        if (!token.isCurrent()) return;
        applyListing(threadId, (owner ?? untrack(() => loops.get(threadId))?.backend ?? HOME_BACKEND) as BackendKey, wakes);
        replay(log);
      } catch (err) {
        // A failed read says nothing about the schedule. Keep the last
        // listing until a read succeeds or the tray lets go.
        if (token.isCurrent()) console.error('ActivityRail: ListThreadWakes failed:', err);
      } finally {
        reads.delete(log);
      }
    },
  });
}

/** Reads the thread again: through its tray's scheduler, or as the sidebar's one read. */
function readAgain(threadId: string): void {
  const retention = retained.get(threadId);
  if (retention) {
    retention.refresh.request();
    return;
  }
  if (!agents.has(threadId)) return;
  // The row keeps its rows until the answer replaces them; a read already
  // out replays this frame over its answer, which asks again.
  agents.delete(threadId);
  ensureAgentWakes(threadId);
}

function applyChange(change: ThreadWakeChange, backend: string): void {
  const { threadId, id, row } = change;
  const entry = untrack(() => loops.get(threadId));
  const listed = untrack(() => reminders.get(threadId));
  if (!row && !change.removed) {
    // The backend could not read the row.
    readAgain(threadId);
    return;
  }
  if (row ? isLoop(row) : entry?.rows.some((current) => current.id === id)) {
    applyLoopChange(threadId, id, row, entry, backend);
    return;
  }
  // Reminders are the tray's alone.
  if (listed) applyReminderChange(threadId, id, row, listed);
}

function applyLoopChange(threadId: string, id: string, row: ThreadWakeRow | undefined, entry: LoopEntry | undefined, backend: string): void {
  // A row of a thread neither surface shows is not state; the agent row
  // asks for its rows when it mounts.
  if (!entry && !(row && (retained.has(threadId) || getThreadById(threadId)?.agentId))) return;
  const rows = (entry?.rows ?? NO_ROWS).filter((current) => current.id !== id);
  if (row) {
    rows.push(row);
    rows.sort(compareLoops);
  } else if (entry && rows.length === entry.rows.length) {
    return;
  }
  setLoops(threadId, (entry?.backend ?? threadBackend(threadId) ?? backend) as BackendKey, rows);
}

function applyReminderChange(threadId: string, id: string, row: ThreadWakeRow | undefined, listed: ReminderEntry): void {
  const at = listed.rows.findIndex((current) => current.id === id);
  if (listed.more > 0) {
    // The listing holds the soonest of more. A listed row that keeps its
    // place is settled here; any other change may bring in, or push out,
    // a reminder this listing does not hold.
    if (row && at >= 0 && listed.rows[at].nextAt === row.nextAt) {
      const rows = [...listed.rows];
      rows[at] = row;
      setReminders(threadId, rows, listed.more);
      return;
    }
    readAgain(threadId);
    return;
  }
  // The listing holds every reminder: any change is settled here.
  if (!row && at < 0) return;
  const rows = listed.rows.filter((current) => current.id !== id);
  if (row) {
    rows.push(row);
    rows.sort(compareReminders);
  }
  const more = rows.length > REMINDER_LIMIT ? rows.length - REMINDER_LIMIT : 0;
  setReminders(threadId, more > 0 ? rows.slice(0, REMINDER_LIMIT) : rows, more);
}

/** A frame for any thread of `backend` may have been lost: read every thread of it again. */
function readComputerAgain(backend: string): void {
  void refreshAgentWakes();
  for (const [threadId, retention] of retained) {
    if (threadBackend(threadId) === backend) retention.refresh.request({ immediate: true });
  }
}

onThreadWakesChanged((change, backend) => {
  if (change === null) {
    readComputerAgain(backend);
    return;
  }
  for (const log of reads) {
    if (log.covers(change.threadId)) log.changes.set(`${change.threadId}\u0000${change.id}`, { change, backend });
  }
  applyChange(change, backend);
});

onBackendStatusChange((backend, status) => {
  for (const [threadId, retention] of retained) {
    if (threadBackend(threadId) !== backend) continue;
    retention.refresh.reset();
    if (status.status === 'connected') retention.refresh.request({ immediate: true });
  }
});

onStandingAgentEnded(forgetAgentWakes);

onBackendDetached(({ threadIds }) => {
  for (const threadId of threadIds) {
    // A read queued or out for the thread would refill what the detach
    // removes. The tray keeps its retention until it lets go.
    retained.get(threadId)?.refresh.reset();
    agents.delete(threadId);
    pending.delete(threadId);
    reminders.delete(threadId);
    loops.delete(threadId);
  }
});

/** Test helper. */
export function resetThreadWakesForTest(): void {
  for (const retention of retained.values()) retention.refresh.dispose();
  retained.clear();
  reads.clear();
  loops.clear();
  reminders.clear();
  agents.clear();
  pending.clear();
}
