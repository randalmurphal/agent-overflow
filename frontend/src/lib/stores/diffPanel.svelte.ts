import type { Checkpoint, DiffPanelTab } from '../types/checkpoint';

/** Visual mode for diff rendering. */
export type DiffViewMode = 'stacked' | 'split';

export interface DiffPanelState {
  readonly open: boolean;
  readonly viewMode: DiffViewMode;
  readonly tabMode: DiffPanelTab;
  readonly selectedCheckpointUserItemId: string | null;
  readonly checkpoints: Checkpoint[];
  readonly checkpointUserItemIds: ReadonlySet<string>;
  readonly checkpointsLoaded: boolean;
  readonly checkpointsUnavailable: boolean;
  readonly checkpointsUnavailableReason: string | null;
  readonly error: string | null;

  open_(): void;
  close(): void;
  toggle(): void;
  setViewMode(mode: DiffViewMode): void;
  setTabMode(mode: DiffPanelTab): void;
  selectCheckpointUserItem(userItemId: string | null): void;
  setCheckpoints(checkpoints: Checkpoint[]): void;
  markCheckpointsUnavailable(reason: string): void;
  setError(message: string | null): void;
  clearForThread(): void;
}

/**
 * Create the checkpoint diff drawer state. Each pane owns one instance.
 *
 * Diffs are fetched from the selected message checkpoint on demand, so the
 * store tracks only drawer UI state and checkpoint availability. It
 * deliberately does not cache patch text: checkpoint refs are cheap to diff,
 * and keeping the cache out of pane state avoids stale message/worktree modes
 * leaking back into the UI.
 */
export function createDiffPanelState(): DiffPanelState {
  let open = $state(false);
  let viewMode: DiffViewMode = $state('stacked');
  let tabMode: DiffPanelTab = $state('messages');
  let selectedCheckpointUserItemId: string | null = $state(null);
  let checkpoints: Checkpoint[] = $state([]);
  let checkpointUserItemIds: ReadonlySet<string> = $state(new Set<string>());
  let checkpointsLoaded = $state(false);
  let checkpointsUnavailable = $state(false);
  let checkpointsUnavailableReason: string | null = $state(null);
  let error: string | null = $state(null);

  return {
    get open() { return open; },
    get viewMode() { return viewMode; },
    get tabMode() { return tabMode; },
    get selectedCheckpointUserItemId() { return selectedCheckpointUserItemId; },
    get checkpoints() { return checkpoints; },
    get checkpointUserItemIds() { return checkpointUserItemIds; },
    get checkpointsLoaded() { return checkpointsLoaded; },
    get checkpointsUnavailable() { return checkpointsUnavailable; },
    get checkpointsUnavailableReason() { return checkpointsUnavailableReason; },
    get error() { return error; },

    open_() { open = true; },
    close() { open = false; },
    toggle() { open = !open; },
    setViewMode(mode) { viewMode = mode; },
    setTabMode(mode) { tabMode = mode; },
    selectCheckpointUserItem(userItemId) { selectedCheckpointUserItemId = userItemId; },

    setCheckpoints(next) {
      checkpoints = [...next].sort((a, b) => a.turnIndex - b.turnIndex);
      checkpointUserItemIds = new Set(next.map((checkpoint) => checkpoint.userItemId));
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
      checkpointUserItemIds = new Set();
    },

    setError(message) { error = message; },

    clearForThread() {
      open = false;
      viewMode = 'stacked';
      tabMode = 'messages';
      selectedCheckpointUserItemId = null;
      checkpoints = [];
      checkpointUserItemIds = new Set();
      checkpointsLoaded = false;
      checkpointsUnavailable = false;
      checkpointsUnavailableReason = null;
      error = null;
    },
  };
}
