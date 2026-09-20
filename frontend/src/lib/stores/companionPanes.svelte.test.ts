import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createPane,
  destroyPane,
  focusPane,
  getFocusedPaneId,
  getPane,
  openThreadFromNavigation,
  resetPanesForTest,
} from './panes.svelte';
import { REVEAL_PANE_EVENT } from './eventNames';
import { setCompactLayoutForTest } from './layoutMode.svelte';
import {
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import {
  closeCompanion,
  closeCompanionsForSource,
  closePaneById,
  getCompanionPane,
  installCompanionPanes,
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
  toggleCompanion,
} from './companionPanes.svelte';
import { getThreads, replaceAllThreads } from './threads.svelte';
import { getToasts } from './toast.svelte';
import { makeThread } from '../../test/helpers/chat';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

function threadItem(paneId: string, widthPx = 560): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', widthPx };
}

function rect(left: number, right: number): DOMRect {
  return { left, right, top: 0, bottom: 800, width: right - left, height: 800, x: left, y: 0, toJSON: () => ({}) };
}

function paneIds(): string[] {
  return getPaneLayoutItems().map((item) => item.paneId);
}

beforeEach(() => {
  resetPanesForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  installCompanionPanes();
});

afterEach(() => {
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
});

describe('companionPanes store', () => {
  it('opens a companion after existing companions for the same source', () => {
    setPaneLayoutItemsForTest([threadItem('main', 900), threadItem('right')]);

    const plan = openCompanion('main', 'plan');
    const review = openCompanion('main', 'review');

    expect(plan).toEqual({ paneId: 'plan-main', kind: 'plan', sourcePaneId: 'main' });
    expect(review).toEqual({ paneId: 'review-main', kind: 'review', sourcePaneId: 'main' });
    expect(paneIds()).toEqual(['main', 'plan-main', 'review-main', 'right']);
    expect(getPaneLayoutItems()[1].widthPx).toBe(900);
    expect(isCompanionOpen('main', 'plan')).toBe(true);
    expect(getCompanionPane('plan-main')).toEqual(plan);
  });

  it('hosts an agent companion like any other panel kind', () => {
    setPaneLayoutItemsForTest([threadItem('main'), threadItem('right')]);
    createPane('main');

    const agent = openCompanion('main', 'agent');

    expect(agent).toEqual({ paneId: 'agent-main', kind: 'agent', sourcePaneId: 'main' });
    expect(paneIds()).toEqual(['main', 'agent-main', 'right']);
    expect(openCompanion('main', 'agent')).toBe(agent);

    destroyPane('main');

    expect(isCompanionOpen('main', 'agent')).toBe(false);
    expect(getCompanionPane('agent-main')).toBeNull();
  });

  it('does not open a duplicate companion for the same source and kind', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    const first = openCompanion('main', 'plan');
    const second = openCompanion('main', 'plan');

    expect(second).toBe(first);
    expect(getPaneLayoutItems().filter((item) => item.kind === 'plan')).toHaveLength(1);
  });

  it('toggles and closes companions without touching the source pane', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    expect(toggleCompanion('main', 'plan')).toBe(true);
    expect(isCompanionOpen('main', 'plan')).toBe(true);

    expect(toggleCompanion('main', 'plan')).toBe(false);
    expect(isCompanionOpen('main', 'plan')).toBe(false);
    expect(paneIds()).toEqual(['main']);

  });

  it('refuses to open when the source pane is absent from the layout', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);

    expect(openCompanion('ghost', 'plan')).toBeNull();
    expect(getPaneLayoutItems()).toEqual([threadItem('main')]);
  });

  it('take-control hugs its source, ahead of open panel companions', () => {
    setPaneLayoutItemsForTest([threadItem('main'), threadItem('right')]);
    openCompanion('main', 'plan');

    const takeControl = openCompanion('main', 'take-control');

    expect(takeControl).toEqual({
      paneId: 'take-control-main',
      kind: 'take-control',
      sourcePaneId: 'main',
    });
    // The shared top-border indicator reads source + terminal as one
    // entity, so nothing may sit between them.
    expect(paneIds()).toEqual(['main', 'take-control-main', 'plan-main', 'right']);

    // A panel companion opened afterwards appends after the run and
    // does not break the pairing.
    openCompanion('main', 'review');
    expect(paneIds()).toEqual(['main', 'take-control-main', 'plan-main', 'review-main', 'right']);
  });

  it('closes every companion for one source, leaving other sources alone', () => {
    setPaneLayoutItemsForTest([threadItem('main'), threadItem('right')]);
    openCompanion('main', 'plan');
    openCompanion('main', 'review');
    openCompanion('right', 'plan');

    closeCompanionsForSource('main');

    expect(isCompanionOpen('main', 'plan')).toBe(false);
    expect(isCompanionOpen('main', 'review')).toBe(false);
    expect(isCompanionOpen('right', 'plan')).toBe(true);
    expect(paneIds()).toEqual(['main', 'right', 'plan-right']);
  });

  it('a focused companion hands focus back to its source on close', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    openCompanion('main', 'review');
    focusPane('review-main');
    expect(getFocusedPaneId()).toBe('review-main');

    closeCompanion('review-main');

    expect(getFocusedPaneId()).toBe('main');
  });

  it('closing the companion on screen under compact reveals its source thread', () => {
    setCompactLayoutForTest(true);
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    openCompanion('main', 'plan');
    openCompanion('main', 'review');
    // The strip as compact renders it, with the review pane under its
    // centre. happy-dom has no layout, so each section states its extent.
    const strip = document.createElement('div');
    strip.className = 'compact-screen-thread';
    strip.getBoundingClientRect = () => rect(0, 412);
    for (const [paneId, onScreen] of [['main', false], ['plan-main', false], ['review-main', true]] as const) {
      const section = document.createElement('section');
      section.dataset.paneId = paneId;
      section.getBoundingClientRect = () => (onScreen ? rect(0, 412) : rect(412, 824));
      strip.appendChild(section);
    }
    document.body.appendChild(strip);
    const revealed: string[] = [];
    const onReveal = ((event: CustomEvent<{ paneId: string }>) => {
      revealed.push(event.detail.paneId);
    }) as EventListener;
    window.addEventListener(REVEAL_PANE_EVENT, onReveal);
    try {
      // The companion's own close control: the thread comes back, not the
      // plan pane that is left beside it.
      closeCompanion('review-main');
      expect(revealed).toEqual(['main']);
      expect(isCompanionOpen('main', 'plan')).toBe(true);

      // Closing a companion that is NOT on screen moves nothing.
      closeCompanion('plan-main');
      expect(revealed).toEqual(['main']);
    } finally {
      window.removeEventListener(REVEAL_PANE_EVENT, onReveal);
      strip.remove();
      setCompactLayoutForTest(false);
    }
  });

  it('closing a companion on the desktop never reveals anything', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    openCompanion('main', 'review');
    const onReveal = vi.fn();
    window.addEventListener(REVEAL_PANE_EVENT, onReveal);
    try {
      closeCompanion('review-main');
      expect(onReveal).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener(REVEAL_PANE_EVENT, onReveal);
    }
  });

  it('closing an unfocused companion leaves focus alone', () => {
    setPaneLayoutItemsForTest([threadItem('main'), threadItem('right')]);
    createPane('main');
    createPane('right');
    openCompanion('main', 'plan');
    focusPane('right');

    closeCompanion('plan-main');

    expect(getFocusedPaneId()).toBe('right');
  });

  it('destroying a source whose companion holds focus falls back to a surviving pane', () => {
    setPaneLayoutItemsForTest([threadItem('p1'), threadItem('p2')]);
    createPane('p1');
    createPane('p2');
    openCompanion('p1', 'review');
    focusPane('review-p1');

    destroyPane('p1');

    // The companion cascade-closed with its source; focus cannot dangle
    // on the dead companion id.
    expect(getFocusedPaneId()).toBe('p2');
  });

  it('opening a companion reveals it without moving focus', () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    focusPane('main');
    const onReveal = vi.fn();
    window.addEventListener(REVEAL_PANE_EVENT, onReveal);
    try {
      openCompanion('main', 'plan');
      expect(getFocusedPaneId()).toBe('main');
      expect(onReveal).toHaveBeenCalledTimes(1);
      expect((onReveal.mock.calls[0][0] as CustomEvent<{ paneId: string }>).detail.paneId).toBe('plan-main');
    } finally {
      window.removeEventListener(REVEAL_PANE_EVENT, onReveal);
    }
  });

  it('cascade-closes companions when the source pane is destroyed', () => {
    setPaneLayoutItemsForTest([threadItem('p1')]);
    createPane('p1');
    openCompanion('p1', 'plan');
    openCompanion('p1', 'review');
    openCompanion('p1', 'browser');
    openCompanion('p1', 'take-control');
    expect(paneIds()).toEqual(['p1', 'take-control-p1', 'plan-p1', 'review-p1', 'browser-p1']);

    destroyPane('p1');

    expect(isCompanionOpen('p1', 'plan')).toBe(false);
    expect(isCompanionOpen('p1', 'review')).toBe(false);
    expect(isCompanionOpen('p1', 'browser')).toBe(false);
    expect(isCompanionOpen('p1', 'take-control')).toBe(false);
    expect(getPaneLayoutItems()).toEqual([]);
  });
});

describe('side chat companions', () => {
  // A side chat is the one companion that IS a thread pane: it owns a scratch
  // thread that exists only while the pane does, so every close path has to
  // delete it.
  function openSideChatPane(sourcePaneId: string, threadId: string): string {
    const companion = openCompanion(sourcePaneId, 'side-chat');
    if (!companion) throw new Error('side chat companion did not open');
    const pane = createPane(companion.paneId);
    pane.replaceThread(makeThread({ id: threadId, mode: 'scratch' }));
    return companion.paneId;
  }

  beforeEach(() => {
    resetBindingMocks();
    replaceAllThreads([]);
    setBindingMock('DeleteThread', async () => {});
  });

  afterEach(() => {
    resetBindingMocks();
    replaceAllThreads([]);
  });

  it('deletes the scratch thread and drops the pane when the companion is closed', async () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    replaceAllThreads([makeThread({ id: 'side-1', mode: 'scratch' })]);
    const paneId = openSideChatPane('main', 'side-1');
    expect(paneIds()).toEqual(['main', 'side-chat-main']);

    closeCompanion(paneId);

    expect(isCompanionOpen('main', 'side-chat')).toBe(false);
    expect(getCompanionPane(paneId)).toBeNull();
    // destroyPane takes the layout item and the ThreadPane with it.
    expect(paneIds()).toEqual(['main']);
    expect(getPane(paneId)).toBeUndefined();
    await vi.waitFor(() => {
      expect(getBindingMock('DeleteThread')?.mock.calls).toEqual([['side-1']]);
    });
    // The deleted row leaves the frontend list too, so nothing can reopen it.
    expect(getThreads().map((thread) => thread.id)).toEqual([]);
  });

  it('deletes the scratch thread when the source pane is destroyed', async () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    openSideChatPane('main', 'side-1');

    destroyPane('main');

    expect(getPaneLayoutItems()).toEqual([]);
    await vi.waitFor(() => {
      expect(getBindingMock('DeleteThread')?.mock.calls).toEqual([['side-1']]);
    });
  });

  it('opens a thread aimed at the focused side chat in the source pane instead, closing the side chat', async () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    const main = createPane('main');
    const original = makeThread({ id: 'orig' });
    replaceAllThreads([original, makeThread({ id: 'side-1', mode: 'scratch' })]);
    main.replaceThread(original);
    const sidePaneId = openSideChatPane('main', 'side-1');
    focusPane(sidePaneId);

    // The sidebar opens into the focused pane; a side chat is a thread pane,
    // so without the redirect this would replace the fork it exists for.
    const other = makeThread({ id: 'other' });
    setBindingMock('SwitchThread', async () => other);
    setBindingMock('ListThreadSliceAround', async () => ({ items: [], oldestTurnIndex: -1, hasMore: false }));
    setBindingMock('ListRecentTurns', async () => []);
    setBindingMock('GetThreadLiveState', async () => null);
    setBindingMock('ListPendingInteractiveRequests', async () => null);
    const landed = await openThreadFromNavigation(other, sidePaneId);

    expect(landed).toBe(main);
    expect(main.threadId).toBe('other');
    expect(isCompanionOpen('main', 'side-chat')).toBe(false);
    expect(paneIds()).toEqual(['main']);
    expect(getFocusedPaneId()).toBe('main');
    await vi.waitFor(() => {
      expect(getBindingMock('DeleteThread')?.mock.calls).toEqual([['side-1']]);
    });
  });

  it('deletes the scratch thread when the source pane changes thread', async () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    openSideChatPane('main', 'side-1');

    // What ThreadPane calls on a switch, clear or draft start: the companion
    // belonged to the thread the fork was cut from.
    closeCompanionsForSource('main');

    expect(isCompanionOpen('main', 'side-chat')).toBe(false);
    expect(paneIds()).toEqual(['main']);
    await vi.waitFor(() => {
      expect(getBindingMock('DeleteThread')?.mock.calls).toEqual([['side-1']]);
    });
  });

  it('routes a pane-header close through the companion path, deleting the fork', async () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    const paneId = openSideChatPane('main', 'side-1');

    closePaneById(paneId);

    expect(getCompanionPane(paneId)).toBeNull();
    expect(paneIds()).toEqual(['main']);
    await vi.waitFor(() => {
      expect(getBindingMock('DeleteThread')?.mock.calls).toEqual([['side-1']]);
    });
  });

  it('leaves an ordinary thread pane alone when closed by id', () => {
    setPaneLayoutItemsForTest([threadItem('main'), threadItem('right')]);
    createPane('main');
    createPane('right');

    closePaneById('right');

    expect(paneIds()).toEqual(['main']);
    expect(getBindingMock('DeleteThread')?.mock.calls ?? []).toEqual([]);
  });

  it('reports a failed delete to the user rather than dropping it', async () => {
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    setBindingMock('DeleteThread', async () => {
      throw new Error('backend unreachable');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const paneId = openSideChatPane('main', 'side-1');

    closeCompanion(paneId);

    await vi.waitFor(() => {
      expect(getToasts().some((toast) => toast.type === 'error')).toBe(true);
    });
    consoleError.mockRestore();
  });

  it('drops the registration when the side chat pane is destroyed directly', () => {
    // Keep destroys the pane after promoting the thread, and a remote
    // thread:deleted closes panes showing the thread. Neither may leave a
    // registry entry pointing at a pane that no longer exists.
    setPaneLayoutItemsForTest([threadItem('main')]);
    createPane('main');
    const paneId = openSideChatPane('main', 'side-1');

    destroyPane(paneId);

    expect(getCompanionPane(paneId)).toBeNull();
    expect(isCompanionOpen('main', 'side-chat')).toBe(false);
    expect(paneIds()).toEqual(['main']);
  });
});
