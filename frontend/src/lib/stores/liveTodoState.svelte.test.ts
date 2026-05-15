import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetActivityRailUiPrefsForTest,
  __resetLiveTodoUiPrefsForTest,
  createLiveTodoState,
  LIVE_TODO_AUTOHIDE_MS,
} from './liveTodoState.svelte';

describe('liveTodoState', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    __resetActivityRailUiPrefsForTest();
    __resetLiveTodoUiPrefsForTest();
  });

  afterEach(() => {
    vi.useRealTimers();
    __resetActivityRailUiPrefsForTest();
    __resetLiveTodoUiPrefsForTest();
  });

  it('sets and clears live todo snapshots', () => {
    const state = createLiveTodoState();

    state.setLiveTodo([{ step: 'write tests', status: 'inProgress' }]);
    expect(state.liveTodo?.steps).toEqual([{ step: 'write tests', status: 'inProgress' }]);

    state.clearLiveTodo();
    expect(state.liveTodo).toBeNull();
  });

  it('auto-hides completed todos and suppresses repeated completed rows for the same cycle', () => {
    const state = createLiveTodoState();

    state.setLiveTodo([
      { step: 'done one', status: 'completed' },
      { step: 'done two', status: 'completed' },
    ]);

    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS);

    expect(state.liveTodo).toBeNull();

    state.setLiveTodo([
      { step: 'done one', status: 'completed' },
      { step: 'done two', status: 'completed' },
    ]);
    expect(state.liveTodo).toBeNull();

    state.setLiveTodo([
      { step: 'done one', status: 'completed' },
      { step: 'new work', status: 'inProgress' },
    ]);
    expect(state.liveTodo?.steps).toEqual([{ step: 'new work', status: 'inProgress' }]);
  });

  it('explicit clear resets completed-cycle suppression', () => {
    const state = createLiveTodoState();

    state.setLiveTodo([{ step: 'done', status: 'completed' }]);
    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS);
    state.clearLiveTodo();
    state.setLiveTodo([{ step: 'done', status: 'completed' }]);

    expect(state.liveTodo?.steps).toEqual([{ step: 'done', status: 'completed' }]);
  });

  it('stores show-all and rail preferences by thread', () => {
    const state = createLiveTodoState();

    state.resetForThread('thread-a');
    state.toggleLiveTodoShowAll('thread-a');
    state.toggleActivityRailTodos('thread-a');
    state.toggleActivityRailBackground('thread-a');

    state.resetForThread('thread-b');
    expect(state.liveTodoShowAll).toBe(false);
    expect(state.activityRailTodosOpen).toBe(false);
    expect(state.activityRailBackgroundOpen).toBe(false);

    state.resetForThread('thread-a');
    expect(state.liveTodoShowAll).toBe(true);
    expect(state.activityRailTodosOpen).toBe(true);
    expect(state.activityRailBackgroundOpen).toBe(true);
  });

  it('hydrates fresh snapshots but ignores stale all-completed snapshots', () => {
    const state = createLiveTodoState();
    const freshRevision = state.revision;

    state.hydrateSnapshotIfUnchanged({
      threadId: 'thread-a',
      steps: [{ step: 'still going', status: 'inProgress' }],
      updatedAt: Date.now(),
    }, 'thread-a', freshRevision);
    expect(state.liveTodo?.steps).toEqual([{ step: 'still going', status: 'inProgress' }]);

    state.clearLiveTodo();
    const staleRevision = state.revision;
    state.hydrateSnapshotIfUnchanged({
      threadId: 'thread-a',
      steps: [{ step: 'old done', status: 'completed' }],
      updatedAt: Date.now() - LIVE_TODO_AUTOHIDE_MS - 1,
    }, 'thread-a', staleRevision);

    expect(state.liveTodo).toBeNull();
  });

  it('does not apply a hydration snapshot after local todo state changes', () => {
    const state = createLiveTodoState();
    const revisionAtRequest = state.revision;

    state.setLiveTodo([{ step: 'local update', status: 'inProgress' }]);
    state.hydrateSnapshotIfUnchanged({
      threadId: 'thread-a',
      steps: [{ step: 'stale backend', status: 'inProgress' }],
      updatedAt: Date.now(),
    }, 'thread-a', revisionAtRequest);

    expect(state.liveTodo?.steps).toEqual([{ step: 'local update', status: 'inProgress' }]);
  });
});
