import type { Checkpoint, DiffPanelTab } from '../types/checkpoint';

/** Visual mode for diff rendering. */
export type DiffViewMode = 'stacked' | 'split';

export interface DiffPanelState {
  readonly open: boolean;
  readonly viewMode: DiffViewMode;
  readonly tabMode: DiffPanelTab;
  readonly selectedCheckpointTurnCount: number | null;
  readonly checkpoints: Checkpoint[];
  readonly checkpointsLoaded: boolean;
  readonly checkpointsUnavailable: boolean;
  readonly checkpointsUnavailableReason: string | null;
  readonly error: string | null;

  open_(): void;
  close(): void;
  toggle(): void;
  setViewMode(mode: DiffViewMode): void;
  setTabMode(mode: DiffPanelTab): void;
  selectCheckpointTurnCount(turnCount: number | null): void;
  setCheckpoints(checkpoints: Checkpoint[]): void;
  markCheckpointsUnavailable(reason: string): void;
  setError(message: string | null): void;
  clearForThread(): void;
}

/**
 * Create the checkpoint diff drawer state. Each pane owns one instance.
 *
 * Diffs are fetched by checkpoint range, so the store tracks only drawer UI
 * state and checkpoint availability. It deliberately does not cache patch text:
 * checkpoint refs are cheap to diff, and keeping the cache out of pane state
 * avoids stale turn/worktree/cumulative modes leaking back into the UI.
 */
export function createDiffPanelState(): DiffPanelState {
  let open = $state(false);
  let viewMode: DiffViewMode = $state('stacked');
  let tabMode: DiffPanelTab = $state('per-turn');
  let selectedCheckpointTurnCount: number | null = $state(null);
  let checkpoints: Checkpoint[] = $state([]);
  let checkpointsLoaded = $state(false);
  let checkpointsUnavailable = $state(false);
  let checkpointsUnavailableReason: string | null = $state(null);
  let error: string | null = $state(null);

  return {
    get open() { return open; },
    get viewMode() { return viewMode; },
    get tabMode() { return tabMode; },
    get selectedCheckpointTurnCount() { return selectedCheckpointTurnCount; },
    get checkpoints() { return checkpoints; },
    get checkpointsLoaded() { return checkpointsLoaded; },
    get checkpointsUnavailable() { return checkpointsUnavailable; },
    get checkpointsUnavailableReason() { return checkpointsUnavailableReason; },
    get error() { return error; },

    open_() { open = true; },
    close() { open = false; },
    toggle() { open = !open; },
    setViewMode(mode) { viewMode = mode; },
    setTabMode(mode) { tabMode = mode; },
    selectCheckpointTurnCount(turnCount) { selectedCheckpointTurnCount = turnCount; },

    setCheckpoints(next) {
      checkpoints = [...next].sort((a, b) => a.checkpointTurnCount - b.checkpointTurnCount);
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
      checkpoints = [];
    },

    setError(message) { error = message; },

    clearForThread() {
      open = false;
      viewMode = 'stacked';
      tabMode = 'per-turn';
      selectedCheckpointTurnCount = null;
      checkpoints = [];
      checkpointsLoaded = false;
      checkpointsUnavailable = false;
      checkpointsUnavailableReason = null;
      error = null;
    },
  };
}
