import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  activeToolKind,
  addToolPaneClosedObserver,
  closeToolPane,
  focusedToolDetailOpen,
  focusedToolKind,
  installToolPanes,
  isToolOpen,
  lastToolKind,
  noteToolUsed,
  openTool,
  registerToolDetailProbe,
  resetToolPanesForTest,
  syncToolPanesFromAppStorage,
  toggleTool,
  toolKindOf,
} from './toolPanes.svelte';
import {
  createPane,
  destroyPane,
  focusAdjacentPane,
  focusPane,
  focusedOpenTargetPane,
  getAllPanes,
  getFocusedPaneId,
  getPane,
  openEmptyPane,
  openThreadFromNavigation,
  openThreadIdInNewPane,
  openThreadInPane,
  resetPanesForTest,
} from './panes.svelte';
import {
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import {
  closeFocusedPaneOrCompanion,
  closePaneById,
  installCompanionPanes,
  isCompanionOpen,
  openCompanion,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
import { getCompactScreen, setCompactLayoutForTest, showCompactThread } from './layoutMode.svelte';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';
import { REVEAL_PANE_EVENT } from './eventNames';
import { prependThread, replaceAllThreads } from './threads.svelte';
import { openDraftThreadForProject } from './threadCreation.svelte';
import { addProjectLocal, resetProjectsForTest } from './projects.svelte';
import { openWorkflowThreadById } from './workflowThreads';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Project, Thread } from '../types/models';

function threadItem(paneId: string, widthPx = 560): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', widthPx };
}

function toolItem(kind: 'pulls' | 'workflows', widthPx = 700): PaneLayoutItem {
  return { id: `tool-${kind}`, paneId: `tool-${kind}`, kind, widthPx };
}

function layoutIds(): string[] {
  return getPaneLayoutItems().map((item) => item.paneId);
}

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp/p1',
    projectPath: '/tmp/p1',
    projectId: 'project-1',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function makeProject(): Project {
  return {
    id: 'project-1',
    path: '/tmp/p1',
    name: 'Project One',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

function mockThreadSwitch(): void {
  setBindingMock('SwitchThread', async (threadId: unknown) => makeThread({ id: String(threadId) }));
  setBindingMock('ListThreadSliceAround', async () => ({ items: [], oldestTurnIndex: -1, hasMore: false }));
  setBindingMock('ListRecentTurns', async () => []);
  setBindingMock('GetThreadLiveState', async () => null);
  setBindingMock('ListPendingInteractiveRequests', async () => null);
  setBindingMock('AutoResumeThread', async () => {});
}

/** A layout of thread panes, each registered, focus on `focus`. */
function threadLayout(items: PaneLayoutItem[], focus: string): void {
  setPaneLayoutItemsForTest(items);
  for (const item of items) if (item.kind === 'thread') createPane(item.paneId);
  focusPane(focus);
}

function resetAll(): void {
  resetToolPanesForTest();
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
  setPaneLayoutItemsForTest([]);
  resetAppStorageForTest();
  replaceAllThreads([]);
  setCompactLayoutForTest(false);
}

beforeEach(() => {
  resetAll();
  installCompanionPanes();
  installToolPanes();
  mockThreadSwitch();
});

afterEach(() => {
  for (const pane of getAllPanes().values()) pane.clear();
  resetAll();
  resetBindingMocks();
});

describe('opening a tool', () => {
  it('replaces the focused thread pane in place, at its index and width', () => {
    threadLayout([threadItem('left'), threadItem('mid', 640), threadItem('right')], 'mid');
    openTool('workflows');
    expect(layoutIds()).toEqual(['left', 'tool-workflows', 'right']);
    expect(getPaneLayoutItems()[1].widthPx).toBe(640);
    expect(getPane('mid')).toBeUndefined();
    expect(getFocusedPaneId()).toBe('tool-workflows');
    expect(focusedToolKind()).toBe('workflows');
  });

  it('closes the replaced thread pane through its close path, companions included', () => {
    threadLayout([threadItem('left'), threadItem('mid')], 'mid');
    openCompanion('mid', 'review', { reveal: false });
    expect(layoutIds()).toEqual(['left', 'mid', 'review-mid']);
    openTool('pulls');
    expect(layoutIds()).toEqual(['left', 'tool-pulls']);
    expect(isCompanionOpen('mid', 'review')).toBe(false);
  });

  it('resolves a focused companion to the thread pane it belongs to', () => {
    threadLayout([threadItem('left'), threadItem('mid'), threadItem('right')], 'mid');
    openCompanion('mid', 'review', { reveal: false });
    focusPane('review-mid');
    openTool('workflows');
    expect(layoutIds()).toEqual(['left', 'tool-workflows', 'right']);
    expect(getPane('mid')).toBeUndefined();
  });

  it('gives a focused tool pane\'s slot to the other tool and tells observers', () => {
    const closed = vi.fn();
    const off = addToolPaneClosedObserver(closed);
    threadLayout([threadItem('left'), toolItem('workflows', 820)], 'tool-workflows');
    openTool('pulls');
    expect(layoutIds()).toEqual(['left', 'tool-pulls']);
    expect(getPaneLayoutItems()[1].widthPx).toBe(820);
    expect(closed).toHaveBeenCalledWith('workflows');
    off();
  });

  it('appends when no pane holds focus', () => {
    threadLayout([threadItem('left')], 'left');
    focusPane('gone');
    setPaneLayoutItemsForTest([threadItem('left')]);
    destroyPane('left');
    expect(getFocusedPaneId()).toBeNull();
    openTool('workflows');
    expect(layoutIds()).toEqual(['tool-workflows']);
    expect(getFocusedPaneId()).toBe('tool-workflows');
  });

  it('inserts a new pane right of the focused one on a new-pane open, replacing nothing', () => {
    threadLayout([threadItem('left'), threadItem('right')], 'left');
    openTool('workflows', { newPane: true });
    expect(layoutIds()).toEqual(['left', 'tool-workflows', 'right']);
    expect(getPane('left')).toBeDefined();
    expect(getFocusedPaneId()).toBe('tool-workflows');
  });

  it('lands a new-pane open after the focused pane\'s companions, never inside its block', () => {
    threadLayout([threadItem('left'), threadItem('right')], 'left');
    openCompanion('left', 'review', { reveal: false });
    openTool('pulls', { newPane: true });
    expect(layoutIds()).toEqual(['left', 'review-left', 'tool-pulls', 'right']);
  });

  it('reveals and focuses an already-open tool instead of opening a second one', () => {
    threadLayout([toolItem('workflows'), threadItem('right')], 'right');
    const reveals: string[] = [];
    const onReveal = (event: Event) => reveals.push((event as CustomEvent<{ paneId: string }>).detail.paneId);
    window.addEventListener(REVEAL_PANE_EVENT, onReveal);
    openTool('workflows');
    window.removeEventListener(REVEAL_PANE_EVENT, onReveal);
    expect(layoutIds()).toEqual(['tool-workflows', 'right']);
    expect(getPane('right')).toBeDefined();
    expect(getFocusedPaneId()).toBe('tool-workflows');
    expect(isToolOpen('workflows')).toBe(true);
    expect(reveals).toEqual(['tool-workflows']);
  });

  it('toggles: opens, then closes only when that tool holds focus', () => {
    threadLayout([threadItem('left'), threadItem('right')], 'left');
    toggleTool('workflows');
    expect(isToolOpen('workflows')).toBe(true);
    focusPane('right');
    toggleTool('workflows');
    expect(isToolOpen('workflows')).toBe(true);
    expect(getFocusedPaneId()).toBe('tool-workflows');
    toggleTool('workflows');
    expect(isToolOpen('workflows')).toBe(false);
  });
});

describe('closing a tool', () => {
  it('removes the item and moves focus to the pane on its left', () => {
    threadLayout([threadItem('left'), toolItem('workflows'), threadItem('right')], 'tool-workflows');
    closeToolPane('tool-workflows');
    expect(layoutIds()).toEqual(['left', 'right']);
    expect(getFocusedPaneId()).toBe('left');
  });

  it('moves focus right when the tool was the first pane', () => {
    threadLayout([toolItem('pulls'), threadItem('right')], 'tool-pulls');
    closeToolPane('tool-pulls');
    expect(getFocusedPaneId()).toBe('right');
  });

  it('routes the pane close control and pane.close to the tool close', () => {
    threadLayout([threadItem('left'), toolItem('workflows'), toolItem('pulls')], 'tool-pulls');
    closeFocusedPaneOrCompanion();
    expect(layoutIds()).toEqual(['left', 'tool-workflows']);
    expect(getFocusedPaneId()).toBe('tool-workflows');
    closePaneById('tool-workflows');
    expect(layoutIds()).toEqual(['left']);
    expect(getPane('left')).toBeDefined();
  });

  it('falls back to the compact list only when the strip is left empty', () => {
    setCompactLayoutForTest(true);
    threadLayout([threadItem('left'), toolItem('workflows')], 'tool-workflows');
    showCompactThread();
    closeToolPane('tool-workflows');
    expect(getCompactScreen()).toBe('thread');
    destroyPane('left');
    expect(getCompactScreen()).toBe('list');
  });

  it('keeps the compact thread screen when the last thread closes beside a tool', () => {
    setCompactLayoutForTest(true);
    threadLayout([threadItem('left'), toolItem('pulls')], 'left');
    showCompactThread();
    destroyPane('left');
    expect(getCompactScreen()).toBe('thread');
    expect(getFocusedPaneId()).toBe('tool-pulls');
  });

  it('ignores an id that is not a tool pane', () => {
    threadLayout([threadItem('left')], 'left');
    closeToolPane('left');
    expect(layoutIds()).toEqual(['left']);
  });
});

describe('a thread opened while a tool pane is focused', () => {
  it('takes the tool pane\'s index and width from a sidebar open', async () => {
    const closed = vi.fn();
    const off = addToolPaneClosedObserver(closed);
    threadLayout([threadItem('left'), toolItem('workflows', 900), threadItem('right')], 'tool-workflows');
    const pane = await openThreadInPane(makeThread({ id: 't-new' }));
    expect(layoutIds()).toEqual(['left', pane.paneId, 'right']);
    expect(getPaneLayoutItems()[1].widthPx).toBe(900);
    expect(pane.threadId).toBe('t-new');
    expect(getFocusedPaneId()).toBe(pane.paneId);
    expect(isToolOpen('workflows')).toBe(false);
    expect(closed).toHaveBeenCalledWith('workflows');
    off();
  });

  it('does the same for a navigation open (sidebar row, thread jump, palette)', async () => {
    threadLayout([toolItem('pulls'), threadItem('right')], 'tool-pulls');
    const pane = await openThreadFromNavigation(makeThread({ id: 't-nav' }));
    expect(layoutIds()).toEqual([pane.paneId, 'right']);
  });

  it('reveals a thread already open elsewhere and leaves the tool pane alone', async () => {
    threadLayout([threadItem('left'), toolItem('workflows')], 'left');
    await openThreadInPane(makeThread({ id: 't-open' }));
    focusPane('tool-workflows');
    const pane = await openThreadInPane(makeThread({ id: 't-open' }));
    expect(pane.paneId).toBe('left');
    expect(layoutIds()).toEqual(['left', 'tool-workflows']);
    expect(getFocusedPaneId()).toBe('left');
  });

  it('mints main for a tool-only layout, and never a second main', async () => {
    threadLayout([toolItem('workflows')], 'tool-workflows');
    const first = await openThreadInPane(makeThread({ id: 't-1' }));
    expect(first.paneId).toBe('main');
    openTool('pulls', { newPane: true });
    const second = await openThreadInPane(makeThread({ id: 't-2' }));
    expect(second.paneId).not.toBe('main');
    expect(layoutIds()).toEqual(['main', second.paneId]);
  });

  it('does not reuse main while a layout item still holds it', () => {
    // A `main` item with no registry pane (it renders "Pane unavailable")
    // still owns the id.
    setPaneLayoutItemsForTest([threadItem('main'), toolItem('workflows')]);
    focusPane('tool-workflows');
    const pane = focusedOpenTargetPane();
    expect(pane.paneId).not.toBe('main');
    expect(layoutIds()).toEqual(['main', pane.paneId]);
  });

  it('opens a new-thread draft in the tool pane\'s place', async () => {
    resetProjectsForTest();
    addProjectLocal(makeProject());
    setBindingMock('GetThreadDefaults', async () => ({
      provider: 'claude', model: 'claude-sonnet-4-6', reasoningEffort: 'high', fastMode: false,
      contextWindow: 200000, runtimeMode: 'full-access', branch: 'main', workspacePath: '/tmp/p1',
    }));
    threadLayout([threadItem('left'), toolItem('workflows')], 'tool-workflows');
    const pane = await openDraftThreadForProject({ projectId: 'project-1', targetPane: null });
    expect(pane?.hasDraftPlaceholder).toBe(true);
    expect(layoutIds()).toEqual(['left', pane!.paneId]);
    expect(isToolOpen('workflows')).toBe(false);
    resetProjectsForTest();
  });

  it('opens a workflow thread from the run map in the workflows pane\'s place', async () => {
    prependThread(makeThread({ id: 'phase-thread' }));
    threadLayout([threadItem('left'), toolItem('workflows')], 'tool-workflows');
    const paneId = await openWorkflowThreadById('phase-thread');
    expect(layoutIds()).toEqual(['left', paneId]);
    expect(getPane(paneId!)?.threadId).toBe('phase-thread');
  });

  it('reveals a workflow thread that is already open and keeps the workflows pane', async () => {
    prependThread(makeThread({ id: 'phase-thread' }));
    threadLayout([threadItem('left'), toolItem('workflows')], 'left');
    await openThreadInPane(makeThread({ id: 'phase-thread' }));
    focusPane('tool-workflows');
    const paneId = await openWorkflowThreadById('phase-thread');
    expect(paneId).toBe('left');
    expect(getFocusedPaneId()).toBe('left');
    expect(layoutIds()).toEqual(['left', 'tool-workflows']);
  });

  it('opens an empty pane beside the tool, replacing nothing', () => {
    threadLayout([toolItem('pulls'), threadItem('right')], 'tool-pulls');
    const pane = openEmptyPane();
    expect(layoutIds()).toEqual(['tool-pulls', pane.paneId, 'right']);
  });

  it('opens a dropped thread at the drop slot, replacing nothing', async () => {
    prependThread(makeThread({ id: 'dropped' }));
    threadLayout([toolItem('workflows'), threadItem('right')], 'tool-workflows');
    const pane = await openThreadIdInNewPane('dropped', 0);
    expect(layoutIds()).toEqual([pane!.paneId, 'tool-workflows', 'right']);
  });
});

describe('focus and the active tool', () => {
  it('stops on a tool pane during pane navigation', () => {
    threadLayout([threadItem('left'), toolItem('workflows'), threadItem('right')], 'left');
    expect(focusAdjacentPane(1)?.paneId).toBe('tool-workflows');
    expect(focusAdjacentPane(1)?.paneId).toBe('right');
    expect(focusAdjacentPane(-1)?.paneId).toBe('tool-workflows');
  });

  it('names a tool pane by its kind and nothing else', () => {
    threadLayout([threadItem('left'), toolItem('pulls')], 'left');
    expect(toolKindOf('tool-pulls')).toBe('pulls');
    expect(toolKindOf('left')).toBeNull();
    expect(toolKindOf(null)).toBeNull();
    expect(focusedToolKind()).toBeNull();
  });

  it('labels the focused tool, else the one used last, and persists the last one', () => {
    expect(activeToolKind()).toBe('pulls');
    threadLayout([threadItem('left')], 'left');
    openTool('workflows', { newPane: true });
    expect(activeToolKind()).toBe('workflows');
    focusPane('left');
    expect(activeToolKind()).toBe('workflows');
    expect(appStorageGet('tools:last')).toBe('workflows');
    noteToolUsed('pulls');
    expect(lastToolKind()).toBe('pulls');
  });

  it('adopts the stored last tool once app storage hydrates, ignoring junk', () => {
    appStorageSet('tools:last', 'workflows');
    syncToolPanesFromAppStorage();
    expect(lastToolKind()).toBe('workflows');
    appStorageSet('tools:last', 'nope');
    syncToolPanesFromAppStorage();
    expect(lastToolKind()).toBe('pulls');
  });

  it('reports the focused tool\'s detail step through its probe', () => {
    let open = false;
    const restore = registerToolDetailProbe('pulls', () => open);
    threadLayout([threadItem('left'), toolItem('pulls')], 'left');
    open = true;
    expect(focusedToolDetailOpen()).toBe(false);
    focusPane('tool-pulls');
    expect(focusedToolDetailOpen()).toBe(true);
    open = false;
    expect(focusedToolDetailOpen()).toBe(false);
    restore();
  });

  it('restores the probe a registration replaced when it is disposed', () => {
    threadLayout([toolItem('pulls')], 'tool-pulls');
    const restoreFirst = registerToolDetailProbe('pulls', () => true);
    const restoreSecond = registerToolDetailProbe('pulls', () => false);
    expect(focusedToolDetailOpen()).toBe(false);
    restoreSecond();
    expect(focusedToolDetailOpen()).toBe(true);
    restoreFirst();
    expect(focusedToolDetailOpen()).toBe(false);
  });
});
