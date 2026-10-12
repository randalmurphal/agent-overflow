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

interface AgentWakeEntry {
  backend: BackendKey;
  rows: ThreadWakeRow[];
}

const NO_ROWS: readonly ThreadWakeRow[] = Object.freeze([]);

let entries = $state<Map<string, AgentWakeEntry>>(new Map());
/** Threads whose own read is in flight, so a row asks once. */
const pending = new Set<string>();

/** The agent's live loops and watches; the shared empty array when none. */
export function getAgentWakeRows(threadId: string): readonly ThreadWakeRow[] {
  return entries.get(threadId)?.rows ?? NO_ROWS;
}

function setRows(threadId: string, backend: BackendKey, rows: readonly ThreadWakeRow[]): void {
  const live = agentWakeRows(rows);
  const next = new Map(entries);
  if (live.length === 0) next.delete(threadId);
  else next.set(threadId, { backend, rows: live });
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
 * promoted after boot keeps the loops it registered before. Asked by the
 * row on mount; a thread that already has rows, or whose read is out, is
 * left alone.
 */
export function ensureAgentWakes(threadId: string): void {
  if (entries.has(threadId) || pending.has(threadId)) return;
  const backend = threadBackend(threadId) ?? HOME_BACKEND;
  pending.add(threadId);
  void withBackendTarget(backend, () => ListThreadWakes(threadId)).then((wakes) => {
    setRows(threadId, backend, wakes?.rows ?? []);
  }).catch((err: unknown) => {
    if (!isPassiveConnectionFailure(err)) console.error(`Failed to load wakes of ${threadId}:`, err);
  }).finally(() => {
    pending.delete(threadId);
  });
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
  const next = new Map(entries);
  next.delete(threadId);
  entries = next;
  ensureAgentWakes(threadId);
});

onBackendDetached(({ threadIds }) => {
  if (threadIds.length === 0) return;
  const gone = new Set(threadIds);
  let changed = false;
  const next = new Map(entries);
  for (const threadId of next.keys()) {
    if (gone.has(threadId)) {
      next.delete(threadId);
      changed = true;
    }
  }
  if (changed) entries = next;
});

/** Test helper. */
export function resetAgentWakesForTest(): void {
  entries = new Map();
  pending.clear();
}
