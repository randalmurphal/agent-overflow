// Visibility votes for a backend pump's subscription handles
// (SetPRUpdatesActive, SetPRListActive, SetPipelineActive).
//
// Votes are LAST-WRITE-WINS on the backend, but the transport dispatches
// concurrently: a hidden→visible flurry (a window minimised and restored,
// a tab flipped) can complete out of order and leave the pump holding
// `false` for a client that is on screen, a surface that silently stops
// updating until the next visibility change. So one vote is in flight per
// handle at a time, with the newest value trailing behind it;
// intermediate values are dropped, which is what "latest wins" means when
// the far side only cares about the final state.

import type { BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { composeWorkspaceKey, workspaceKeyBackend, workspaceKeyPath } from '../utils/workspaceKey';

interface Vote {
  sending: boolean;
  pending: boolean | null;
}

export interface PumpVotes {
  /** Records the handle's vote; sent at once or behind the one in flight. */
  setActive(backend: BackendKey, id: string, active: boolean): void;
  resetForTest(): void;
}

/**
 * Votes sent through `send`, pinned to the computer whose subscribe
 * answered. A failed vote is not user-surfaced: a failed pause keeps the
 * status quo (the pump keeps polling), and a failed resume implies a dying
 * transport whose server-side connection cleanup closes the pump anyway.
 */
export function createPumpVotes(name: string, send: (id: string, active: boolean) => Promise<void>): PumpVotes {
  const votes = new Map<string, Vote>();

  async function drain(key: string, vote: Vote): Promise<void> {
    const id = workspaceKeyPath(key);
    const backend = workspaceKeyBackend(key);
    vote.sending = true;
    try {
      while (vote.pending !== null) {
        const active = vote.pending;
        vote.pending = null;
        try {
          await withBackendTarget(backend, () => send(id, active));
        } catch (err) {
          console.error(`${name}: visibility vote failed`, { id, active, err });
        }
      }
    } finally {
      vote.sending = false;
      // Nothing arrived while the last send was in flight, so this handle
      // owes no vote: drop the bookkeeping rather than keep an entry per
      // handle the app ever made.
      if (vote.pending === null && votes.get(key) === vote) votes.delete(key);
    }
  }

  return {
    setActive(backend, id, active) {
      const key = composeWorkspaceKey(backend, id);
      let vote = votes.get(key);
      if (!vote) {
        vote = { sending: false, pending: null };
        votes.set(key, vote);
      }
      vote.pending = active;
      if (vote.sending) return;
      void drain(key, vote);
    },
    resetForTest() {
      votes.clear();
    },
  };
}
