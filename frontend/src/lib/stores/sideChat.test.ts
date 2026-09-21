import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { keepSideChat, openSideChat } from './sideChat';
import {
  closeCompanion,
  getCompanionPane,
  installCompanionPanes,
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
import {
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from './paneLayout.svelte';
import { createPane, getPane, resetPanesForTest } from './panes.svelte';
import { replaceAllThreads } from './threads.svelte';
import { getToasts } from './toast.svelte';
import type { ThreadPane } from './thread.svelte';
import { installPaneMocks, makeThread } from '../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const SOURCE = makeThread({ id: 'source-thread', title: 'Ship the parser', mode: 'plan' });
const FORK = makeThread({ id: 'fork-thread', title: 'Side chat: Ship the parser', mode: 'scratch' });

function sourcePane(): ThreadPane {
  setPaneLayoutItemsForTest([
    { id: 'main', paneId: 'main', kind: 'thread', widthPx: 660 },
  ]);
  const pane = createPane('main');
  pane.replaceThread(SOURCE);
  return pane;
}

function paneIds(): string[] {
  return getPaneLayoutItems().map((item) => item.paneId);
}

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  installCompanionPanes();
  replaceAllThreads([SOURCE]);
  installPaneMocks();
  setBindingMock('ForkSideChat', async () => FORK);
  setBindingMock('PromoteScratchThread', async () => makeThread({ ...FORK, mode: 'plan' }));
  setBindingMock('DeleteThread', async () => {});
});

afterEach(() => {
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
  resetBindingMocks();
  replaceAllThreads([]);
});

describe('openSideChat', () => {
  it('forks the pane’s thread and mounts the fork in a companion beside it', async () => {
    const pane = sourcePane();

    expect(await openSideChat(pane)).toEqual({ error: '' });

    expect(getBindingMock('ForkSideChat')?.mock.calls).toEqual([['source-thread']]);
    expect(paneIds()).toEqual(['main', 'side-chat-main']);
    expect(getPaneLayoutItems()[1].kind).toBe('side-chat');
    expect(getCompanionPane('side-chat-main')).toEqual({
      paneId: 'side-chat-main',
      kind: 'side-chat',
      sourcePaneId: 'main',
    });
    // The companion is a thread pane of its own, showing the fork.
    expect(getPane('side-chat-main')?.threadId).toBe('fork-thread');
    // The source pane is untouched: the fork is a snapshot, not a move.
    expect(getPane('main')?.threadId).toBe('source-thread');
  });

  it('refuses a pane with no thread without calling the backend', async () => {
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 660 }]);
    const pane = createPane('main');

    expect(await openSideChat(pane)).toEqual({
      error: 'Open a thread before starting a side chat.',
    });
    expect(getBindingMock('ForkSideChat')?.mock.calls ?? []).toEqual([]);
    expect(paneIds()).toEqual(['main']);
  });

  it('refuses a second side chat for the same pane', async () => {
    const pane = sourcePane();
    openCompanion('main', 'side-chat');

    expect(await openSideChat(pane)).toEqual({
      error: 'This pane already has a side chat open.',
    });
    expect(getBindingMock('ForkSideChat')?.mock.calls ?? []).toEqual([]);
  });

  it('reports a fork failure to the composer and opens nothing', async () => {
    const pane = sourcePane();
    setBindingMock('ForkSideChat', async () => {
      throw new Error('backend unreachable');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    const result = await openSideChat(pane);

    expect(result.error).not.toBe('');
    expect(isCompanionOpen('main', 'side-chat')).toBe(false);
    expect(paneIds()).toEqual(['main']);
    consoleError.mockRestore();
  });

  it('deletes the fork when the pane moved on while it was in flight', async () => {
    const pane = sourcePane();
    setBindingMock('ForkSideChat', async () => {
      // The user switched the pane to another thread before the fork landed.
      pane.replaceThread(makeThread({ id: 'other-thread' }));
      return FORK;
    });

    const result = await openSideChat(pane);

    expect(result.error).toBe('The pane moved on before the side chat opened.');
    expect(isCompanionOpen('main', 'side-chat')).toBe(false);
    await vi.waitFor(() => {
      expect(getBindingMock('DeleteThread')?.mock.calls).toEqual([['fork-thread']]);
    });
  });
});

describe('keepSideChat', () => {
  it('promotes the scratch thread and swaps the companion for a thread pane in place', async () => {
    const pane = sourcePane();
    setPaneLayoutItemsForTest([
      ...getPaneLayoutItems(),
      { id: 'right', paneId: 'right', kind: 'thread', widthPx: 660 },
    ]);
    await openSideChat(pane);
    expect(paneIds()).toEqual(['main', 'side-chat-main', 'right']);

    await keepSideChat('side-chat-main');

    expect(getBindingMock('PromoteScratchThread')?.mock.calls).toEqual([['fork-thread']]);
    // The companion is gone, registry entry included, and an ordinary thread
    // pane holds the kept thread in the same slot.
    expect(getCompanionPane('side-chat-main')).toBeNull();
    expect(getPane('side-chat-main')).toBeUndefined();
    const items = getPaneLayoutItems();
    expect(items.map((item) => item.kind)).toEqual(['thread', 'thread', 'thread']);
    expect(getPane(items[1].paneId)?.threadId).toBe('fork-thread');
    // Keep does NOT delete: the thread is the point. The sidebar row arrives
    // with the backend's `listed` broadcast, not from this store.
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);
  });

  it('leaves the pane alone and toasts when the promotion fails', async () => {
    const pane = sourcePane();
    await openSideChat(pane);
    setBindingMock('PromoteScratchThread', async () => {
      throw new Error('not a side chat');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    await keepSideChat('side-chat-main');

    expect(getToasts().some((toast) => toast.type === 'error')).toBe(true);
    expect(getCompanionPane('side-chat-main')).not.toBeNull();
    expect(getPane('side-chat-main')?.threadId).toBe('fork-thread');
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);
    consoleError.mockRestore();
  });

  it('does nothing for a pane that holds no thread', async () => {
    await keepSideChat('ghost');
    expect(getBindingMock('PromoteScratchThread')?.mock.calls ?? []).toEqual([]);
  });
});

describe('side chat preparation', () => {
  it('opens a loading pane immediately and refuses duplicate requests', async () => {
    let finish!: (thread: typeof FORK) => void;
    setBindingMock('ForkSideChat', () => new Promise(resolve => { finish = resolve; }));
    const pane = sourcePane();
    const pending = openSideChat(pane);
    expect(paneIds()).toEqual(['main', 'side-chat-main']);
    expect(getPane('side-chat-main')?.threadId).toBeFalsy();
    expect((await openSideChat(pane)).error).toContain('already');
    finish(FORK);
    expect(await pending).toEqual({ error: '' });
    expect(getPane('side-chat-main')?.threadId).toBe(FORK.id);
  });

  it.each(['success', 'failure'])('keeps a reopened pane when the old request ends with %s', async (outcome) => {
    let finish!: (thread: typeof FORK) => void;
    let fail!: (error: Error) => void;
    setBindingMock('ForkSideChat', () => new Promise((resolve, reject) => { finish = resolve; fail = reject; }));
    const pane = sourcePane();
    const old = openSideChat(pane);
    closeCompanion('side-chat-main');
    const newer = makeThread({ ...FORK, id: 'new-fork' });
    setBindingMock('ForkSideChat', async () => newer);
    expect(await openSideChat(pane)).toEqual({ error: '' });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    if (outcome === 'success') finish(FORK); else fail(new Error('old request failed'));
    expect((await old).error).not.toBe('');
    expect(getPane('side-chat-main')?.threadId).toBe(newer.id);
    expect(paneIds()).toEqual(['main', 'side-chat-main']);
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual(outcome === 'success' ? [[FORK.id]] : []);
    consoleError.mockRestore();
  });
});
