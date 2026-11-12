// The terminal surface's find: the tab strip button opens the active tab's
// own bar, each tab keeps its own find, the pane's find commands follow the
// surface while it holds the pane's focus, and a closed tab, a switched tab
// and a hidden-and-reopened drawer leave no provider or open bar on the
// wrong terminal. The body is the stub, whose find provider records each
// call by terminal; the provider itself is terminalFind.svelte.test.ts and,
// over a real xterm, terminalFind.browser.test.ts.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import TerminalSurface from './TerminalSurface.svelte';
import {
  getTerminalFocused,
  getThreadTerminalState,
  resetThreadTerminalStatesForTest,
  terminalStateKeyForPane,
} from './terminalStore.svelte';
import { terminalFindKey } from './terminalFind.svelte';
import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';
import {
  closePaneFind,
  getPaneFind,
  getPaneFindProvider,
  isPaneFindOpen,
  openPaneFind,
  paneFindTarget,
  registerPaneFindProvider,
  resetPaneFindForTest,
} from '../../stores/paneFind.svelte';
import { setPaneLayoutItemsForTest, resetPaneLayoutForTest, removePaneLayoutItem } from '../../stores/paneLayout.svelte';
import { terminalFindCalls, resetTerminalFocusCalls } from '../../../test/mocks/terminalBodyFocusRecorder';

function terminalSummary(terminalID: string) {
  return {
    terminalID,
    threadID: 'thread-A',
    shell: '/bin/bash',
    cwd: '/tmp',
    rows: 24,
    cols: 80,
    pid: 1,
    startedAt: 0,
    running: true,
    exitCode: 0,
    exitReason: '',
  };
}

vi.mock('../../stores/bindings', () => ({
  OpenTerminal: vi.fn(),
  CloseTerminal: vi.fn(async () => {}),
  ListTerminals: vi.fn(async () => []),
  RefreshTerminal: vi.fn(async () => {}),
  TerminalOpenOptions: class TerminalOpenOptions {},
}));

vi.mock('../../stores/toast.svelte', () => ({ addToast: vi.fn(), addErrorToast: vi.fn() }));

vi.mock('./TerminalBody.svelte', async () => ({
  default: (await import('../../../test/mocks/StubTerminalBody.svelte')).default,
}));

const PANE = 'pane-1';
const key1 = terminalFindKey(PANE, 't1');
const key2 = terminalFindKey(PANE, 't2');

function makeSurface(): ThreadTerminalSurfaceContext {
  return {
    paneId: PANE,
    threadId: 'thread-A',
    workspacePath: undefined,
    setVisible: () => {},
    acquireResizeLease: () => null,
    consumeFocusRequest: () => false,
  };
}

/** The pane element the surface sits in, with a timeline beside it. */
function paneElement(): { pane: HTMLElement; timeline: HTMLElement } {
  const pane = document.createElement('section');
  pane.dataset.paneId = PANE;
  const timeline = document.createElement('div');
  timeline.tabIndex = -1;
  timeline.dataset.testid = 'timeline';
  pane.appendChild(timeline);
  document.body.appendChild(pane);
  return { pane, timeline };
}

function mount(target: HTMLElement) {
  return render(TerminalSurface, { target, props: { surface: makeSurface(), manual: true } });
}

async function settle(): Promise<void> {
  for (let i = 0; i < 4; i += 1) await Promise.resolve();
  await tick();
}

beforeEach(() => {
  resetThreadTerminalStatesForTest();
  resetTerminalFocusCalls();
  resetPaneFindForTest();
  setPaneLayoutItemsForTest([{ id: PANE, paneId: PANE, kind: 'thread', widthPx: 600 }]);
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
    cb(0);
    return 0;
  });
  const handle = getThreadTerminalState(terminalStateKeyForPane('thread-A', PANE));
  handle.addTab(terminalSummary('t1'));
  handle.addTab(terminalSummary('t2'));
  handle.setActive('t1');
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  resetPaneFindForTest();
  resetPaneLayoutForTest();
  document.body.replaceChildren();
});

describe('TerminalSurface find', () => {
  it('opens the active tab\'s bar from the strip button, and a close returns focus to the terminal', async () => {
    const { pane } = paneElement();
    const { getByTestId, queryByTestId } = mount(pane);
    await tick();
    expect(queryByTestId('pane-find-bar')).toBeNull();

    await fireEvent.click(getByTestId('terminal-find'));
    await tick();
    const input = getByTestId('pane-find-input') as HTMLInputElement;
    expect(document.activeElement).toBe(input);
    expect(input.placeholder).toBe('Find in terminal');
    // The bar floats over the terminal, after it in the same host.
    const surface = getByTestId('terminal-surface');
    const bar = getByTestId('pane-find-bar');
    expect(surface.contains(bar)).toBe(true);
    const overlay = bar.closest('[data-terminal-find-overlay]')!;
    expect(overlay.classList.contains('absolute')).toBe(true);
    expect(overlay.previousElementSibling).toBe(getByTestId('stub-terminal-body-t1'));
    // Focus in the bar is focus in the pane's terminal.
    expect(getTerminalFocused(PANE)).toBe(true);
    getByTestId('stub-terminal-input-t1').focus();
    expect(getTerminalFocused(PANE)).toBe(false);
    input.focus();
    expect(getTerminalFocused(PANE)).toBe(true);

    await fireEvent.input(input, { target: { value: 'needle' } });
    expect(terminalFindCalls).toEqual(['t1:search:needle']);
    expect(getByTestId('pane-find-count').textContent?.trim()).toBe('1/1');

    // What find.close does on Escape: the pane's target is this tab.
    expect(paneFindTarget(PANE)).toBe(key1);
    closePaneFind(paneFindTarget(PANE)!);
    await settle();
    expect(queryByTestId('pane-find-bar')).toBeNull();
    expect(terminalFindCalls.at(-1)).toBe('t1:clear');
    expect(document.activeElement).toBe(getByTestId('stub-terminal-input-t1'));
  });

  it('a tab switch shows the new tab\'s own find, and the old tab\'s bar again on return', async () => {
    const { pane } = paneElement();
    const { getByTestId, queryByTestId } = mount(pane);
    await tick();
    await fireEvent.click(getByTestId('terminal-find'));
    await fireEvent.input(getByTestId('pane-find-input'), { target: { value: 'needle' } });

    await fireEvent.click(getByTestId('terminal-tab-t2'));
    await settle();
    expect(queryByTestId('pane-find-bar')).toBeNull();
    expect(getPaneFindProvider(key1)).toBeNull();
    expect(getPaneFindProvider(key2)).not.toBeNull();
    expect(terminalFindCalls).toEqual(['t1:search:needle', 't1:clear']);
    expect(paneFindTarget(PANE)).toBe(key2);
    expect(isPaneFindOpen(paneFindTarget(PANE))).toBe(false);

    terminalFindCalls.length = 0;
    await fireEvent.click(getByTestId('terminal-tab-t1'));
    await settle();
    expect((getByTestId('pane-find-input') as HTMLInputElement).value).toBe('needle');
    // t2 had no query to search; t1 searches its own again.
    expect(terminalFindCalls.filter((call) => call.includes('search'))).toEqual(['t1:search:needle']);
  });

  it('a closed tab takes its find state with it and leaves its sibling\'s', async () => {
    const { pane } = paneElement();
    const { getByTestId } = mount(pane);
    await tick();
    await fireEvent.click(getByTestId('terminal-find'));
    await fireEvent.input(getByTestId('pane-find-input'), { target: { value: 'one' } });
    await fireEvent.click(getByTestId('terminal-tab-t2'));
    await settle();
    await fireEvent.click(getByTestId('terminal-find'));
    await fireEvent.input(getByTestId('pane-find-input'), { target: { value: 'two' } });

    await fireEvent.click(getByTestId('terminal-tab-close-t1'));
    await settle();
    expect(getPaneFind(key1)).toBeNull();
    expect(getPaneFind(key2)).toMatchObject({ open: true, query: 'two' });
  });

  it('a hidden and reopened drawer shows the open bar on its own terminal and searches it again', async () => {
    const first = paneElement();
    const view = mount(first.pane);
    await tick();
    await fireEvent.click(view.getByTestId('terminal-find'));
    await fireEvent.input(view.getByTestId('pane-find-input'), { target: { value: 'needle' } });
    view.unmount();
    expect(getPaneFindProvider(key1)).toBeNull();
    expect(paneFindTarget(PANE)).toBe(PANE);
    expect(terminalFindCalls.at(-1)).toBe('t1:clear');

    const reopened = mount(first.pane);
    await settle();
    expect((reopened.getByTestId('pane-find-input') as HTMLInputElement).value).toBe('needle');
    expect(terminalFindCalls.at(-1)).toBe('t1:search:needle');
    expect(getPaneFindProvider(key2)).toBeNull();
  });

  it('the pane\'s commands follow focus into the surface and back out to the pane\'s own find', async () => {
    const { pane, timeline } = paneElement();
    registerPaneFindProvider(PANE, { noun: 'thread', result: { count: 0, current: -1, pending: false }, search: () => {}, clear: () => {} });
    const { getByTestId } = mount(pane);
    await tick();
    // Nothing in the surface has been focused or pressed yet.
    expect(paneFindTarget(PANE)).toBe(PANE);

    getByTestId('stub-terminal-input-t1').focus();
    expect(paneFindTarget(PANE)).toBe(key1);
    timeline.focus();
    expect(paneFindTarget(PANE)).toBe(PANE);
    await fireEvent.pointerDown(getByTestId('terminal-tab-t2'));
    expect(paneFindTarget(PANE)).toBe(key1);
    await fireEvent.pointerDown(timeline);
    expect(paneFindTarget(PANE)).toBe(PANE);

    // Opened from a command while the surface is the target.
    getByTestId('stub-terminal-input-t1').focus();
    expect(openPaneFind(paneFindTarget(PANE)!)).toBe(true);
    await tick();
    expect(getByTestId('pane-find-bar')).toBeTruthy();
    expect(getPaneFind(PANE)).toBeNull();
  });

  it('the pane leaving the layout drops every tab\'s find', async () => {
    const { pane } = paneElement();
    const view = mount(pane);
    await tick();
    await fireEvent.click(view.getByTestId('terminal-find'));
    view.unmount();
    removePaneLayoutItem(PANE, { persist: false });
    expect(getPaneFind(key1)).toBeNull();
    expect(paneFindTarget(PANE)).toBe(PANE);
  });
});
