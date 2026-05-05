import { beforeEach, describe, expect, it } from 'vitest';
import type { Checkpoint } from '../types/checkpoint';
import { createDiffPanelState } from './diffPanel.svelte';

function checkpoint(overrides: Partial<Checkpoint> = {}): Checkpoint {
  const turnIndex = overrides.turnIndex ?? 0;
  return {
    id: `c-${turnIndex}`,
    threadId: 't-1',
    userItemId: `user:${turnIndex}`,
    turnIndex,
    status: 'ready',
    files: [],
    capturedAt: 0,
    ...overrides,
  };
}

describe('createDiffPanelState', () => {
  let store: ReturnType<typeof createDiffPanelState>;

  beforeEach(() => {
    store = createDiffPanelState();
  });

  it('starts closed with checkpoint drawer defaults', () => {
    expect(store.open).toBe(false);
    expect(store.viewMode).toBe('stacked');
    expect(store.selectedCheckpointUserItemId).toBeNull();
    expect(store.checkpoints).toEqual([]);
    expect(store.checkpointsLoaded).toBe(false);
    expect(store.checkpointsUnavailable).toBe(false);
    expect(store.checkpointsUnavailableReason).toBeNull();
    expect(store.error).toBeNull();
  });

  it('opens, closes, and toggles', () => {
    store.open_();
    expect(store.open).toBe(true);

    store.toggle();
    expect(store.open).toBe(false);

    store.toggle();
    expect(store.open).toBe(true);

    store.close();
    expect(store.open).toBe(false);
  });

  it('tracks view mode and selected checkpoint message', () => {
    store.setViewMode('split');
    store.selectCheckpointUserItem('user:3');

    expect(store.viewMode).toBe('split');
    expect(store.selectedCheckpointUserItemId).toBe('user:3');

    store.selectCheckpointUserItem(null);
    expect(store.selectedCheckpointUserItemId).toBeNull();
  });

  it('sorts checkpoints by turn index', () => {
    store.setCheckpoints([
      checkpoint({ id: 'c3', turnIndex: 3 }),
      checkpoint({ id: 'c1', turnIndex: 1 }),
      checkpoint({ id: 'c2', turnIndex: 2 }),
    ]);

    expect(store.checkpoints.map((c) => c.id)).toEqual(['c1', 'c2', 'c3']);
    expect(store.checkpointsLoaded).toBe(true);
  });

  it('clears unavailable state once checkpoints are available again', () => {
    store.markCheckpointsUnavailable('not-a-git-repo');
    store.setCheckpoints([checkpoint({ turnIndex: 0 })]);

    expect(store.checkpointsUnavailable).toBe(false);
    expect(store.checkpointsUnavailableReason).toBeNull();
  });

  it('keeps unavailable reason when an empty checkpoint list arrives', () => {
    store.markCheckpointsUnavailable('not-a-git-repo');
    store.setCheckpoints([]);

    expect(store.checkpointsUnavailable).toBe(true);
    expect(store.checkpointsUnavailableReason).toBe('not-a-git-repo');
  });

  it('records checkpoint unavailability and clears stale checkpoints', () => {
    store.setCheckpoints([checkpoint({ turnIndex: 0 })]);
    store.markCheckpointsUnavailable('not-a-git-repo');

    expect(store.checkpoints).toEqual([]);
    expect(store.checkpointsLoaded).toBe(true);
    expect(store.checkpointsUnavailable).toBe(true);
  });

  it('resets all drawer state on thread switch', () => {
    store.open_();
    store.setViewMode('split');
    store.selectCheckpointUserItem('user:4');
    store.setCheckpoints([checkpoint({ turnIndex: 4 })]);
    store.setError('boom');
    store.markCheckpointsUnavailable('temporary');

    store.clearForThread();

    expect(store.open).toBe(false);
    expect(store.viewMode).toBe('stacked');
    expect(store.selectedCheckpointUserItemId).toBeNull();
    expect(store.checkpoints).toEqual([]);
    expect(store.checkpointsLoaded).toBe(false);
    expect(store.checkpointsUnavailable).toBe(false);
    expect(store.checkpointsUnavailableReason).toBeNull();
    expect(store.error).toBeNull();
  });
});
