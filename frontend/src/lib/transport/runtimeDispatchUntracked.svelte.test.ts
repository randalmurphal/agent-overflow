// A binding call is an action, not a reactive read. `Call.ByID` resolves
// its route from reactive state (the entity index, the focused pane's
// thread, the selected computer), and an `$effect` that issues a call must
// not become a subscriber of that state through the door: before dispatch
// was untracked, every image host refetched its bytes each time another
// pane took focus.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { Call } from './runtime';
import { __attachBackendForTest, __resetBackendsForTest } from './backends';
import { HOME_BACKEND } from './backendKey';
import { HOME_DESCRIPTOR } from './manifestBackends';
import * as index from './entityIndex';
import * as selection from '../stores/selectedBackend.svelte';

const GET_LOCAL_IMAGE_DATA = 3247514443; // route `selected`
const GET_THREAD = 1098302047; // route `thread`

const client = {
  callByID: vi.fn(async () => ({})),
  callByName: vi.fn(async () => ({})),
  subscribe: vi.fn(() => () => undefined),
  installStepUpProver: vi.fn(),
  setWatchedThreads: vi.fn(),
  getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
  onReplay: vi.fn(() => () => undefined),
  onStatusChange: vi.fn(() => () => undefined),
  close: vi.fn(),
};

beforeEach(() => {
  client.callByID.mockClear();
  __attachBackendForTest(HOME_DESCRIPTOR, client as unknown as Parameters<typeof __attachBackendForTest>[1]);
  selection.__resetSelectedBackendForTest();
  selection.setActiveBackendPaneResolver(() => null);
  index.__resetEntityIndexForTest();
});

afterEach(() => {
  selection.setFocusedThreadResolver(() => null);
  __resetBackendsForTest();
});

it('an effect issuing a selected-routed call does not re-run when the focused pane or the index changes', () => {
  const first = { id: 'thread-a', projectId: 'project-a' };
  const second = { id: 'thread-b', projectId: 'project-b' };
  index.noteThread(first.id, HOME_BACKEND, 0);
  // On another computer, so focusing it changes the route's answer.
  index.noteThread(second.id, 'gpu', 0);
  let focused = $state(first);
  selection.setFocusedThreadResolver(() => focused);

  let dispatches = 0;
  const routeAnswers: string[] = [];
  const stop = $effect.root(() => {
    $effect(() => {
      dispatches += 1;
      void Call.ByID(GET_LOCAL_IMAGE_DATA, '/workspace/diagram.png', '/workspace');
    });
    // The control: the same inputs, read reactively, do wake a reader.
    const answer = $derived(selection.selectedBackend());
    $effect(() => {
      routeAnswers.push(answer);
    });
  });
  try {
    flushSync();
    expect(dispatches).toBe(1);
    expect(client.callByID).toHaveBeenCalledTimes(1);

    focused = second;
    flushSync();
    index.noteThread(first.id, HOME_BACKEND, 1);
    index.noteThread(second.id, 'gpu', 1);
    flushSync();

    expect(routeAnswers).toEqual([HOME_BACKEND, 'gpu']);
    expect(dispatches).toBe(1);
    expect(client.callByID).toHaveBeenCalledTimes(1);
  } finally {
    stop();
  }
});

it('an effect issuing a thread-routed call does not re-run when that thread is re-indexed', () => {
  index.noteThread('thread-a', HOME_BACKEND, 0);

  let dispatches = 0;
  const stop = $effect.root(() => {
    $effect(() => {
      dispatches += 1;
      void Call.ByID(GET_THREAD, 'thread-a');
    });
  });
  try {
    flushSync();
    expect(dispatches).toBe(1);

    index.noteThread('thread-a', HOME_BACKEND, 1);
    flushSync();

    expect(dispatches).toBe(1);
    expect(client.callByID).toHaveBeenCalledTimes(1);
  } finally {
    stop();
  }
});
