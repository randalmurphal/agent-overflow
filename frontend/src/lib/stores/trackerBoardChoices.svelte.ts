// The boards the Issues pane's picker lists: every board some project lists,
// on every attached computer that grants tracker:read, one
// `TrackerBoardChoices` answer per computer, replaced whole. Read when the
// pane mounts, when the picker opens and when Settings closes over the
// pane; nothing pushes a change, and no read reaches a tracker. A newer
// read of a computer wins over one still out; a failed read keeps the
// computer's last answer beside its error, and a read that could not reach
// the computer keeps it without one. A detached computer's rows go.

import { TrackerBoardChoices } from './bindings';
import { captureError, type CapturedError } from './errorReports.svelte';
import type { BackendKey } from '../transport/backendKey';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { getAttachedBackends } from './attachedBackends.svelte';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { hasScope } from '../transport/scopes';
import type { BoardChoice } from '../../../bindings/agent-overflow/internal/trackerapp/models';

/** A board a project lists, with the computer that lists it. */
export interface ListedBoardChoice {
  readonly backend: BackendKey;
  readonly choice: BoardChoice;
}

interface ComputerChoices {
  /** The last answer; null before one arrived. */
  readonly rows: readonly BoardChoice[] | null;
  readonly error: CapturedError | null;
}

let byComputer = $state.raw<ReadonlyMap<BackendKey, ComputerChoices>>(new Map());
const generation = new Map<BackendKey, number>();

function put(backend: BackendKey, next: ComputerChoices): void {
  const map = new Map(byComputer);
  map.set(backend, next);
  byComputer = map;
}

async function readComputer(backend: BackendKey): Promise<void> {
  const mine = (generation.get(backend) ?? 0) + 1;
  generation.set(backend, mine);
  try {
    const rows = await withBackendTarget(backend, () => TrackerBoardChoices());
    if (generation.get(backend) === mine) put(backend, { rows, error: null });
  } catch (err) {
    if (generation.get(backend) !== mine) return;
    const kept = byComputer.get(backend)?.rows ?? null;
    put(backend, { rows: kept, error: isPassiveConnectionFailure(err) ? null : captureError(err, { context: 'Could not list the boards' }) });
  }
}

/** Reads every computer that grants tracker:read; settles when each did. */
export function readTrackerBoardChoices(): Promise<void> {
  const reads = getAttachedBackends()
    .filter((entry) => hasScope('tracker:read', entry.id))
    .map((entry) => readComputer(entry.id));
  return Promise.all(reads).then(() => undefined);
}

/** Every listed board, computer by computer in attach order; reactive. */
export function trackerBoardChoices(): readonly ListedBoardChoice[] {
  const order = getAttachedBackends().map((entry) => entry.id);
  return order.flatMap((backend) => (byComputer.get(backend)?.rows ?? []).map((choice) => ({ backend, choice })));
}

/** Whether a computer that grants tracker:read has not settled a read yet. */
export function trackerBoardChoicesPending(): boolean {
  return getAttachedBackends().some((entry) => hasScope('tracker:read', entry.id) && !byComputer.has(entry.id));
}

/** The computers whose last read failed, with the failure. */
export function trackerBoardChoiceErrors(): readonly { backend: BackendKey; error: CapturedError }[] {
  return [...byComputer].flatMap(([backend, c]) => (c.error ? [{ backend, error: c.error }] : []));
}

onBackendDetached(({ backendId }) => {
  generation.delete(backendId);
  if (!byComputer.has(backendId)) return;
  const map = new Map(byComputer);
  map.delete(backendId);
  byComputer = map;
});

export function resetTrackerBoardChoicesForTest(): void {
  byComputer = new Map();
  generation.clear();
}
