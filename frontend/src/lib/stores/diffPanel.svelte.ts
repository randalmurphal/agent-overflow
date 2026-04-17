// Reactive state container for the three-source diff panel.
//
// Each chat pane owns one instance. The panel renders three sources:
//   * 'turn'     — per-turn git checkpoints from Wave 4C
//   * 'worktree' — uncommitted git working tree diff
//   * 'cumulative' — aggregate of all kind==='diff' items in the pane
//
// Turn diffs can be viewed in two compare modes: 'next' (turn N -> turn N+1)
// and 'worktree' (turn N -> current worktree). The store drives every tab,
// selected-turn, and compare-mode transition.
//
// Caches:
//   * `turnDiffCache`   — bounded LRU keyed by "<threadId>|<turnIndex>|<mode>"
//   * `cumulativeCache` — unbounded per-payloadId cache (payload ids are
//     content-addressed, so they're effectively immutable once fetched)
// Both caches are cleared when the pane switches threads so stale rows from a
// prior thread can't leak.

import type { Checkpoint } from '../types/checkpoint';

/** Distinct data sources the panel can display. */
export type DiffPanelSource = 'turn' | 'worktree' | 'cumulative';

/** Comparison modes for turn diffs. */
export type TurnCompareMode = 'next' | 'worktree';

/** Visual mode for diff rendering. */
export type DiffViewMode = 'stacked' | 'split';

const TURN_CACHE_LIMIT = 16;

export interface DiffPanelState {
  readonly open: boolean;
  readonly source: DiffPanelSource;
  readonly turnCompareMode: TurnCompareMode;
  readonly viewMode: DiffViewMode;
  readonly selectedTurnIndex: number | null;
  readonly checkpoints: Checkpoint[];
  readonly checkpointsLoaded: boolean;
  readonly checkpointsUnavailable: boolean;
  readonly checkpointsUnavailableReason: string | null;
  readonly error: string | null;

  open_(): void;
  close(): void;
  toggle(): void;
  setSource(source: DiffPanelSource): void;
  setTurnCompareMode(mode: TurnCompareMode): void;
  setViewMode(mode: DiffViewMode): void;
  selectTurn(turnIndex: number | null): void;
  setCheckpoints(checkpoints: Checkpoint[]): void;
  markCheckpointsUnavailable(reason: string): void;
  setError(message: string | null): void;
  clearForThread(): void;

  // LRU-backed turn diff cache.
  readTurnDiff(threadId: string, turnIndex: number, mode: TurnCompareMode): string | undefined;
  writeTurnDiff(
    threadId: string,
    turnIndex: number,
    mode: TurnCompareMode,
    text: string,
  ): void;

  // Cumulative per-payload cache.
  readonly cumulativeCache: Map<string, string>;
  invalidateCumulative(): void;
}

/**
 * Create a panel state store. One instance per pane (see pane.diffPanel).
 */
export function createDiffPanelState(): DiffPanelState {
  let open = $state(false);
  let source: DiffPanelSource = $state('turn');
  let turnCompareMode: TurnCompareMode = $state('next');
  let viewMode: DiffViewMode = $state('stacked');
  let selectedTurnIndex: number | null = $state(null);
  let checkpoints: Checkpoint[] = $state([]);
  let checkpointsLoaded = $state(false);
  let checkpointsUnavailable = $state(false);
  let checkpointsUnavailableReason: string | null = $state(null);
  let error: string | null = $state(null);

  // LRU cache for turn diffs. Map iteration order gives us insertion order,
  // which we exploit for eviction.
  const turnDiffCache = new Map<string, string>();
  const cumulativeCache = new Map<string, string>();

  function turnKey(threadId: string, turnIndex: number, mode: TurnCompareMode): string {
    return `${threadId}|${turnIndex}|${mode}`;
  }

  function evictIfOversize(): void {
    while (turnDiffCache.size > TURN_CACHE_LIMIT) {
      const first = turnDiffCache.keys().next();
      if (first.done) break;
      turnDiffCache.delete(first.value);
    }
  }

  return {
    get open() { return open; },
    get source() { return source; },
    get turnCompareMode() { return turnCompareMode; },
    get viewMode() { return viewMode; },
    get selectedTurnIndex() { return selectedTurnIndex; },
    get checkpoints() { return checkpoints; },
    get checkpointsLoaded() { return checkpointsLoaded; },
    get checkpointsUnavailable() { return checkpointsUnavailable; },
    get checkpointsUnavailableReason() { return checkpointsUnavailableReason; },
    get error() { return error; },
    get cumulativeCache() { return cumulativeCache; },

    open_() { open = true; },
    close() { open = false; },
    toggle() { open = !open; },

    setSource(next) {
      source = next;
      // Clear any transient error when the user navigates away; each source
      // owns its own failure state.
      error = null;
    },

    setTurnCompareMode(mode) { turnCompareMode = mode; },
    setViewMode(mode) { viewMode = mode; },
    selectTurn(turnIndex) { selectedTurnIndex = turnIndex; },

    setCheckpoints(next) {
      checkpoints = [...next].sort((a, b) => a.turnIndex - b.turnIndex);
      checkpointsLoaded = true;
      if (next.length > 0) {
        checkpointsUnavailable = false;
        checkpointsUnavailableReason = null;
      }
    },

    markCheckpointsUnavailable(reason) {
      checkpointsUnavailable = true;
      checkpointsUnavailableReason = reason;
      checkpointsLoaded = true;
      // If the workspace isn't a git repo, keep the list empty.
      checkpoints = [];
    },

    setError(message) { error = message; },

    clearForThread() {
      open = false;
      source = 'turn';
      turnCompareMode = 'next';
      viewMode = 'stacked';
      selectedTurnIndex = null;
      checkpoints = [];
      checkpointsLoaded = false;
      checkpointsUnavailable = false;
      checkpointsUnavailableReason = null;
      error = null;
      turnDiffCache.clear();
      cumulativeCache.clear();
    },

    readTurnDiff(threadId, turnIndex, mode) {
      const key = turnKey(threadId, turnIndex, mode);
      const hit = turnDiffCache.get(key);
      if (hit === undefined) return undefined;
      // Touch to refresh LRU position: re-insert moves the entry to the tail.
      turnDiffCache.delete(key);
      turnDiffCache.set(key, hit);
      return hit;
    },

    writeTurnDiff(threadId, turnIndex, mode, text) {
      const key = turnKey(threadId, turnIndex, mode);
      turnDiffCache.delete(key);
      turnDiffCache.set(key, text);
      evictIfOversize();
    },

    invalidateCumulative() {
      cumulativeCache.clear();
    },
  };
}

export const TURN_DIFF_CACHE_LIMIT = TURN_CACHE_LIMIT;
