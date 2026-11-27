// The entries of one agent's time_draft call, as its card in the thread
// shows them (docs/specs/issues.md, "Time"; ThreadTimeEntries in
// docs/architecture/tracker-backend.md). The call's timeline row names the
// call, its connection and its days and is never rewritten; what the card
// shows is read live from the thread's computer: the call's entries that
// still exist, in their state now.
//
// One card per computer, thread and call, held while a card shows it and
// dropped with its last holder, so only the cards the timeline mounts hold
// entries (at most a call's 250). Reads are numbered as they start, and
// an answer, a refusal included, applies only above the one applied last. The card reads when
// it is first held; a `tracker:time_entries_updated` frame of its
// computer that names its thread reads it again, coalesced
// (`DRAFT_CARD_REREAD_DELAY_MS`), since the frame names the threads of
// every entry it changed, a question folded into another call's entry
// included; a gap on the channel and a reconnect read every card of that
// computer again.
//
// What the person changes from a card goes through
// stores/trackerTimeWrites.svelte.ts, under the same targets as the Time
// screen's, so both show one write.

import { SvelteMap } from 'svelte/reactivity';
import type { TimeEntryView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { ThreadTimeEntries } from './bindings';
import { takeTrackerTimeEntriesChannel } from './trackerBoardEvents';
import { onBackendStatusChange } from './transportStatus.svelte';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { createRefreshScheduler } from '../utils/refreshScheduler';

/** One time_draft call of a thread, on the computer the thread runs on. */
export interface DraftCallRef {
  readonly backend: BackendKey;
  readonly thread: string;
  readonly call: string;
}

/** A call's entries as the card draws them. Replaced whole on every
 * change. */
export interface DraftCallState {
  /** The call's entries as last read; null before the first answer. */
  readonly entries: readonly TimeEntryView[] | null;
  /** The error of the last read the computer refused (any but a dropped
   * connection); null once a later read answers. */
  readonly readError: unknown;
}

export interface DraftCallAttachment {
  readonly key: string;
  readonly current: DraftCallState;
  release(): void;
}

interface Card {
  readonly ref: DraftCallRef;
  holders: number;
  started: number;
  applied: number;
}

const EMPTY: DraftCallState = { entries: null, readError: null };

const cards = new Map<string, Card>();
const states = new SvelteMap<string, DraftCallState>();

export function draftCallKey(ref: DraftCallRef): string {
  return `${ref.backend}\n${ref.thread}\n${ref.call}`;
}

function read(card: Card): Promise<void> {
  const { backend, thread, call } = card.ref;
  const key = draftCallKey(card.ref);
  const n = ++card.started;
  return withBackendTarget(backend, () => ThreadTimeEntries(thread, call)).then(
    (entries) => {
      if (n <= card.applied || cards.get(key) !== card) return;
      card.applied = n;
      states.set(key, { entries: entries ?? [], readError: null });
    },
    (err: unknown) => {
      // A dropped connection says nothing new: the reconnect reads again.
      if (n <= card.applied || cards.get(key) !== card || isPassiveConnectionFailure(err)) return;
      // A refusal is an answer too: an older read that answers later does
      // not clear it.
      card.applied = n;
      states.set(key, { ...(states.get(key) ?? EMPTY), readError: err });
    },
  );
}

/** Holds a call's card until the release; the first holder reads it. */
export function attachDraftCall(ref: DraftCallRef): DraftCallAttachment {
  const key = draftCallKey(ref);
  let card = cards.get(key);
  if (!card) {
    card = { ref, holders: 0, started: 0, applied: 0 };
    cards.set(key, card);
    states.set(key, EMPTY);
    void read(card);
  }
  const held = card;
  held.holders += 1;
  let released = false;
  return {
    key,
    get current() {
      return states.get(key) ?? EMPTY;
    },
    release() {
      if (released) return;
      released = true;
      held.holders -= 1;
      if (held.holders > 0 || cards.get(key) !== held) return;
      cards.delete(key);
      states.delete(key);
      changed.delete(held);
    },
  };
}

/** The coalescing of a burst of entry frames into one read per card. */
export const DRAFT_CARD_REREAD_DELAY_MS = 100;

// The cards a frame named since the last coalesced read.
const changed = new Set<Card>();
const rereads = createRefreshScheduler({
  name: 'trackerTimeDraftCards',
  delayMs: DRAFT_CARD_REREAD_DELAY_MS,
  maxWaitMs: 5 * DRAFT_CARD_REREAD_DELAY_MS,
  run: () => {
    const due = [...changed];
    changed.clear();
    for (const card of due) {
      if (cards.get(draftCallKey(card.ref)) === card) void read(card);
    }
  },
});

function cardsOf(backend: string): Card[] {
  return [...cards.values()].filter((c) => c.ref.backend === backend);
}

takeTrackerTimeEntriesChannel({
  event(event, backend) {
    const threads = new Set(event.threads ?? []);
    for (const card of cardsOf(backend)) {
      if (threads.has(card.ref.thread)) changed.add(card);
    }
    if (changed.size > 0) rereads.request();
  },
  gap(backend) {
    const reads = cardsOf(backend).map(read);
    return reads.length > 0 ? Promise.all(reads).then(() => undefined) : undefined;
  },
});

// The computer connected again: its entries may have changed meanwhile.
onBackendStatusChange((backend, status) => {
  if (status.status !== 'connected') return;
  for (const card of cardsOf(backend)) void read(card);
});

export function resetTrackerTimeDraftForTest(): void {
  cards.clear();
  states.clear();
  changed.clear();
  rereads.reset();
}
