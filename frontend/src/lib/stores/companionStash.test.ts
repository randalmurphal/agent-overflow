// stores/companionStash.test.ts
//
// Companions belong to their thread, per client: a pane leaving a thread
// hides them, the thread's next mount reopens them, an explicit close
// forgets them, and a deleted thread takes them with it. Exercised through
// the real pane registry, so the leave edges (switch, close) and the mount
// observer are the production ones.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { agentScopeForPane, agentStateForPane, openAgentCompanion } from './agentPane.svelte';
import { appStorageGet } from './appStorage';
import {
  closeCompanion,
  getCompanionPane,
  installCompanionPanes,
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
import { installCompanionStash, resetCompanionStashForTest, stashCompanions } from './companionStash';
import { draftPlaceholderId } from './draftPlaceholderId';
import { REVEAL_PANE_EVENT } from './eventNames';
import { getPaneLayoutItems, resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import {
  closePanesShowingThread,
  createPane,
  destroyPane,
  focusPane,
  getFocusedPaneId,
  getPane,
  openThreadInNewPane,
  openThreadInPane,
  resetPanesForTest,
} from './panes.svelte';
import { keepSideChat } from './sideChat';
import { removeThread, replaceAllThreads } from './threads.svelte';
import { getToasts } from './toast.svelte';
import type { Thread } from '../types/models';
import { TransportError } from '../transport/wsClient';
import { installPaneMocks, makeThread } from '../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

const A = makeThread({ id: 'thread-a', title: 'A' });
const B = makeThread({ id: 'thread-b', title: 'B' });
const SIDE = makeThread({ id: 'side-1', title: 'Side chat: A', mode: 'scratch' });

function paneIds(): string[] {
  return getPaneLayoutItems().map((item) => item.paneId);
}

function widthOf(paneId: string): number | undefined {
  return getPaneLayoutItems().find((item) => item.paneId === paneId)?.widthPx;
}

function setWidth(paneId: string, widthPx: number): void {
  setPaneLayoutItemsForTest(
    getPaneLayoutItems().map((item) => (item.paneId === paneId ? { ...item, widthPx } : item)),
  );
}

async function mountMain(thread: Thread): Promise<void> {
  await openThreadInPane(thread, 'main');
}

/** A side chat as openSideChat leaves it: a companion pane showing its fork. */
async function openSideChatOn(sourcePaneId: string, thread: Thread = SIDE): Promise<string> {
  const companion = openCompanion(sourcePaneId, 'side-chat');
  if (!companion) throw new Error('side chat did not open');
  await openThreadInPane(thread, createPane(companion.paneId));
  return companion.paneId;
}

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  installCompanionPanes();
  installCompanionStash();
  installPaneMocks();
  replaceAllThreads([A, B]);
  setBindingMock('DeleteThread', async () => {});
  setBindingMock('GetThread', async (id: unknown) => (id === SIDE.id ? SIDE : null));
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

describe('companion stash', () => {
  it('hides companions when the pane switches away and reopens them as they were', async () => {
    await mountMain(A);
    openCompanion('main', 'plan');
    openCompanion('main', 'review');
    setWidth('review-main', 720);
    openAgentCompanion('main', A.id, 'launch-1', 'reviewer');
    agentStateForPane('main', A.id).pushScope('launch-2', 'angle-b');
    expect(paneIds()).toEqual(['main', 'plan-main', 'review-main', 'agent-main']);

    await mountMain(B);
    expect(paneIds()).toEqual(['main']);

    await mountMain(A);
    expect(paneIds()).toEqual(['main', 'plan-main', 'review-main', 'agent-main']);
    expect(widthOf('review-main')).toBe(720);
    expect(agentScopeForPane('main', A.id)).toEqual({
      scopeItemId: 'launch-2',
      breadcrumb: [
        { itemId: '', label: 'main' },
        { itemId: 'launch-1', label: 'reviewer' },
        { itemId: 'launch-2', label: 'angle-b' },
      ],
    });
  });

  it('does not carry one thread\'s companions to the next', async () => {
    await mountMain(A);
    openCompanion('main', 'plan');

    await mountMain(B);
    openCompanion('main', 'review');
    await mountMain(A);

    expect(paneIds()).toEqual(['main', 'plan-main']);
    await mountMain(B);
    expect(paneIds()).toEqual(['main', 'review-main']);
  });

  it('reopens them without revealing them or moving focus', async () => {
    await mountMain(A);
    openCompanion('main', 'plan');
    await mountMain(B);
    const revealed: string[] = [];
    const onReveal = ((event: CustomEvent<{ paneId: string }>) => {
      revealed.push(event.detail.paneId);
    }) as EventListener;
    window.addEventListener(REVEAL_PANE_EVENT, onReveal);
    try {
      await mountMain(A);
    } finally {
      window.removeEventListener(REVEAL_PANE_EVENT, onReveal);
    }

    expect(isCompanionOpen('main', 'plan')).toBe(true);
    expect(revealed).toEqual(['main']);
    expect(getFocusedPaneId()).toBe('main');
  });

  it('forgets a companion closed explicitly', async () => {
    await mountMain(A);
    openCompanion('main', 'plan');
    openCompanion('main', 'review');
    closeCompanion('review-main');

    await mountMain(B);
    await mountMain(A);

    expect(paneIds()).toEqual(['main', 'plan-main']);
  });

  it('reopens them in whichever pane next shows the thread, after the pane closed', async () => {
    await mountMain(A);
    openCompanion('main', 'plan');

    destroyPane('main');
    expect(paneIds()).toEqual([]);

    const pane = await openThreadInNewPane(A);
    expect(paneIds()).toEqual([pane.paneId, `plan-${pane.paneId}`]);
  });

  it('leaves the browser companion to the backend state that reopens it', async () => {
    await mountMain(A);
    openCompanion('main', 'browser');

    await mountMain(B);
    await mountMain(A);

    expect(isCompanionOpen('main', 'browser')).toBe(false);
  });

  it('closes a draft placeholder\'s companions, since nothing mounts it again', () => {
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 600 }]);
    openCompanion('main', 'review');
    const placeholder = makeThread({ id: draftPlaceholderId('main', 'p-1', 'chat'), isDraft: true });

    stashCompanions('main', placeholder);

    expect(paneIds()).toEqual(['main']);
    expect(appStorageGet(`companionStash:${placeholder.id}`)).toBeNull();
  });

  it('persists plan, review and agent for the next session, and nothing session-bound', async () => {
    const tui = makeThread({ id: 'thread-tui', provider: 'claude-tui' });
    replaceAllThreads([A, B, tui]);
    await mountMain(tui);
    openCompanion('main', 'take-control');
    openCompanion('main', 'plan');
    openAgentCompanion('main', tui.id, 'launch-1', 'reviewer');
    await openSideChatOn('main');

    await mountMain(B);

    expect(JSON.parse(appStorageGet('companionStash:thread-tui') ?? 'null')).toEqual([
      { kind: 'plan', widthPx: 600 },
      {
        kind: 'agent',
        widthPx: 600,
        agentScope: {
          scopeItemId: 'launch-1',
          breadcrumb: [{ itemId: '', label: 'main' }, { itemId: 'launch-1', label: 'reviewer' }],
        },
      },
    ]);

    // A new session: the in-memory stash is gone, appStorage is not.
    resetCompanionStashForTest();
    installCompanionStash();
    await mountMain(tui);

    expect(paneIds()).toEqual(['main', 'plan-main', 'agent-main']);
    expect(appStorageGet('companionStash:thread-tui')).toBeNull();
  });

  it('closes an agent pane that has no scope to reopen at', async () => {
    await mountMain(A);
    openCompanion('main', 'agent');

    await mountMain(B);
    await mountMain(A);

    expect(isCompanionOpen('main', 'agent')).toBe(false);
  });
});

describe('side chats in the stash', () => {
  it('hides a side chat with its thread and reopens the same conversation', async () => {
    await mountMain(A);
    const sidePaneId = await openSideChatOn('main');
    openAgentCompanion(sidePaneId, SIDE.id, 'launch-9', 'worker');
    expect(paneIds()).toEqual(['main', 'side-chat-main', 'agent-side-chat-main']);

    await mountMain(B);
    expect(paneIds()).toEqual(['main']);
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);

    focusPane('main');
    await mountMain(A);
    await vi.waitFor(() => {
      expect(paneIds()).toEqual(['main', 'side-chat-main', 'agent-side-chat-main']);
    });
    expect(getPane('side-chat-main')?.threadId).toBe(SIDE.id);
    expect(agentScopeForPane('side-chat-main', SIDE.id)?.scopeItemId).toBe('launch-9');
    expect(getFocusedPaneId()).toBe('main');
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);
    // A scratch thread's companions are never persisted: it does not outlive
    // the session.
    expect(appStorageGet(`companionStash:${SIDE.id}`)).toBeNull();
  });

  it('hides the side chat when its source pane closes, too', async () => {
    await mountMain(A);
    await openSideChatOn('main');

    destroyPane('main');

    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);
    const pane = await openThreadInNewPane(A);
    await vi.waitFor(() => {
      expect(getPane(`side-chat-${pane.paneId}`)?.threadId).toBe(SIDE.id);
    });
  });

  it('keeps a side chat that is leaving again before its reopen settles', async () => {
    await mountMain(A);
    await openSideChatOn('main');
    await mountMain(B);
    let answer: (thread: Thread) => void = () => {};
    setBindingMock('GetThread', () => new Promise<Thread>((resolve) => { answer = resolve; }));

    await mountMain(A);
    expect(getCompanionPane('side-chat-main')).not.toBeNull();
    await mountMain(B);
    answer(SIDE);
    await Promise.resolve();

    expect(paneIds()).toEqual(['main']);
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);
    setBindingMock('GetThread', async () => SIDE);
    await mountMain(A);
    await vi.waitFor(() => {
      expect(getPane('side-chat-main')?.threadId).toBe(SIDE.id);
    });
  });

  it('drops a side chat whose computer swept it, without an error', async () => {
    await mountMain(A);
    await openSideChatOn('main');
    await mountMain(B);
    setBindingMock('GetThread', async () => {
      throw new TransportError('not_found', 'thread not found');
    });

    await mountMain(A);

    await vi.waitFor(() => {
      expect(paneIds()).toEqual(['main']);
    });
    expect(getToasts()).toEqual([]);
    await mountMain(B);
    await mountMain(A);
    expect(getBindingMock('GetThread')?.mock.calls).toHaveLength(1);
  });

  it('keeps a side chat it could not reopen for the next visit, and says so', async () => {
    await mountMain(A);
    await openSideChatOn('main');
    await mountMain(B);
    setBindingMock('GetThread', async () => {
      throw new Error('backend unreachable');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    await mountMain(A);

    await vi.waitFor(() => {
      expect(getToasts().some((toast) => toast.type === 'error')).toBe(true);
    });
    expect(paneIds()).toEqual(['main']);
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);
    consoleError.mockRestore();

    setBindingMock('GetThread', async () => SIDE);
    await mountMain(B);
    await mountMain(A);
    await vi.waitFor(() => {
      expect(getPane('side-chat-main')?.threadId).toBe(SIDE.id);
    });
  });

  it('carries the side chat\'s own companions to the pane Keep opens', async () => {
    await mountMain(A);
    const sidePaneId = await openSideChatOn('main');
    openCompanion(sidePaneId, 'plan');
    setBindingMock('PromoteScratchThread', async () => makeThread({ ...SIDE, mode: 'chat' }));

    await keepSideChat(sidePaneId);

    const kept = getPaneLayoutItems().find((item) => item.kind === 'thread' && getPane(item.paneId)?.threadId === SIDE.id);
    expect(kept).toBeDefined();
    expect(isCompanionOpen(kept!.paneId, 'plan')).toBe(true);
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);
  });
});

describe('a removed thread takes its companions with it', () => {
  it('forgets a hidden stash and deletes the side chat hidden with it', async () => {
    await mountMain(A);
    openCompanion('main', 'plan');
    await openSideChatOn('main');
    await mountMain(B);
    expect(appStorageGet(`companionStash:${A.id}`)).not.toBeNull();

    removeThread(A.id);

    expect(getBindingMock('DeleteThread')?.mock.calls).toEqual([[SIDE.id]]);
    expect(appStorageGet(`companionStash:${A.id}`)).toBeNull();
    replaceAllThreads([A, B]);
    await mountMain(A);
    expect(paneIds()).toEqual(['main']);
  });

  it('closes the companions of a pane still showing it, deleting its side chat', async () => {
    await mountMain(A);
    openCompanion('main', 'plan');
    await openSideChatOn('main');

    // The order every delete and archive flow uses.
    removeThread(A.id);
    closePanesShowingThread(A.id);

    expect(paneIds()).toEqual([]);
    expect(getBindingMock('DeleteThread')?.mock.calls).toEqual([[SIDE.id]]);
    expect(appStorageGet(`companionStash:${A.id}`)).toBeNull();
    const pane = await openThreadInNewPane(A);
    expect(paneIds()).toEqual([pane.paneId]);
  });

  it('drops a deleted side chat from the stash of the thread it was hidden with', async () => {
    await mountMain(A);
    await openSideChatOn('main');
    await mountMain(B);

    removeThread(SIDE.id);
    await mountMain(A);

    expect(paneIds()).toEqual(['main']);
    expect(getBindingMock('GetThread')?.mock.calls ?? []).toEqual([]);
  });
});
