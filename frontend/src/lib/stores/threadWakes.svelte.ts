// The loops, watches and reminders of the threads a surface shows, by thread
// id (docs/specs/standing-agents.md, "Sidebar" and "Activity rail"). One
// model feeds both readers: a standing agent's sidebar row reads its live
// loops and watches for the state line, and a composer's background tray
// reads its open thread's whole listing, reminders included.
//
// Loops and watches are kept for every standing agent the sidebar knows and
// every thread a tray retains; reminders only while a tray retains the
// thread. One bulk read per computer at boot and on resync
// (ListStandingAgentWakes, which lists standing agents alone), each
// settling when its own computer answers and superseded only by a newer
// read of that computer, one
// ListThreadWakes for an agent row the bulk missed (a thread promoted since)
// and for a tray's thread when it opens, after its computer reconnects and
// after a lost frame. `thread:wakes_changed` frames are applied in place, in
// the listing's own order, so a frame costs no read except where the frame
// alone cannot settle the listing: a frame with neither row nor removed, and
// a reminder change past the listing's bound while the thread has more
// reminders than it lists. A frame that lands while a read is out is applied
// again over that read's answer: a bulk read's, when the frame's computer is
// the one it reads.
//
// Reads are ordered by the sequence number each takes when it starts, the
// bulk read and a thread's own reads, the sidebar's and a tray's, alike. A
// thread's loops take a read's answer only when no read that started later
// has written them, and the frames a read replays only then; a bulk read
// writes the threads it answers for. Reminders, which only a thread's own
// read carries, are ordered among those reads the same way, so a tray read
// whose loops a newer bulk answer settled still lands its reminders. A
// thread's own read still out when its computer's bulk answer lands may
// predate it and is asked again. A thread's sequence is kept only while an
// older read is still out.
//
// Each thread is its own key, and a known thread keeps its key with no
// rows, so a frame reruns only the readers of the thread it names.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import { ListStandingAgentWakes, ListThreadWakes, type ThreadWakeRow, type ThreadWakes, type StandingAgentWakes } from './bindings';
import { onThreadWakesChanged, type ThreadWakeChange } from './eventsThreadWakes';
import { readComputerRows } from './computerRows';
import { getTransportStatusFor, onBackendStatusChange } from './transportStatus.svelte';
import { attachedBackends, onBackendDetached, withBackendTarget } from '../transport/backends';
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
/** Each thread's read in flight, by a ticket only that read holds, naming the computer it asked. */
const pending = new Map<string, { backend: BackendKey }>();

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
 * `covers` takes the frame's thread and the computer that sent it: a read
 * logs only the frames its answer can predate, so its replay never applies
 * one that a newer read of another computer has settled.
 */
interface FrameLog {
  covers: (threadId: string, backend: string) => boolean;
  changes: Map<string, { change: ThreadWakeChange; backend: string }>;
}
const reads = new Set<FrameLog>();

let sequence = 0;
/** The start sequences of the reads still out. */
const outstanding = new Set<number>();
/** The start sequence of the newest read that wrote each thread's loops, and its reminders, while an older read is out. */
const loopsAt = new Map<string, number>();
const remindersAt = new Map<string, number>();
/** Each tray's read in flight, by thread. */
const trayReads = new Set<string>();

function beginRead(): number {
  const seq = ++sequence;
  outstanding.add(seq);
  return seq;
}

/** Ends a read, and forgets each thread's sequence that no read still out could undercut. */
function endRead(seq: number): void {
  outstanding.delete(seq);
  let oldest = Infinity;
  for (const out of outstanding) oldest = Math.min(oldest, out);
  for (const at of [loopsAt, remindersAt]) {
    for (const [threadId, readAt] of at) if (readAt <= oldest) at.delete(threadId);
  }
}

/** Whether the read that started at `seq` may write the thread: no read that started later has. */
function admits(at: Map<string, number>, threadId: string, seq: number): boolean {
  return seq >= (at.get(threadId) ?? 0);
}

/** Writes a thread's loops from the read that started at `seq`, unless a later read has. */
function landLoops(threadId: string, backend: BackendKey, rows: readonly ThreadWakeRow[], seq: number): void {
  if (!admits(loopsAt, threadId, seq)) return;
  loopsAt.set(threadId, seq);
  setLoops(threadId, backend, rows);
}

function openLog(covers: FrameLog['covers']): FrameLog {
  const log: FrameLog = { covers, changes: new Map() };
  reads.add(log);
  return log;
}

/** Applies the frames logged during the read that started at `seq` over its answer. */
function replay(log: FrameLog, seq: number): void {
  reads.delete(log);
  for (const { change, backend } of log.changes.values()) applyChange(change, backend, seq);
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

/**
 * Writes a ListThreadWakes answer from the read that started at `seq`: the
 * loops, and the reminders while a tray retains the thread, each unless a
 * later read has written them.
 */
function applyListing(threadId: string, backend: BackendKey, wakes: ThreadWakes | null | undefined, seq: number): void {
  const rows = wakes?.rows ?? [];
  landLoops(threadId, backend, rows.filter(isLoop), seq);
  if (!retained.has(threadId) || !admits(remindersAt, threadId, seq)) return;
  remindersAt.set(threadId, seq);
  setReminders(threadId, rows.filter((row) => row.kind === 'reminder'), wakes?.moreReminders ?? 0);
}

/** Each computer's bulk read in flight, by a ticket only that read holds. */
const bulkReads = new Map<BackendKey, object>();

/**
 * Boot-time / resync wholesale load: every attached computer, or `only`.
 * Each computer's read stands alone and settles when that computer answers,
 * however late. Errors surface to the console only: the line is decoration.
 */
export async function refreshAgentWakes(only?: BackendKey): Promise<void> {
  const targets = attachedBackends().filter((entry) => only === undefined || entry.id === only);
  await Promise.all(targets.map((entry) => refreshComputerAgentWakes(entry.id)));
}

async function refreshComputerAgentWakes(backend: BackendKey): Promise<void> {
  // A newer read of the computer supersedes this one; its frames from here
  // until this answer lands replay over it.
  const ticket = {};
  bulkReads.set(backend, ticket);
  const seq = beginRead();
  const log = openLog((_threadId, origin) => origin === backend);
  try {
    await readComputerRows<StandingAgentWakes>(
      async () => await ListStandingAgentWakes(),
      () => {},
      (result) => {
        if (bulkReads.get(backend) !== ticket) return;
        const listed = new Set<string>();
        for (const agent of result.rows) {
          listed.add(agent.threadId);
          agents.add(agent.threadId);
          // The row's computer is the one that answered it: the read may
          // land before the thread list has indexed the agent.
          landLoops(agent.threadId, backend, agent.rows.filter(isLoop), seq);
        }
        for (const [threadId, entry] of untrack(() => [...loops])) {
          // A thread only a tray shows is not the bulk read's to answer.
          if (listed.has(threadId) || !(agents.has(threadId) || getThreadById(threadId)?.agentId)) continue;
          // An agent the computer answered for without it has no live
          // rows; one on a computer that no longer answers keeps the last
          // it had while that computer stays attached.
          if (result.answered.has(entry.backend)) landLoops(threadId, entry.backend, NO_ROWS, seq);
          else if (!result.attached.has(entry.backend)) {
            agents.delete(threadId);
            dropLoopsIfUnshown(threadId);
          }
        }
        replay(log, seq);
        askPendingAgain(backend);
      },
      // Wait for the computer itself: the frames this read buffers replay
      // only over its own answer.
      { only: backend, deadlineMs: null },
    );
  } catch (err) {
    if (isPassiveConnectionFailure(err)) return;
    console.error('Failed to load standing agent wakes:', err);
  } finally {
    reads.delete(log);
    if (bulkReads.get(backend) === ticket) bulkReads.delete(backend);
    endRead(seq);
  }
}

/**
 * A thread's own read that is out when its computer's bulk answer lands may
 * predate that answer, so it is asked again: the sidebar's is dropped for a
 * new one, and a tray's is followed by another. The bulk's loops stand
 * until a newer read answers.
 */
function askPendingAgain(backend: BackendKey): void {
  for (const [threadId, read] of [...pending]) {
    if (read.backend !== backend) continue;
    pending.delete(threadId);
    ensureAgentWakes(threadId);
  }
  for (const threadId of trayReads) {
    if ((threadBackend(threadId) ?? HOME_BACKEND) === backend) retained.get(threadId)?.refresh.request({ immediate: true });
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
  const ticket = { backend };
  pending.set(threadId, ticket);
  const seq = beginRead();
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
    applyListing(threadId, backend, wakes, seq);
    replay(log, seq);
  }).catch((err: unknown) => {
    reads.delete(log);
    if (pending.get(threadId) === ticket) pending.delete(threadId);
    if (!isPassiveConnectionFailure(err)) console.error(`Failed to load wakes of ${threadId}:`, err);
  }).finally(() => endRead(seq));
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
      const seq = beginRead();
      const log = openLog((id) => id === threadId);
      trayReads.add(threadId);
      try {
        const wakes = await ListThreadWakes(threadId);
        if (!token.isCurrent()) return;
        applyListing(threadId, (owner ?? untrack(() => loops.get(threadId))?.backend ?? HOME_BACKEND) as BackendKey, wakes, seq);
        replay(log, seq);
      } catch (err) {
        // A failed read says nothing about the schedule. Keep the last
        // listing until a read succeeds or the tray lets go.
        if (token.isCurrent()) console.error('ActivityRail: ListThreadWakes failed:', err);
      } finally {
        reads.delete(log);
        trayReads.delete(threadId);
        endRead(seq);
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

/**
 * Applies a frame as it lands, or, with `replayOf`, as the read that started
 * then replays it over its answer: only over what that read may still write.
 */
function applyChange(change: ThreadWakeChange, backend: string, replayOf?: number): void {
  const { threadId, id, row } = change;
  const entry = untrack(() => loops.get(threadId));
  const listed = untrack(() => reminders.get(threadId));
  if (!row && !change.removed) {
    // The backend could not read the row.
    readAgain(threadId);
    return;
  }
  if (row ? isLoop(row) : entry?.rows.some((current) => current.id === id)) {
    if (replayOf === undefined || admits(loopsAt, threadId, replayOf)) applyLoopChange(threadId, id, row, entry, backend);
    return;
  }
  // Reminders are the tray's alone.
  if (listed && (replayOf === undefined || admits(remindersAt, threadId, replayOf))) applyReminderChange(threadId, id, row, listed);
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
  void refreshAgentWakes(backend as BackendKey);
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
    if (log.covers(change.threadId, backend)) log.changes.set(`${change.threadId}\u0000${change.id}`, { change, backend });
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

/** Test helper: what the read ordering retains, by kind. */
export function threadWakesRetentionForTest(): { outstanding: number; loopsAt: number; remindersAt: number; trayReads: number } {
  return { outstanding: outstanding.size, loopsAt: loopsAt.size, remindersAt: remindersAt.size, trayReads: trayReads.size };
}

/** Test helper. */
export function resetThreadWakesForTest(): void {
  for (const retention of retained.values()) retention.refresh.dispose();
  retained.clear();
  reads.clear();
  bulkReads.clear();
  outstanding.clear();
  loopsAt.clear();
  remindersAt.clear();
  trayReads.clear();
  sequence = 0;
  loops.clear();
  reminders.clear();
  agents.clear();
  pending.clear();
}
