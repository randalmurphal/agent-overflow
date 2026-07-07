import type { Checkpoint } from '../types/checkpoint';

export interface ThreadCheckpointState {
  readonly checkpoints: Checkpoint[];
  readonly checkpointUserItemIds: ReadonlySet<string>;
  readonly loaded: boolean;
  readonly unavailable: boolean;
  readonly unavailableReason: string | null;
  readonly error: string | null;

  setCheckpoints(checkpoints: Checkpoint[]): void;
  markUnavailable(reason: string): void;
  setError(message: string | null): void;
  clearForThread(): void;
}

export function createThreadCheckpointState(): ThreadCheckpointState {
  let checkpoints: Checkpoint[] = $state([]);
  let checkpointUserItemIds: ReadonlySet<string> = $state(new Set<string>());
  let loaded = $state(false);
  let unavailable = $state(false);
  let unavailableReason: string | null = $state(null);
  let error: string | null = $state(null);

  return {
    get checkpoints() { return checkpoints; },
    get checkpointUserItemIds() { return checkpointUserItemIds; },
    get loaded() { return loaded; },
    get unavailable() { return unavailable; },
    get unavailableReason() { return unavailableReason; },
    get error() { return error; },

    setCheckpoints(next) {
      checkpoints = [...next].sort((a, b) => a.turnIndex - b.turnIndex);
      checkpointUserItemIds = new Set(checkpoints.map((checkpoint) => checkpoint.userItemId));
      loaded = true;
      if (checkpoints.length > 0) {
        unavailable = false;
        unavailableReason = null;
      }
    },

    markUnavailable(reason) {
      unavailable = true;
      unavailableReason = reason;
      loaded = true;
      checkpoints = [];
      checkpointUserItemIds = new Set();
    },

    setError(message) {
      error = message;
    },

    clearForThread() {
      checkpoints = [];
      checkpointUserItemIds = new Set();
      loaded = false;
      unavailable = false;
      unavailableReason = null;
      error = null;
    },
  };
}
