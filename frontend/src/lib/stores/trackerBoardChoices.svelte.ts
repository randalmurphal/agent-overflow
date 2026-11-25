// The boards the Issues pane picks from and opens: every board some project
// lists, on every attached computer that grants tracker:read, one
// `TrackerBoardChoices` answer per computer, replaced whole. No read reaches
// a tracker.
//
// Held while a pane shows it (`acquireTrackerBoardChoices`); nothing is read
// while nobody holds it, and the last release drops every answer. While
// held, the store reads a computer:
//   - when the hold starts, and when a computer joins the computers that
//     grant tracker:read (attached, or its grant arrived);
//   - when the computer's connection comes back;
//   - when the boards its projects list may have changed: its
//     `tracker:connections_updated`, or a save of a project's boards in this
//     window (`trackerBoardEvents.ts#trackerBoardListsChanged`);
//   - when the picker opens (`readTrackerBoardChoices`), which is how a save
//     in another window reaches the picker, since that sends no event.
//
// Each computer is in one state: `reading` before its first answer,
// `answered`, `offline` when it could not be reached and never answered (one
// that answered keeps its answer), or `failed` with the last answer kept
// beside the error. A computer whose grant has not arrived counts as
// `reading`, and as `offline` once it has been unreachable for the settle
// time. A newer read of a computer wins over one still out. A detached
// computer's answer goes.

import { untrack } from 'svelte';
import { TrackerBoardChoices } from './bindings';
import { captureError, type CapturedError } from './errorReports.svelte';
import { getAttachedBackends } from './attachedBackends.svelte';
import { onTrackerBoardListsChanged } from './trackerBoardEvents';
import { backendUnreachableSettled, onBackendStatusChange } from './transportStatus.svelte';
import type { BackendKey } from '../transport/backendKey';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { scopeGrant } from '../transport/scopes';
import type { BoardChoice } from '../../../bindings/agent-overflow/internal/trackerapp/models';

/** A board a project lists, with the computer that lists it. */
export interface ListedBoardChoice {
  readonly backend: BackendKey;
  readonly choice: BoardChoice;
}

/** What is known of one computer's boards. */
export type ComputerChoices =
  | { readonly kind: 'reading'; readonly rows: null }
  | { readonly kind: 'answered'; readonly rows: readonly BoardChoice[] }
  | { readonly kind: 'offline'; readonly rows: null }
  | { readonly kind: 'failed'; readonly rows: readonly BoardChoice[] | null; readonly error: CapturedError };

interface Held {
  readonly state: ComputerChoices;
  /** The read the state is the answer of; 0 for none. */
  readonly answeredBy: number;
}

const READING: ComputerChoices = { kind: 'reading', rows: null };
const OFFLINE: ComputerChoices = { kind: 'offline', rows: null };

let byComputer = $state.raw<ReadonlyMap<BackendKey, Held>>(new Map());
// The last read started of each computer, numbered across holds so an
// answer of a read from an earlier hold never lands.
const started = new Map<BackendKey, number>();
let reads = 0;
let holders = 0;
let stopEffects: (() => void) | null = null;

function put(backend: BackendKey, next: Held): void {
  const map = new Map(byComputer);
  map.set(backend, next);
  byComputer = map;
}

function grants(backend: BackendKey): boolean {
  return scopeGrant('tracker:read', backend) === 'granted';
}

async function readComputer(backend: BackendKey): Promise<void> {
  const mine = ++reads;
  started.set(backend, mine);
  try {
    const rows = await withBackendTarget(backend, () => TrackerBoardChoices());
    if (started.get(backend) === mine) put(backend, { state: { kind: 'answered', rows }, answeredBy: mine });
  } catch (err) {
    if (started.get(backend) !== mine) return;
    const was = byComputer.get(backend);
    const rows = was?.state.rows ?? null;
    if (isPassiveConnectionFailure(err)) {
      // The reconnect reads it again.
      if (!rows) put(backend, { state: OFFLINE, answeredBy: was?.answeredBy ?? 0 });
      return;
    }
    put(backend, { state: { kind: 'failed', rows, error: captureError(err, { context: 'Could not list the boards' }) }, answeredBy: was?.answeredBy ?? 0 });
  }
}

function readIfHeld(backend: BackendKey): void {
  if (holders > 0 && grants(backend)) void readComputer(backend);
}

/**
 * Holds the boards while the caller shows them, reading every computer
 * that grants tracker:read now and on the triggers above; the returned
 * function releases the hold. Call it where an effect may be created (a
 * component's setup).
 */
export function acquireTrackerBoardChoices(): () => void {
  holders += 1;
  if (holders === 1) {
    stopEffects = $effect.root(() => {
      // Each computer is read once when it joins the computers that grant
      // tracker:read; the other triggers read it again.
      const seen = new Set<BackendKey>();
      $effect(() => {
        const granted = getAttachedBackends().map((entry) => entry.id).filter(grants);
        untrack(() => {
          for (const backend of granted) {
            if (seen.has(backend)) continue;
            seen.add(backend);
            void readComputer(backend);
          }
          for (const backend of seen) if (!granted.includes(backend)) seen.delete(backend);
        });
      });
    });
  }
  let released = false;
  return () => {
    if (released) return;
    released = true;
    holders -= 1;
    if (holders > 0) return;
    stopEffects?.();
    stopEffects = null;
    started.clear();
    byComputer = new Map();
  };
}

/** Reads every computer that grants tracker:read again, while held: the
 * picker opened. */
export function readTrackerBoardChoices(): void {
  for (const entry of getAttachedBackends()) readIfHeld(entry.id);
}

/** The computers whose boards the pane waits on or tells about, in attach
 * order, each with its state; reactive. A computer that refuses
 * tracker:read is not one of them. */
export function trackerBoardChoiceComputers(): readonly { readonly backend: BackendKey; readonly state: ComputerChoices }[] {
  const out: { backend: BackendKey; state: ComputerChoices }[] = [];
  for (const entry of getAttachedBackends()) {
    const grant = scopeGrant('tracker:read', entry.id);
    if (grant === 'refused') continue;
    if (grant === 'pending') {
      out.push({ backend: entry.id, state: backendUnreachableSettled(entry.id) ? OFFLINE : READING });
      continue;
    }
    out.push({ backend: entry.id, state: byComputer.get(entry.id)?.state ?? READING });
  }
  return out;
}

/** What is known of one computer's boards; null for a computer that is
 * not attached or refuses tracker:read. Reactive. */
export function trackerBoardChoicesOf(backend: BackendKey): ComputerChoices | null {
  return trackerBoardChoiceComputers().find((c) => c.backend === backend)?.state ?? null;
}

/** The number of the last read of `backend` started, for a caller that
 * waits on an answer read after something happened; 0 for none. */
export function trackerBoardChoicesReadStarted(backend: BackendKey): number {
  return started.get(backend) ?? 0;
}

/** The number of the read whose answer `backend`'s rows are; 0 for none.
 * Reactive. */
export function trackerBoardChoicesAnsweredBy(backend: BackendKey): number {
  return byComputer.get(backend)?.answeredBy ?? 0;
}

/** Every listed board, computer by computer in attach order; reactive. */
export function trackerBoardChoices(): readonly ListedBoardChoice[] {
  return trackerBoardChoiceComputers().flatMap(({ backend, state }) => (state.rows ?? []).map((choice) => ({ backend, choice })));
}

/** Whether a board can be opened: the picker offers it, and the pane opens
 * it on its own. A board whose connection is turned off cannot. */
export function isBoardChoiceOpenable(choice: BoardChoice): boolean {
  return choice.connection_enabled;
}

onBackendStatusChange((backend, status) => {
  if (status.status === 'connected') readIfHeld(backend);
});

onTrackerBoardListsChanged(readIfHeld);

onBackendDetached(({ backendId }) => {
  started.delete(backendId);
  if (!byComputer.has(backendId)) return;
  const map = new Map(byComputer);
  map.delete(backendId);
  byComputer = map;
});

export function resetTrackerBoardChoicesForTest(): void {
  stopEffects?.();
  stopEffects = null;
  holders = 0;
  started.clear();
  byComputer = new Map();
}
