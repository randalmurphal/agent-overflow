// The live loops and watches of every standing agent, by thread id: what
// the sidebar's agent row turns into its state line
// (docs/specs/standing-agents.md, "Sidebar"). One bulk read per computer at
// boot and on resync (ListStandingAgentWakes, which lists standing agents
// alone), one read for a row the bulk missed (a thread promoted since), and
// `thread:wakes_changed` frames in between. Reminders never enter: they
// are not state. A gap on a computer re-reads that computer.

import { ListStandingAgentWakes, ListThreadWakes, type ThreadWakeRow, type StandingAgentWakes } from './bindings';
import { onThreadWakesChanged } from './eventsThreadWakes';
import { readComputerRows } from './computerRows';
import { onBackendDetached } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { threadBackend } from '../transport/entityIndex';
import { withBackendTarget } from '../transport/backends';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import type { BackendKey } from '../transport/backendKey';
import { agentWakeRows } from '../utils/agentStateLine';
import { getThreadById } from './threads.svelte';
import { onStandingAgentEnded } from './standingAgentEnded';

interface AgentWakeEntry {
  backend: BackendKey;
  rows: ThreadWakeRow[];
}

const NO_ROWS: readonly ThreadWakeRow[] = Object.freeze([]);

let entries = $state<Map<string, AgentWakeEntry>>(new Map());
/**
 * Threads whose rows this store has read, with or without live rows, and
 * threads whose own read is in flight. Both stay outside the reactive
 * state: a row's mount effect consults them, and a reactive read there
 * would re-run every mounted row's effect on every change to `entries`,
 * which for two idle agents answering at different speeds is a loop of
 * reads that never settles.
 */
const known = new Set<string>();
const pending = new Set<string>();

/** The agent's live loops and watches; the shared empty array when none. */
export function getAgentWakeRows(threadId: string): readonly ThreadWakeRow[] {
  return entries.get(threadId)?.rows ?? NO_ROWS;
}

/** Writes the thread's live rows; `entries` changes only when the map does. */
function setRows(threadId: string, backend: BackendKey, rows: readonly ThreadWakeRow[]): void {
  known.add(threadId);
  const live = agentWakeRows(rows);
  if (live.length === 0) {
    if (!entries.has(threadId)) return;
    const next = new Map(entries);
    next.delete(threadId);
    entries = next;
    return;
  }
  const next = new Map(entries);
  next.set(threadId, { backend, rows: live });
  entries = next;
}

/** Boot-time / resync wholesale load. Errors surface to the console only: the line is decoration. */
export async function refreshAgentWakes(): Promise<void> {
  try {
    // Each row's computer is the one that answered it: the read may land
    // before the thread list has indexed the agent.
    const owners = new Map<string, BackendKey>();
    await readComputerRows<StandingAgentWakes>(
      async () => await ListStandingAgentWakes(),
      (row, backend) => owners.set(row.threadId, backend),
      (result) => {
        const next = new Map<string, AgentWakeEntry>();
        for (const [threadId, entry] of entries) {
          if (!result.answered.has(entry.backend) && result.attached.has(entry.backend)) next.set(threadId, entry);
        }
        for (const agent of result.rows) {
          known.add(agent.threadId);
          const backend = owners.get(agent.threadId) ?? threadBackend(agent.threadId) ?? HOME_BACKEND;
          const live = agentWakeRows(agent.rows);
          if (live.length > 0) next.set(agent.threadId, { backend, rows: live });
        }
        entries = next;
      },
    );
  } catch (err) {
    if (isPassiveConnectionFailure(err)) return;
    console.error('Failed to load standing agent wakes:', err);
  }
}

/**
 * Fill one agent's rows when the bulk read did not carry them: a thread
 * promoted after boot keeps the loops it registered before, and an idle
 * agent is absent from the bulk read altogether. Asked by the row on
 * mount; a thread already read, with rows or without, or whose read is
 * out, is left alone. Reads nothing reactive: see `known`.
 */
export function ensureAgentWakes(threadId: string): void {
  if (known.has(threadId) || pending.has(threadId)) return;
  const backend = threadBackend(threadId) ?? HOME_BACKEND;
  pending.add(threadId);
  void withBackendTarget(backend, () => ListThreadWakes(threadId)).then((wakes) => {
    // Forgotten while the read was out (the thread stopped being an
    // agent): its answer is not state the sidebar shows.
    if (!pending.has(threadId)) return;
    setRows(threadId, backend, wakes?.rows ?? []);
  }).catch((err: unknown) => {
    if (!isPassiveConnectionFailure(err)) console.error(`Failed to load wakes of ${threadId}:`, err);
  }).finally(() => {
    pending.delete(threadId);
  });
}

/**
 * Drop what the store holds for a thread that stopped being a standing
 * agent here, so a later promotion reads its rows afresh.
 */
export function forgetAgentWakes(threadId: string): void {
  known.delete(threadId);
  pending.delete(threadId);
  if (!entries.has(threadId)) return;
  const next = new Map(entries);
  next.delete(threadId);
  entries = next;
}

onThreadWakesChanged((change, backend) => {
  if (change === null) {
    void refreshAgentWakes();
    return;
  }
  const { threadId, id } = change;
  const current = entries.get(threadId);
  if (change.row) {
    if (change.row.kind === 'reminder') return;
    // A row of a thread that is not a standing agent is not state the
    // sidebar shows; the agent row asks for its rows when it is promoted.
    if (!current && getThreadById(threadId)?.agentPromotedAt == null) return;
    const rows = (current?.rows ?? []).filter((row) => row.id !== id);
    rows.push(change.row);
    setRows(threadId, (current?.backend ?? threadBackend(threadId) ?? backend) as BackendKey, rows);
    return;
  }
  if (!current) return;
  if (change.removed) {
    setRows(threadId, current.backend, current.rows.filter((row) => row.id !== id));
    return;
  }
  // Neither row nor removed: the backend could not read the row. Read the
  // thread again.
  forgetAgentWakes(threadId);
  ensureAgentWakes(threadId);
});

onStandingAgentEnded(forgetAgentWakes);

onBackendDetached(({ threadIds }) => {
  if (threadIds.length === 0) return;
  let changed = false;
  const next = new Map(entries);
  for (const threadId of threadIds) {
    known.delete(threadId);
    if (next.delete(threadId)) changed = true;
  }
  if (changed) entries = next;
});

/** Test helper. */
export function resetAgentWakesForTest(): void {
  entries = new Map();
  known.clear();
  pending.clear();
}
