import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserAddressBarPaneHarness from './BrowserAddressBarPaneHarness.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';
import { resetDevServersForTest } from '../../stores/devServers.svelte';
import { airspaceSurfaces, resetAirspaceForTest } from '../../utils/paneAirspace.svelte';
import { makeCommandContext } from '../../stores/builtinCommands.svelte';
import { attachBrowserPaneForTest } from '../../../test/helpers/browserPane';
import { resetFocusIntentForTest } from '../../utils/focusIntent';
import { forgetBackendEntities, forgetThread, noteThread } from '../../transport/entityIndex';
import { HOME_BACKEND } from '../../transport/backendKey';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';

const THREAD = 'thread-1';

function page(id: string, url: string, title = id) {
  return { id, url, title, canGoBack: false, canGoForward: false };
}

const PAGES = [page('a', 'https://example.test/a', 'Page a'), page('b', 'http://localhost:5173/settings', 'Settings')];

function seed(activePageId = 'a') {
  push('worktree', 'L.1', activePageId);
}

/** The thread's state push, with the Manager's recent-address forget generation. */
function push(jar: string, recentGen: string, activePageId = 'a'): void {
  applyBrowserCompanionState({ kind: 'state', threadId: THREAD, activePageId, visible: true, jar, recentGen, pages: PAGES } as never);
}

/** Mounts the row on `activePage`, or with 'state' on the page the thread's admitted state shows, as the pane does. */
function mount(activePage: (typeof PAGES)[number] | 'state' = PAGES[0]) {
  const onAct = vi.fn(async (..._args: unknown[]) => {});
  const onLeave = vi.fn();
  const view = render(BrowserAddressBarPaneHarness, {
    threadId: THREAD,
    activePage: activePage === 'state' ? undefined : (activePage as never),
    sizeLabel: '',
    promotePaneId: null,
    onAct,
    onRevealPageFile: async () => {},
    onOpenFile: () => {},
    onLeave,
  });
  const input = view.getByRole('textbox', { name: 'Address' }) as HTMLInputElement;
  return { view, input, onAct, onLeave };
}

function key(input: HTMLInputElement, init: KeyboardEventInit): KeyboardEvent {
  const event = new KeyboardEvent('keydown', { bubbles: true, cancelable: true, ...init });
  input.dispatchEvent(event);
  return event;
}

async function type(input: HTMLInputElement, text: string): Promise<void> {
  await fireEvent.input(input, { target: { value: text } });
  await tick();
}

function rows(): HTMLElement[] {
  return Array.from(document.querySelectorAll<HTMLElement>('[data-testid="browser-address-suggestion"]'));
}

function rowText(): string[] {
  return rows().map((row) => `${row.dataset.kind} ${row.title}`);
}

describe('<BrowserAddressSuggestions>', () => {
  let recent: ReturnType<typeof vi.fn>;
  beforeEach(async () => {
    resetBrowserCompanionForTest();
    resetDevServersForTest();
    resetAirspaceForTest();
    recent = setBindingMock('BrowserCompanionRecentAddresses', vi.fn(async () => [
      { url: 'https://example.test/a', title: 'Page a' },
      { url: 'https://github.test/org/repo', title: 'Repo' },
    ]));
    setBindingMock('GetDevServers', async () => ({
      previewHost: '',
      servers: [{ port: 8080, threadId: THREAD, process: 'node', listening: true, allowed: false, source: 'seen', scheme: 'http' }],
    }));
    seed();
    // The list reads from the pane's attachment, as a mounted pane has one.
    await attachBrowserPaneForTest(THREAD, 'mount-1');
  });
  afterEach(() => {
    resetBrowserCompanionForTest();
    resetDevServersForTest();
    resetBindingMocks();
    resetAirspaceForTest();
    resetFocusIntentForTest();
  });

  it('reads the recent addresses and dev servers on focus and offers them for typed text', async () => {
    const { input } = mount();
    input.focus();
    await waitFor(() => expect(recent).toHaveBeenCalledWith(THREAD));
    expect(rows()).toEqual([]);
    await type(input, 'local');
    await waitFor(() => expect(rowText()).toEqual(['tab http://localhost:5173/settings', 'dev-server http://localhost:8080/']));
    await type(input, 'test');
    // The field's own tab, and its address among the recent ones, are not offered.
    expect(rowText()).toEqual(['recent https://github.test/org/repo']);
    expect(input.value).toBe('test');
    await type(input, '');
    expect(rows()).toEqual([]);
  });

  it('moves with the arrows without touching the typed text, and Enter takes the highlighted row', async () => {
    const { input, onAct, onLeave } = mount();
    input.focus();
    await type(input, 'o');
    await waitFor(() => expect(rows().length).toBe(3));
    expect(key(input, { key: 'ArrowDown' }).defaultPrevented).toBe(true);
    await tick();
    expect(rows().map((row) => row.getAttribute('aria-selected'))).toEqual(['true', 'false', 'false']);
    expect(input.getAttribute('aria-activedescendant')).toBe(rows()[0].id);
    key(input, { key: 'ArrowUp' });
    await tick();
    // Up from the first row returns to the typed text.
    expect(rows().map((row) => row.getAttribute('aria-selected'))).toEqual(['false', 'false', 'false']);
    expect(input.hasAttribute('aria-activedescendant')).toBe(false);
    key(input, { key: 'ArrowUp' });
    await tick();
    expect(rows()[2].getAttribute('aria-selected')).toBe('true');
    expect(input.value).toBe('o');
    expect(key(input, { key: 'Enter' }).defaultPrevented).toBe(true);
    await tick();
    expect(onAct).toHaveBeenCalledWith({ kind: 'navigate', pageId: 'a', address: 'https://github.test/org/repo' });
    expect(onLeave).toHaveBeenCalledTimes(1);
    expect(rows()).toEqual([]);
  });

  it('switches to a tab, then leaves the field for it', async () => {
    let finish: () => void = () => {};
    const { input, onAct, onLeave } = mount();
    onAct.mockImplementation(() => new Promise<void>((resolve) => (finish = resolve)));
    input.focus();
    await type(input, 'settings');
    await waitFor(() => expect(rows().length).toBe(1));
    key(input, { key: 'ArrowDown' });
    await tick();
    key(input, { key: 'Enter' });
    expect(onAct).toHaveBeenCalledWith({ kind: 'activate', pageId: 'b' });
    expect(onLeave).not.toHaveBeenCalled();
    finish();
    await waitFor(() => expect(onLeave).toHaveBeenCalledTimes(1));
  });

  it('does not take focus back after a tab switch if the user moved on', async () => {
    let finish: () => void = () => {};
    const { input, onAct, onLeave } = mount();
    onAct.mockImplementation(() => new Promise<void>((resolve) => (finish = resolve)));
    input.focus();
    await type(input, 'settings');
    await waitFor(() => expect(rows().length).toBe(1));
    key(input, { key: 'ArrowDown' });
    key(input, { key: 'Enter' });
    input.blur();
    finish();
    await Promise.resolve();
    await tick();
    expect(onLeave).not.toHaveBeenCalled();
  });

  it('keeps what the user typed while a tab switch was answered', async () => {
    let finish: () => void = () => {};
    const { input, onAct, onLeave } = mount('state');
    onAct.mockImplementation(() => new Promise<void>((resolve) => (finish = resolve)));
    input.focus();
    await type(input, 'settings');
    await waitFor(() => expect(rows().length).toBe(1));
    key(input, { key: 'ArrowDown' });
    key(input, { key: 'Enter' });
    expect(onAct).toHaveBeenCalledWith({ kind: 'activate', pageId: 'b' });
    // The field still has focus; the user starts another address.
    await type(input, 'https://next.test/');
    // The switch's answer: the state with b active, then the reply.
    seed('b');
    await tick();
    finish();
    await Promise.resolve();
    await tick();
    expect(onLeave).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(input);
    expect(input.value).toBe('https://next.test/');
    // The text was typed for the tab the suggestion chose, and goes there.
    key(input, { key: 'Enter' });
    expect(onAct).toHaveBeenLastCalledWith({ kind: 'navigate', pageId: 'b', address: 'https://next.test/' });
  });

  // The switch the suggestion asked for did not happen: what was typed for
  // that tab goes with it, and the field shows the tab still active.
  it('drops what was typed for a tab switch that did not land', async () => {
    let finish: () => void = () => {};
    const { input, onAct } = mount('state');
    onAct.mockImplementation(() => new Promise<void>((resolve) => (finish = resolve)));
    input.focus();
    await type(input, 'settings');
    await waitFor(() => expect(rows().length).toBe(1));
    key(input, { key: 'ArrowDown' });
    key(input, { key: 'Enter' });
    await type(input, 'https://next.test/');
    finish();
    await Promise.resolve();
    await tick();
    expect(input.value).toBe('https://example.test/a');
  });

  it('keeps the draft over the page address changing until the user leaves the field', async () => {
    const { input } = mount('state');
    input.focus();
    await type(input, 'https://draft.test/');
    applyBrowserCompanionState({
      kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, jar: 'worktree', recentGen: 'L.1',
      pages: [page('a', 'https://example.test/moved', 'Page a'), PAGES[1]],
    } as never);
    await tick();
    expect(input.value).toBe('https://draft.test/');
    input.blur();
    await tick();
    expect(input.value).toBe('https://example.test/moved');
  });

  it('follows the page while focused with no edit, keeping the text selected', async () => {
    const { input } = mount('state');
    input.focus();
    input.select();
    seed('b');
    await tick();
    expect(input.value).toBe('http://localhost:5173/settings');
    expect([input.selectionStart, input.selectionEnd]).toEqual([0, input.value.length]);
  });

  it('Escape abandons the draft for the address the page has now', async () => {
    const { input } = mount('state');
    input.focus();
    await type(input, 'https://draft.test/');
    applyBrowserCompanionState({
      kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, jar: 'worktree', recentGen: 'L.1',
      pages: [page('a', 'https://example.test/moved', 'Page a'), PAGES[1]],
    } as never);
    await tick();
    expect(input.value).toBe('https://draft.test/');
    key(input, { key: 'Escape' });
    await tick();
    expect(input.value).toBe('https://example.test/moved');
  });

  it('does not leave the field after a tab switch once another control was pressed', async () => {
    let finish: () => void = () => {};
    const { view, input, onAct, onLeave } = mount();
    onAct.mockImplementation(() => new Promise<void>((resolve) => (finish = resolve)));
    input.focus();
    await type(input, 'settings');
    await waitFor(() => expect(rows().length).toBe(1));
    key(input, { key: 'ArrowDown' });
    key(input, { key: 'Enter' });
    // A press on the toolbar, which leaves focus in the field.
    await fireEvent.pointerDown(view.getByTestId('browser-close'));
    finish();
    await Promise.resolve();
    await tick();
    expect(onLeave).not.toHaveBeenCalled();
  });

  it('Enter with no row highlighted navigates the typed text', async () => {
    const { input, onAct } = mount();
    input.focus();
    await type(input, 'local');
    await waitFor(() => expect(rows().length).toBe(2));
    key(input, { key: 'Enter' });
    expect(onAct).toHaveBeenCalledWith({ kind: 'navigate', pageId: 'a', address: 'local' });
  });

  it('Escape closes the list first and reverts on the next press', async () => {
    const { input, onLeave } = mount();
    input.focus();
    await type(input, 'local');
    await waitFor(() => expect(rows().length).toBe(2));
    const first = key(input, { key: 'Escape' });
    await tick();
    expect(first.defaultPrevented).toBe(true);
    expect(rows()).toEqual([]);
    expect(input.value).toBe('local');
    expect(onLeave).not.toHaveBeenCalled();
    const second = key(input, { key: 'Escape' });
    await tick();
    expect(second.defaultPrevented).toBe(true);
    expect(input.value).toBe('https://example.test/a');
    expect(onLeave).toHaveBeenCalledTimes(1);
  });

  it('takes a clicked row without moving focus off the field first', async () => {
    const { input, onAct } = mount();
    input.focus();
    await type(input, 'github');
    await waitFor(() => expect(rows().length).toBe(1));
    const press = new MouseEvent('mousedown', { bubbles: true, cancelable: true });
    rows()[0].dispatchEvent(press);
    expect(press.defaultPrevented).toBe(true);
    await fireEvent.mouseMove(rows()[0]);
    expect(rows()[0].getAttribute('aria-selected')).toBe('true');
    await fireEvent.click(rows()[0]);
    expect(onAct).toHaveBeenCalledWith({ kind: 'navigate', pageId: 'a', address: 'https://github.test/org/repo' });
  });

  // The field keeps the keyboard while the list is open, so the list is an
  // airspace layer the native view yields to, never an open overlay that
  // would turn the browser pane's commands off.
  it('paints as a layer while open, never as an open overlay', async () => {
    const { input } = mount();
    input.focus();
    await type(input, 'local');
    await waitFor(() => expect(rows().length).toBe(2));
    expect(airspaceSurfaces()).toEqual([]);
    expect(makeCommandContext(null, {}).overlayOpen).toBe(false);
    input.blur();
    await tick();
    expect(rows()).toEqual([]);
  });

  it('closes when another tab becomes active', async () => {
    const { view, input } = mount();
    input.focus();
    await type(input, 'local');
    await waitFor(() => expect(rows().length).toBe(2));
    seed('b');
    await view.rerender({ activePage: PAGES[1] as never });
    await tick();
    expect(rows()).toEqual([]);
  });

  it('says when the recent addresses could not be read', async () => {
    setBindingMock('BrowserCompanionRecentAddresses', async () => {
      throw new Error('browser manager unavailable');
    });
    const { view, input } = mount();
    input.focus();
    await tick();
    await type(input, 'local');
    expect((await view.findByTestId('browser-address-suggestions-error')).textContent).toContain('browser manager unavailable');
  });

  it('keeps Enter on the highlighted row when the list changes under it', async () => {
    const { input, onAct } = mount();
    input.focus();
    await type(input, 'github');
    await waitFor(() => expect(rowText()).toEqual(['recent https://github.test/org/repo']));
    key(input, { key: 'ArrowDown' });
    await tick();
    // A tab push puts a row above the highlighted one.
    applyBrowserCompanionState({
      kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, jar: 'worktree', recentGen: 'L.1',
      pages: [...PAGES, page('g', 'https://github.test/other', 'Other')],
    } as never);
    await tick();
    expect(rowText()).toEqual(['tab https://github.test/other', 'recent https://github.test/org/repo']);
    expect(rows().map((row) => row.getAttribute('aria-selected'))).toEqual(['false', 'true']);
    expect(input.getAttribute('aria-activedescendant')).toBe(rows()[1].id);
    key(input, { key: 'Enter' });
    expect(onAct).toHaveBeenLastCalledWith({ kind: 'navigate', pageId: 'a', address: 'https://github.test/org/repo' });
  });

  it('goes back to the typed text when the highlighted row leaves the list', async () => {
    const other = page('d', 'https://d.test/settings', 'More settings');
    applyBrowserCompanionState({ kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, jar: 'worktree', recentGen: 'L.1', pages: [...PAGES, other] } as never);
    const { input, onAct } = mount();
    input.focus();
    await type(input, 'settings');
    await waitFor(() => expect(rows().length).toBe(2));
    key(input, { key: 'ArrowDown' });
    await tick();
    expect(rows().map((row) => row.getAttribute('aria-selected'))).toEqual(['true', 'false']);
    // The highlighted tab closes, and the other slides into its place.
    applyBrowserCompanionState({ kind: 'state', threadId: THREAD, activePageId: 'a', visible: true, jar: 'worktree', recentGen: 'L.1', pages: [PAGES[0], other] } as never);
    await tick();
    expect(rowText()).toEqual(['tab https://d.test/settings']);
    expect(rows()[0].getAttribute('aria-selected')).toBe('false');
    key(input, { key: 'Enter' });
    expect(onAct).toHaveBeenLastCalledWith({ kind: 'navigate', pageId: 'a', address: 'settings' });
  });

  it('reads again for the new thread when the pane changes thread under the focused field', async () => {
    recent.mockImplementation(async (thread: string) =>
      thread === THREAD ? [{ url: 'https://github.test/org/repo', title: 'Repo' }] : [{ url: 'https://two.test/', title: 'Two' }]);
    applyBrowserCompanionState({ kind: 'state', threadId: 'thread-2', activePageId: 'c', visible: true, jar: 'worktree', pages: [page('c', 'https://c.test/')] } as never);
    await attachBrowserPaneForTest('thread-2', 'mount-2');
    const { view, input } = mount();
    input.focus();
    await waitFor(() => expect(recent).toHaveBeenCalledWith(THREAD));
    await view.rerender({ threadId: 'thread-2', activePage: page('c', 'https://c.test/') as never });
    await waitFor(() => expect(recent).toHaveBeenLastCalledWith('thread-2'));
    expect(document.activeElement).toBe(input);
    await type(input, 'test');
    await waitFor(() => expect(rowText()).toEqual(['recent https://two.test/']));
  });

  it('drops the recent addresses, and a read in flight, when the Manager forgets them', async () => {
    const answers: ((list: { url: string; title: string }[]) => void)[] = [];
    recent.mockImplementation(() => new Promise((resolve) => answers.push(resolve)));
    const { input } = mount();
    input.focus();
    await waitFor(() => expect(answers.length).toBe(1));
    push('thread', 'L.2');
    await waitFor(() => expect(answers.length).toBe(2));
    // The old jar's read answers last, with what the Manager has forgotten.
    answers[1]([]);
    answers[0]([{ url: 'https://github.test/org/repo', title: 'Repo' }]);
    await tick();
    await type(input, 'github');
    expect(rows()).toEqual([]);

    // A read the field left in flight is the old jar's even with no focus
    // to read again for.
    answers.length = 0;
    input.blur();
    input.focus();
    await waitFor(() => expect(answers.length).toBe(1));
    input.blur();
    push('project', 'L.3');
    await tick();
    answers[0]([{ url: 'https://github.test/org/repo', title: 'Repo' }]);
    await tick();
    input.focus();
    await type(input, 'github');
    expect(rows()).toEqual([]);

    // A list already read goes with the jar too, focus or not.
    answers.length = 0;
    input.blur();
    input.focus();
    await waitFor(() => expect(answers.length).toBe(1));
    answers[0]([{ url: 'https://github.test/org/repo', title: 'Repo' }]);
    await type(input, 'github');
    await waitFor(() => expect(rows().length).toBe(1));
    input.blur();
    push('private', 'L.4');
    await tick();
    expect(answers.length).toBe(1);
    // A focus reads again (pending here); until it answers nothing is offered.
    input.focus();
    await type(input, 'github');
    expect(rows()).toEqual([]);
  });

  it('drops them after a jar round trip that ends on the same jar in one batch', async () => {
    const answers: ((list: { url: string; title: string }[]) => void)[] = [];
    recent.mockImplementation(() => new Promise((resolve) => answers.push(resolve)));
    const { input } = mount();
    input.focus();
    await waitFor(() => expect(answers.length).toBe(1));
    answers[0]([{ url: 'https://github.test/org/repo', title: 'Repo' }]);
    await type(input, 'github');
    await waitFor(() => expect(rows().length).toBe(1));
    // A read in flight when the batch lands.
    input.blur();
    input.focus();
    await waitFor(() => expect(answers.length).toBe(2));
    // worktree -> private -> worktree, applied together as a socket batch
    // is, with the same tab: only the generation says the list was forgotten.
    push('private', 'L.2');
    push('worktree', 'L.3');
    await waitFor(() => expect(answers.length).toBe(3));
    answers[1]([{ url: 'https://github.test/org/repo', title: 'Repo' }]);
    await tick();
    await type(input, 'github');
    expect(rows()).toEqual([]);
    answers[2]([]);
    await tick();
    await type(input, 'github');
    expect(rows()).toEqual([]);
  });

  it('keeps the newest focus\'s read when an older one answers late', async () => {
    const answers: ((list: { url: string; title: string }[]) => void)[] = [];
    setBindingMock('BrowserCompanionRecentAddresses', () => new Promise((resolve) => answers.push(resolve)));
    const { input } = mount();
    input.focus();
    input.blur();
    input.focus();
    await waitFor(() => expect(answers.length).toBe(2));
    answers[1]([{ url: 'https://new.test/', title: '' }]);
    await tick();
    answers[0]([{ url: 'https://old.test/', title: '' }]);
    await tick();
    await type(input, '.test');
    await waitFor(() => expect(rowText()).toContain('recent https://new.test/'));
    expect(rowText()).not.toContain('recent https://old.test/');
  });

  // Two computers claim the thread at one ownership epoch: this window may
  // not host its browser, so its pane attachment is no longer one to read
  // from. A focused field then reads nothing, and an answer that was on its
  // way shows nothing.
  describe('once two computers claim the thread', () => {
    afterEach(() => {
      forgetThread(THREAD);
      forgetBackendEntities('remote');
      resetStagedBackends();
    });

    it('reads no recent address and shows none', async () => {
      stageBackend({ id: 'remote', backendId: 'remote' });
      noteThread(THREAD, HOME_BACKEND, 4);
      noteThread(THREAD, 'remote', 4);
      const { input } = mount();
      input.focus();
      await type(input, 'repo');
      await tick();
      expect(recent).not.toHaveBeenCalled();
      expect(rowText()).toEqual([]);
    });

    it('offers none of the home computer\'s open tabs', async () => {
      stageBackend({ id: 'remote', backendId: 'remote' });
      noteThread(THREAD, HOME_BACKEND, 4);
      const { input } = mount();
      input.focus();
      await type(input, 'local');
      await waitFor(() => expect(rowText()).toContain('tab http://localhost:5173/settings'));
      noteThread(THREAD, 'remote', 4);
      await type(input, 'locall');
      await type(input, 'local');
      expect(rowText().filter((row) => row.startsWith('tab '))).toEqual([]);
    });

    it('drops an answer that arrives after the claim', async () => {
      stageBackend({ id: 'remote', backendId: 'remote' });
      noteThread(THREAD, HOME_BACKEND, 4);
      let answer: (list: { url: string; title: string }[]) => void = () => {};
      recent.mockImplementation(() => new Promise((resolve) => { answer = resolve; }));
      const { input } = mount();
      input.focus();
      await waitFor(() => expect(recent).toHaveBeenCalledTimes(1));
      noteThread(THREAD, 'remote', 4);
      answer([{ url: 'https://github.test/org/repo', title: 'Repo' }]);
      await type(input, 'repo');
      await tick();
      expect(rowText()).toEqual([]);
    });
  });
});
