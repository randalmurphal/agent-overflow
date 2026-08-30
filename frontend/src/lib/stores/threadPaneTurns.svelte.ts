// stores/threadPaneTurns.svelte.ts
//
// OWNS the pane's turn-lifecycle vocabulary: `latestSettledTurn` (the only
// turn fact that is per-PANE), the `TimelineTurnFacet` rows read their
// response decorations from, and the four mutations that project a wire
// turn event onto both — start, settle, optimistic clear, and reset.
//
// MUST NOT own the ACTIVE turn. That lives in the global registry in
// `threadStatuses.svelte.ts`, keyed by thread, precisely so switching panes
// cannot clear a turn still in flight; every read here goes back through
// `getActiveTurn`. It also owns no timeline rows — the deferred window
// prune a settle releases arrives as a callback.

import { itemTurnIndexKey } from '../utils/subagentGrouping';
import type { Thread } from '../types/models';
import {
  getActiveTurn,
  projectTurnCompleted,
  projectTurnStarted,
  type ActiveTurn,
} from './threadStatuses.svelte';
import type { SettledTurn, TimelineTurnFacet } from './threadTurnProjection';

export interface ThreadPaneTurnsOptions {
  getThread(): Thread | null;
  /**
   * Release the deferred recent-window prune a settled turn unblocks
   * (`threadTimelineWindow.settleRecentWindowPrune`).
   */
  settleRecentWindowPrune(): void;
}

export function createThreadPaneTurns(options: ThreadPaneTurnsOptions) {
  // Turn-lifecycle state. The active turn lives in the global registry
  // in threadStatuses.svelte.ts (read directly via `getActiveTurn` at
  // every call site so the source of truth is traceable); the load-
  // bearing benefit is that switching threads no longer clears the
  // working indicator for a turn that's still in flight on the
  // departing thread. `latestSettledTurn` stays per-pane for read-state
  // and trace/debug consumers; on thread switch we rehydrate it from the
  // most recent `ListRecentTurns` row whose `completedAt` is non-null.
  let latestSettledTurn: SettledTurn | null = $state(null);

  // Turn identity for the timeline's response decorations
  // (`TimelineTurnFacet`): the provider turn. One object for the pane's
  // lifetime — the getters read the live signals — so the per-row pill
  // lookup allocates nothing. The agent pane's scoped facade overrides
  // this with its launch's own lifecycle.
  const timelineTurns: TimelineTurnFacet = {
    keyOf: itemTurnIndexKey,
    get activeKey() {
      return getActiveTurn(options.getThread()?.id)?.turnIndex ?? null;
    },
    get settled() {
      const settled = latestSettledTurn;
      if (!settled) return null;
      return {
        key: settled.turnIndex,
        startedAt: settled.startedAt,
        completedAt: settled.completedAt,
      };
    },
  };

  return {
    /**
     * Most recent completed turn, or null if the thread has no settled
     * turns yet. Populated from `provider:turn_completed` pushes and
     * from thread-switch rehydration.
     */
    get latestSettledTurn() {
      return latestSettledTurn;
    },
    /** Thread-switch rehydration's write (threadSwitchLoad). */
    setLatestSettledTurn(next: SettledTurn | null): void {
      latestSettledTurn = next;
    },
    timelineTurns,

    /**
     * Flip the pane into "turn in flight" on `provider:turn_started`. Safe
     * to call repeatedly — a re-emission (Claude re-init after interrupt)
     * maps back to the same turnId and leaves startedAt as the
     * authoritative first-wall-clock the working indicator anchors on.
     * Idempotent by turnId: a second call with the same id preserves the
     * existing startedAt so the on-screen counter doesn't reset mid-turn.
     *
     * Pane facade for `provider:turn_started`. Production goes through
     * the wire-push handler in eventsProvider.ts → projectTurnStarted
     * directly; this method is the test-and-explicit-control entry point.
     */
    setActiveTurn(turn: ActiveTurn): void {
      const tid = options.getThread()?.id ?? '';
      if (!tid) return;
      projectTurnStarted(tid, turn.turnId, turn.turnIndex, turn.startedAt);
    },

    /**
     * Settle the current turn on `provider:turn_completed`. Writes
     * `latestSettledTurn` for thread-switch rehydration/read state and
     * clears the global active-turn registry via projectTurnCompleted.
     */
    settleTurn(settled: SettledTurn): void {
      const tid = options.getThread()?.id ?? '';
      if (tid) {
        projectTurnCompleted(tid, settled.turnId, {
          aborted: settled.aborted,
          errorMessage: settled.errorMessage,
        });
      }
      latestSettledTurn = settled;
      // Any smoother still behind keeps revealing at the normal cadence
      // (adaptive catch-up, PerItemSmoother) — there is deliberately no
      // end-of-turn drain, and no successor-waiting one either: rushed
      // reveal motion read as jank both times. A long final message
      // finishing a few seconds after the wire settles is the accepted
      // trade for uniform reveal speed. Nothing is skipped to shorten that
      // wait either — the bursty wire's idle gaps are what let the drain
      // catch back up, so a queued row's wait is transient without a rush.
      // The deferred window prune does NOT run here: wire settle is not
      // visual quiet — the reveal above keeps draining for seconds. A
      // mounted timeline records the prune as pending and the quiet
      // scheduler (timelineQuietWork) runs it once nothing is animating;
      // a pane with no timeline behind it prunes immediately.
      options.settleRecentWindowPrune();
    },

    /**
     * Optimistic clear used by the Esc / Stop interrupt path. Drops
     * the live turn from the global registry synchronously so the
     * spinner / Stop button flip to idle in the same render tick as
     * the keystroke — matching Claude Code's `resetLoadingState()`
     * (REPL.tsx:2106-2163) and the Codex TUI's spinner clear on
     * `EventMsg::TurnAborted`. The real `provider:turn_completed`
     * arrives shortly after and re-runs the same path (idempotent on
     * already-cleared registry). Does NOT clear `latestSettledTurn`
     * so read-state/trace surfaces keep the previous settled turn.
     */
    clearActiveTurn(): void {
      const tid = options.getThread()?.id ?? '';
      if (!tid) return;
      const current = getActiveTurn(tid);
      if (current) {
        projectTurnCompleted(tid, current.turnId, { aborted: true });
      }
    },

    /**
     * Reset both turn-lifecycle slots without rehydrating. Used by
     * the frontend on explicit "clear this pane" paths that aren't a
     * full switchThread — e.g. a user-triggered stop that leaves the
     * pane in a known-quiet state until the next wire push.
     */
    clearTurnState(): void {
      const tid = options.getThread()?.id ?? '';
      if (tid) {
        const current = getActiveTurn(tid);
        if (current) {
          projectTurnCompleted(tid, current.turnId, { aborted: true });
        }
      }
      latestSettledTurn = null;
    },
  };
}

export type ThreadPaneTurns = ReturnType<typeof createThreadPaneTurns>;
