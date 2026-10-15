// stores/agentThreadCompanion.test.ts
//
// The agent thread companion: an Agents row's thread opens beside the source
// pane, another row swaps it, a thread already shown is revealed instead,
// closing never deletes the thread, it hides and reopens with its source
// thread, and Open as Pane keeps it in the same slot. Exercised through the
// real pane registry, companion registry and stash.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { openAgentThreadCompanion, openAgentThreadCompanionAsPane } from './agentThreadCompanion';
import {
  closeCompanion,
  getCompanionPane,
  installCompanionPanes,
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
import { installCompanionStash, resetCompanionStashForTest } from './companionStash';
import { REVEAL_PANE_EVENT } from './eventNames';
import { getPaneLayoutItems, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import {
  createPane,
  getFocusedPaneId,
  getPane,
  moveAssistantPanes,
  openThreadInNewPane,
  openThreadInPane,
  resetPanesForTest,
} from './panes.svelte';
import { removeThread, replaceAllThreads } from './threads.svelte';
import type { Thread } from '../types/models';
import { installPaneMocks, makeThread } from '../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const SOURCE = makeThread({ id: 'source', title: 'Source' });
const OTHER = makeThread({ id: 'other', title: 'Other' });
const WORKER = makeThread({ id: 'worker', title: 'Worker' });
const SECOND = makeThread({ id: 'second', title: 'Second worker' });

function paneIds(): string[] {
  return getPaneLayoutItems().map((item) => item.paneId);
}

function deletes(): unknown[] {
  return getBindingMock('DeleteThread')?.mock.calls ?? [];
}

async function source(): Promise<void> {
  await openThreadInPane(SOURCE, 'main');
}

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  installCompanionPanes();
  installCompanionStash();
  installPaneMocks();
  replaceAllThreads([SOURCE, OTHER, WORKER, SECOND]);
  setBindingMock('DeleteThread', async () => {});
  setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 600 }]);
  createPane('main');
});

afterEach(() => {
  resetCompanionStashForTest();
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetBindingMocks();
  replaceAllThreads([]);
});

describe('openAgentThreadCompanion', () => {
  it('opens the thread in a companion beside the source, leaving the source as it was', async () => {
    await source();

    await openAgentThreadCompanion(getPane('main')!, WORKER);

    expect(paneIds()).toEqual(['main', 'agent-thread-main']);
    expect(getCompanionPane('agent-thread-main')).toEqual({ paneId: 'agent-thread-main', kind: 'agent-thread', sourcePaneId: 'main' });
    expect(getPane('agent-thread-main')?.threadId).toBe(WORKER.id);
    expect(getPane('main')?.threadId).toBe(SOURCE.id);
  });

  it('swaps another row\'s thread into the same companion', async () => {
    await source();
    await openAgentThreadCompanion(getPane('main')!, WORKER);

    await openAgentThreadCompanion(getPane('main')!, SECOND);

    expect(paneIds()).toEqual(['main', 'agent-thread-main']);
    expect(getPane('agent-thread-main')?.threadId).toBe(SECOND.id);
    expect(deletes()).toEqual([]);
  });

  it('reveals a pane already showing the thread instead of opening a companion', async () => {
    await source();
    const shown = await openThreadInNewPane(WORKER);
    await openThreadInPane(SOURCE, 'main');

    const revealed: string[] = [];
    const onReveal = ((event: CustomEvent<{ paneId: string }>) => {
      revealed.push(event.detail.paneId);
    }) as EventListener;
    window.addEventListener(REVEAL_PANE_EVENT, onReveal);
    try {
      await openAgentThreadCompanion(getPane('main')!, WORKER);
    } finally {
      window.removeEventListener(REVEAL_PANE_EVENT, onReveal);
    }

    expect(paneIds()).toEqual(['main', shown.paneId]);
    expect(getFocusedPaneId()).toBe(shown.paneId);
    // Under compact the strip moves to it: focus alone shows nothing.
    expect(revealed).toEqual([shown.paneId]);
  });

  it('closes a companion it opened when the thread cannot be mounted', async () => {
    await source();
    await expect(openAgentThreadCompanion(getPane('main')!, { ...WORKER, forkPreparing: true } as Thread)).rejects.toThrow();
    expect(paneIds()).toEqual(['main']);
    expect(isCompanionOpen('main', 'agent-thread')).toBe(false);
  });

  it('never deletes the thread when closed, and remembers what the companion had open', async () => {
    await source();
    await openAgentThreadCompanion(getPane('main')!, WORKER);
    openCompanion('agent-thread-main', 'plan');

    closeCompanion('agent-thread-main');

    expect(paneIds()).toEqual(['main']);
    expect(deletes()).toEqual([]);
    const pane = await openThreadInNewPane(WORKER);
    expect(isCompanionOpen(pane.paneId, 'plan')).toBe(true);
  });
});

describe('the agent thread companion in the stash', () => {
  it('hides with its source thread and reopens on the same thread', async () => {
    await source();
    await openAgentThreadCompanion(getPane('main')!, WORKER);

    await openThreadInPane(OTHER, 'main');
    expect(paneIds()).toEqual(['main']);

    await openThreadInPane(SOURCE, 'main');
    await vi.waitFor(() => expect(getPane('agent-thread-main')?.threadId).toBe(WORKER.id));
    expect(paneIds()).toEqual(['main', 'agent-thread-main']);
    expect(deletes()).toEqual([]);
  });

  it('reopens on the assistant\'s current thread when the one it showed was superseded meanwhile', async () => {
    const past = makeThread({ id: 'assistant-1', title: 'Ops', agentId: 'agent-ops' });
    replaceAllThreads([SOURCE, OTHER, past]);
    await source();
    await openAgentThreadCompanion(getPane('main')!, past);
    await openThreadInPane(OTHER, 'main');

    const current = makeThread({ id: 'assistant-2', title: 'Ops', agentId: 'agent-ops' });
    replaceAllThreads([SOURCE, OTHER, { ...past, agentId: undefined, pastAgentId: 'agent-ops' }, current]);
    await openThreadInPane(SOURCE, 'main');

    await vi.waitFor(() => expect(getPane('agent-thread-main')?.threadId).toBe(current.id));
  });

  it('does not reopen when its thread is shown in another pane by then', async () => {
    await source();
    await openAgentThreadCompanion(getPane('main')!, WORKER);
    await openThreadInPane(OTHER, 'main');
    const shown = await openThreadInNewPane(WORKER);

    await openThreadInPane(SOURCE, 'main');

    await vi.waitFor(() => expect(isCompanionOpen('main', 'agent-thread')).toBe(false));
    expect(getPane(shown.paneId)?.threadId).toBe(WORKER.id);
  });

  it('is forgotten, never deleted, when its source thread is removed', async () => {
    await source();
    await openAgentThreadCompanion(getPane('main')!, WORKER);
    await openThreadInPane(OTHER, 'main');

    removeThread(SOURCE.id);

    expect(deletes()).toEqual([]);
  });

  it('leaves the stash of its source thread when its own thread is removed', async () => {
    await source();
    await openAgentThreadCompanion(getPane('main')!, WORKER);
    await openThreadInPane(OTHER, 'main');

    removeThread(WORKER.id);
    await openThreadInPane(SOURCE, 'main');

    expect(paneIds()).toEqual(['main']);
  });
});

describe('a handoff', () => {
  it('moves the companion to the assistant\'s current thread in place', async () => {
    const past = makeThread({ id: 'assistant-1', title: 'Ops', agentId: 'agent-ops' });
    const current = makeThread({ id: 'assistant-2', title: 'Ops', agentId: 'agent-ops' });
    replaceAllThreads([SOURCE, past, current]);
    await source();
    await openAgentThreadCompanion(getPane('main')!, past);

    moveAssistantPanes(past.id, current);

    await vi.waitFor(() => expect(getPane('agent-thread-main')?.threadId).toBe(current.id));
    expect(getPane('main')?.threadId).toBe(SOURCE.id);
    expect(paneIds()).toEqual(['main', 'agent-thread-main']);
  });
});

describe('openAgentThreadCompanionAsPane', () => {
  it('swaps the companion for a normal pane in the same slot that stays when the source leaves', async () => {
    await source();
    await openAgentThreadCompanion(getPane('main')!, WORKER);

    await openAgentThreadCompanionAsPane('agent-thread-main');

    const items = getPaneLayoutItems();
    expect(items.map((item) => item.kind)).toEqual(['thread', 'thread']);
    expect(getPane(items[1].paneId)?.threadId).toBe(WORKER.id);
    expect(getCompanionPane('agent-thread-main')).toBeNull();
    await openThreadInPane(OTHER, 'main');
    expect(getPane(items[1].paneId)?.threadId).toBe(WORKER.id);
    expect(deletes()).toEqual([]);
  });
});
