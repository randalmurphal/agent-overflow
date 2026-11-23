// The Thread actions menu on desktop: the terminal rows, find in thread,
// title regeneration and the editor rows. Its compact-only rows are covered in
// ChatHeaderActions.test.ts, and the spinner a regeneration shows beside
// the title in ChatHeader.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatHeaderMenu from './ChatHeaderMenu.svelte';
import { ensureEditorsLoaded, getResolvedEditor, resetEditorsForTest, setEditorPreference } from '../../stores/editors.svelte';
import { resetKeybindingsStore, setKeybindingsForTest } from '../../stores/keybindings.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { isMessageSearchOpen } from '../../stores/messageSearch.svelte';
import {
  closePaneFind,
  getPaneFind,
  registerPaneFindProvider,
  resetPaneFindForTest,
  takePaneFindReturnFocus,
  type PaneFindProvider,
} from '../../stores/paneFind.svelte';
import { focusPane, getFocusedPaneId, resetPanesForTest } from '../../stores/panes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { addProjectLocal, resetProjectsForTest } from '../../stores/projects.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { openTerminalThread } from '../../stores/threadCreation.svelte';
import {
  applyThreadTitleGeneration,
  resetThreadTitleGenerationForTest,
} from '../../stores/threadTitleGeneration.svelte';
import type { EditorInfo } from '../../stores/bindings';
import type { Thread } from '../../types/models';
import { buildPane, makeThread as makeBaseThread } from '../../../test/helpers/chat';
import { OBSERVE_SCOPES, pairViewOnly, pairWithScopes, resetToLocalPage } from '../../../test/helpers/scopes';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

vi.mock('../../stores/threadCreation.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/threadCreation.svelte')>()),
  openTerminalThread: vi.fn(),
}));

// Popover/Menu transitions poke Element.animate on mount; happy-dom lacks it.
if (typeof Element !== 'undefined' && !('animate' in Element.prototype)) {
  (Element.prototype as unknown as { animate: unknown }).animate = function () {
    return {
      cancel() {}, finish() {}, play() {}, pause() {}, reverse() {},
      addEventListener() {}, removeEventListener() {},
      onfinish: null, oncancel: null, finished: Promise.resolve(),
      effect: null, startTime: 0, currentTime: 0, playState: 'finished', playbackRate: 1,
    };
  };
}

const INERT = 'Not granted to this device';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({ projectId: 'project-1', workspacePath: '/tmp/ws', projectPath: '/tmp/proj', ...overrides });
}

function addProject(): void {
  addProjectLocal({
    id: 'project-1',
    path: '/tmp/proj',
    name: 'Alpha',
    sortPosition: 0,
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  });
}

function ed(id: string, name: string): EditorInfo {
  return { id, name, available: true, envFallback: false } as EditorInfo;
}

// Seed the shared catalog before mount so the rows are settled on first
// render (the menu's own effect loads it too).
async function seedEditors(editors: EditorInfo[], preference = ''): Promise<void> {
  setBindingMock('ListAvailableEditors', async () => editors);
  setBindingMock('GetEditorSettings', async () => ({ preference }));
  await ensureEditorsLoaded();
}

async function flush(n = 6): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

// The find ChatView registers for its pane's timeline (ThreadPaneFind).
function findProvider(): PaneFindProvider {
  return { noun: 'thread', result: { count: 0, current: -1, pending: false }, search: vi.fn(), clear: vi.fn() };
}

async function renderOpen(thread: Thread = makeThread(), { find = false } = {}) {
  const pane = await buildPane(thread);
  if (find) registerPaneFindProvider(pane.paneId, findProvider());
  const view = render(ChatHeaderMenu, { props: { pane } });
  await flush();
  await fireEvent.click(view.getByTestId('chat-header-more'));
  await flush();
  return { pane, ...view };
}

async function reopen(view: { getByTestId: (id: string) => HTMLElement }): Promise<void> {
  await fireEvent.click(view.getByTestId('chat-header-more'));
  await flush();
}

// A row that toggles something is a menuitemcheckbox; the rest are menuitems.
const rows = (): string[] =>
  [...document.body.querySelectorAll('[data-menuitem]')].map((row) => row.textContent?.trim() ?? '');
const queryRow = (name: string | RegExp): HTMLElement | null =>
  within(document.body).queryByRole('menuitem', { name })
    ?? within(document.body).queryByRole('menuitemcheckbox', { name });
const row = (name: string | RegExp): HTMLElement => {
  const found = queryRow(name);
  if (!found) throw new Error(`no menu row named ${String(name)}`);
  return found;
};

describe('<ChatHeaderMenu> on desktop', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetProjectsForTest();
    resetEditorsForTest();
    resetThreadTitleGenerationForTest();
    resetPaneFindForTest();
    setBindingMock('ListAvailableEditors', async () => []);
    setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
    vi.mocked(openTerminalThread).mockClear();
  });

  afterEach(() => {
    setCompactLayoutForTest(false);
    resetKeybindingsStore();
    resetPaneFindForTest();
    resetPaneLayoutForTest();
    resetToLocalPage();
  });

  it('lists the terminal rows, find, title regeneration and the editor, and none of the compact rows', async () => {
    addProject();
    await renderOpen(makeThread(), { find: true });
    expect(rows()).toEqual(['Terminal', 'New terminal pane', 'Find in thread', 'Regenerate title', 'Open in editor']);
  });

  it('Find in thread opens this pane\'s find bar, not the search across every thread', async () => {
    const view = await renderOpen(makeThread(), { find: true });
    const { pane } = view;
    // The row acts on its own pane, not on the focused one as the chord
    // does: another pane holds focus here, and its find stays shut.
    setPaneLayoutItemsForTest([
      { id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 600 },
      { id: 'other-pane', paneId: 'other-pane', kind: 'thread', widthPx: 600 },
    ]);
    registerPaneFindProvider('other-pane', findProvider());
    focusPane('other-pane');
    expect(getFocusedPaneId()).toBe('other-pane');
    await fireEvent.click(row('Find in thread'));
    await flush();
    expect(getPaneFind(pane.paneId)?.open).toBe(true);
    expect(getPaneFind('other-pane')).toBeNull();
    expect(isMessageSearchOpen()).toBe(false);
    expect(queryRow('Find in thread')).toBeNull();
    // Closing the bar hands focus back to the menu button it was opened from.
    expect(takePaneFindReturnFocus(pane.paneId)).toBe(view.getByTestId('chat-header-more'));
  });

  it('seeds the find with the composer selection held when the button was pressed, and never a stale one', async () => {
    const pane = await buildPane(makeThread());
    registerPaneFindProvider(pane.paneId, findProvider());
    const view = render(ChatHeaderMenu, { props: { pane } });
    const section = document.createElement('section');
    section.dataset.paneId = pane.paneId;
    const composer = document.createElement('textarea');
    composer.value = 'find the needle here';
    section.append(composer);
    document.body.append(section);
    try {
      await flush();
      const button = view.getByTestId('chat-header-more');
      const pressDown = async (): Promise<void> => {
        composer.focus();
        composer.setSelectionRange(9, 15);
        await fireEvent.pointerDown(button);
        // The press moves focus to the button before any click lands.
        button.focus();
      };
      // A pointer's click counts its presses; a keyboard activation is a
      // click with none.
      const pointerClick = async (): Promise<void> => {
        await fireEvent.click(button, { detail: 1 });
        await flush();
      };
      const keyboardClick = async (): Promise<void> => {
        await fireEvent.click(button, { detail: 0 });
        await flush();
      };
      const findQuery = async (): Promise<string | undefined> => {
        await fireEvent.click(row('Find in thread'));
        await flush();
        const query = getPaneFind(pane.paneId)?.query;
        closePaneFind(pane.paneId);
        return query;
      };

      // A press whose menu closes without a pick leaves nothing behind: the
      // next open, from the keyboard, reads the selection as it is then.
      await pressDown();
      await pointerClick();
      await fireEvent.keyDown(row('Find in thread'), { key: 'Escape' });
      await flush();
      await keyboardClick();
      expect(await findQuery()).toBe('');

      // Nor does a press dragged off the button, which never became a click.
      await pressDown();
      await keyboardClick();
      expect(await findQuery()).toBe('');

      await pressDown();
      await pointerClick();
      expect(await findQuery()).toBe('needle');
    } finally {
      section.remove();
    }
  });

  it('has no Find in thread row for a pane with no find, and gains it when the find registers', async () => {
    const view = await renderOpen();
    expect(queryRow('Find in thread')).toBeNull();
    const release = registerPaneFindProvider(view.pane.paneId, findProvider());
    await flush();
    expect(row('Find in thread')).toBeInTheDocument();
    release();
    await flush();
    expect(queryRow('Find in thread')).toBeNull();
  });

  it('keeps the search across every thread off the desktop menu and on the compact one', async () => {
    const view = await renderOpen(makeThread(), { find: true });
    expect(queryRow('Search messages')).toBeNull();
    view.unmount();

    setCompactLayoutForTest(true);
    await renderOpen(makeThread(), { find: true });
    const compactRows = rows();
    expect(compactRows.slice(compactRows.indexOf('Terminal'))).toEqual([
      'Terminal',
      'New terminal pane',
      'Find in thread',
      'Search messages',
      'Regenerate title',
    ]);
  });

  it('is a menu button that says whether its menu is open', async () => {
    const pane = await buildPane(makeThread());
    const view = render(ChatHeaderMenu, { props: { pane } });
    await flush();
    const button = view.getByTestId('chat-header-more');
    expect(button).toHaveAccessibleName('Thread actions');
    expect(button).toHaveAttribute('aria-haspopup', 'menu');
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(button).not.toHaveAttribute('aria-pressed');
    await fireEvent.click(button);
    await flush();
    expect(button).toHaveAttribute('aria-expanded', 'true');
    expect(within(document.body).getByRole('menu', { name: 'Thread actions' })).toBeInTheDocument();
    await fireEvent.click(button);
    await flush();
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(within(document.body).queryByRole('menu', { name: 'Thread actions' })).toBeNull();
  });

  it('closes on Escape and on a pick, and both hand focus back to its button', async () => {
    setBindingMock('RegenerateThreadTitle', async () => undefined);
    const view = await renderOpen();
    const button = view.getByTestId('chat-header-more');
    await fireEvent.keyDown(row('Terminal'), { key: 'Escape' });
    await flush();
    expect(queryRow('Terminal')).toBeNull();
    expect(document.activeElement).toBe(button);

    // A pick whose action takes no focus of its own must not leave the
    // keyboard on <body> when the focused row unmounts.
    button.blur();
    await reopen(view);
    await fireEvent.keyDown(row('Regenerate title'), { key: 'Enter' });
    await flush();
    expect(queryRow('Regenerate title')).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it('Terminal opens the drawer with focus latched, marks it open, and closes it again', async () => {
    const view = await renderOpen();
    const { pane } = view;
    // The row says whether the drawer is open, as the button it replaced did.
    expect(row('Terminal').getAttribute('role')).toBe('menuitemcheckbox');
    expect(row('Terminal').getAttribute('aria-checked')).toBe('false');
    await fireEvent.click(row('Terminal'));
    await flush();
    expect(pane.showTerminal).toBe(true);
    // The row opens AND focuses, identical to the terminal.toggle chord: it
    // routes through runTerminalToggle, which latches the focus intent the
    // drawer consumes on mount (read-and-clear, so set exactly once).
    expect(pane.consumeTerminalFocusRequest()?.current).toBe(true);

    await reopen(view);
    expect(row(/^Terminal/).textContent).toContain('✓');
    expect(row(/^Terminal/).getAttribute('aria-checked')).toBe('true');
    await fireEvent.click(row(/^Terminal/));
    await flush();
    expect(pane.showTerminal).toBe(false);
    await reopen(view);
    expect(row('Terminal').getAttribute('aria-checked')).toBe('false');
    expect(vi.mocked(openTerminalThread)).not.toHaveBeenCalled();
  });

  it('New terminal pane opens a fresh terminal rooted at the thread workspace and leaves the drawer shut', async () => {
    const { pane } = await renderOpen(makeThread({ projectId: 'proj-7', workspacePath: '/tmp/ws7' }));
    await fireEvent.click(row('New terminal pane'));
    await flush();
    expect(vi.mocked(openTerminalThread)).toHaveBeenCalledWith({ projectId: 'proj-7', cwd: '/tmp/ws7' });
    expect(pane.showTerminal).toBe(false);
  });

  it('names the chord of each row that has one from the loaded keybindings, and none under compact', async () => {
    // Chord hints come from the loaded keybindings store: there is no
    // hardcoded fallback, which would resurrect a chord the user rebound
    // or cleared.
    setKeybindingsForTest([
      { key: 'ctrl+`', command: 'terminal.toggle' },
      { key: 'ctrl+shift+n', command: 'terminal.newPane' },
      { key: 'ctrl+f', command: 'find.in-pane' },
      { key: 'ctrl+shift+f', command: 'search.messages' },
    ]);
    const view = await renderOpen(makeThread(), { find: true });
    expect(row('Terminal').textContent).toContain('Ctrl+`');
    expect(row('New terminal pane').textContent).toContain('Ctrl+Shift+N');
    expect(row('Find in thread').textContent).toContain('Ctrl+F');
    expect(row('Find in thread').textContent).not.toContain('Shift');
    // A row whose command is unbound names no chord.
    expect(row('Regenerate title').textContent?.trim()).toBe('Regenerate title');
    view.unmount();

    setCompactLayoutForTest(true);
    await renderOpen(makeThread(), { find: true });
    for (const name of ['Terminal', 'New terminal pane', 'Find in thread', 'Search messages']) {
      expect(row(name).textContent?.trim(), name).toBe(name);
    }
  });

  it('renders both terminal rows inert with the reason without terminal:operate', async () => {
    await pairWithScopes([...OBSERVE_SCOPES, 'threads:operate']);
    const { pane } = await renderOpen();
    for (const name of ['Terminal', 'New terminal pane']) {
      expect(row(name).getAttribute('aria-disabled'), name).toBe('true');
      expect(row(name).getAttribute('title'), name).toBe(INERT);
    }
    await fireEvent.click(row('Terminal'));
    await fireEvent.click(row('New terminal pane'));
    await flush();
    expect(pane.showTerminal).toBe(false);
    expect(vi.mocked(openTerminalThread)).not.toHaveBeenCalled();
  });

  it('Regenerate title starts the run and stays inert until its completion event', async () => {
    const regenerate = setBindingMock('RegenerateThreadTitle', async () => undefined);
    const view = await renderOpen();
    await fireEvent.click(row('Regenerate title'));
    await vi.waitFor(() => expect(regenerate).toHaveBeenCalledWith('thread-1'));

    // The ack resolved, but the run goes on server-side: only the
    // completion event releases the row.
    await reopen(view);
    expect(row('Regenerate title').getAttribute('aria-disabled')).toBe('true');
    await fireEvent.click(row('Regenerate title'));
    await flush();
    expect(regenerate).toHaveBeenCalledTimes(1);

    applyThreadTitleGeneration({ threadId: 'thread-1', error: '' });
    await flush();
    expect(row('Regenerate title').getAttribute('aria-disabled')).toBeNull();
  });

  it('offers New terminal pane on the phone only where the sidebar would list the terminal', async () => {
    // A thread outside every project (an assistant's): its terminal would
    // have no sidebar row, the phone's only way back to a pane.
    const desktop = await renderOpen(makeThread({ projectId: undefined }));
    expect(queryRow('New terminal pane')).not.toBeNull();
    desktop.unmount();

    setCompactLayoutForTest(true);
    const phone = await renderOpen(makeThread({ projectId: undefined }));
    expect(queryRow('Terminal')).not.toBeNull();
    expect(queryRow('New terminal pane')).toBeNull();
    phone.unmount();

    await renderOpen(makeThread());
    expect(queryRow('New terminal pane')).not.toBeNull();
  });

  it('has no Regenerate title row on a draft, and gains it once the thread is persisted', async () => {
    const project = {
      id: 'proj-1', path: '/tmp/proj', name: 'Alpha', sortPosition: 0, createdAt: 0, updatedAt: 0, archived: false,
    };
    const pane = createThreadPane({ paneId: 'draft-menu' });
    pane.startDraftPlaceholder(project, 'chat');
    expect(pane.threadId).toBeNull();
    const view = render(ChatHeaderMenu, { props: { pane } });
    await flush();
    await fireEvent.click(view.getByTestId('chat-header-more'));
    await flush();
    // A draft has no conversation to title from; its other rows are there.
    expect(queryRow('Regenerate title')).toBeNull();
    expect(queryRow('Terminal')).not.toBeNull();

    pane.adoptMaterializedDraftThread(makeThread());
    await flush();
    expect(pane.threadId).toBe('thread-1');
    expect(row('Regenerate title').getAttribute('aria-disabled')).toBeNull();
  });

  it('reads the selection on a press only for a pane with a find to seed', async () => {
    const pane = await buildPane(makeThread());
    const view = render(ChatHeaderMenu, { props: { pane } });
    await flush();
    const button = view.getByTestId('chat-header-more');
    // The pane's element, where a selection would be read from.
    setPaneLayoutItemsForTest([{ id: pane.paneId, paneId: pane.paneId, kind: 'thread', widthPx: 600 }]);
    const section = document.createElement('section');
    section.dataset.paneId = pane.paneId;
    document.body.append(section);
    const selection = vi.spyOn(window, 'getSelection');
    try {
      await fireEvent.pointerDown(button);
      expect(selection).not.toHaveBeenCalled();

      registerPaneFindProvider(pane.paneId, findProvider());
      await flush();
      await fireEvent.pointerDown(button);
      expect(selection).toHaveBeenCalledTimes(1);

      // The press that closes the open menu seeds nothing.
      await fireEvent.click(button);
      await flush();
      expect(queryRow('Find in thread')).not.toBeNull();
      await fireEvent.pointerDown(button);
      expect(selection).toHaveBeenCalledTimes(1);
    } finally {
      selection.mockRestore();
      section.remove();
    }
  });

  it('renders Regenerate title inert with the reason in a view-only session', async () => {
    await pairViewOnly();
    const regenerate = setBindingMock('RegenerateThreadTitle', async () => undefined);
    await renderOpen();
    expect(row('Regenerate title').getAttribute('aria-disabled')).toBe('true');
    expect(row('Regenerate title').getAttribute('title')).toBe(INERT);
    await fireEvent.click(row('Regenerate title'));
    await flush();
    expect(regenerate).not.toHaveBeenCalled();
  });

  it('names the one installed editor and opens the project root in it', async () => {
    addProject();
    await seedEditors([ed('code', 'Visual Studio Code')]);
    const open = setBindingMock('OpenInEditor', async () => undefined);
    await renderOpen();
    await fireEvent.click(row('Open in Visual Studio Code'));
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    // The row names an editor, so it opens that one, whatever the host's
    // default is by the time the call lands.
    expect(open.mock.calls[0]).toEqual(['/tmp/proj', 0, 0, '', 'code']);
  });

  it('leaves the open menu\'s editor row alone when the catalog arrives, and lists the editors on the next open', async () => {
    addProject();
    let deliver: (editors: EditorInfo[]) => void = () => {};
    setBindingMock('ListAvailableEditors', () => new Promise<EditorInfo[]>((resolve) => { deliver = resolve; }));
    const open = setBindingMock('OpenInEditor', async () => undefined);
    const view = await renderOpen();
    const before = row('Open in editor');
    before.focus();

    deliver([ed('code', 'Visual Studio Code'), ed('cursor', 'Cursor')]);
    await ensureEditorsLoaded();
    await flush();
    // The row the keyboard is on is still there, unnamed, and still opens
    // the default.
    expect(row('Open in editor')).toBe(before);
    expect(document.activeElement).toBe(before);
    expect(queryRow('Open in Cursor')).toBeNull();
    await fireEvent.click(before);
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    expect(open.mock.calls[0]).toEqual(['/tmp/proj', 0, 0, '', '']);

    await reopen(view);
    expect(rows().slice(-2)).toEqual(['Open in Visual Studio Code', 'Open in Cursor']);
  });

  it('keeps each editor row what it was when the default changes under the open menu', async () => {
    addProject();
    await seedEditors([ed('code', 'Visual Studio Code'), ed('cursor', 'Cursor')]);
    const open = setBindingMock('OpenInEditor', async () => undefined);
    setBindingMock('SetEditorSettings', async () => ({ preference: 'cursor' }));
    const view = await renderOpen();
    const code = row('Open in Visual Studio Code');
    const cursor = row('Open in Cursor');
    cursor.focus();

    // Another surface makes Cursor the default while the menu is open.
    await setEditorPreference('cursor');
    await flush();
    expect(getResolvedEditor()?.id).toBe('cursor');
    expect(row('Open in Cursor')).toBe(cursor);
    expect(document.activeElement).toBe(cursor);
    expect(row('Open in Visual Studio Code')).toBe(code);
    await fireEvent.click(code);
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    expect(open.mock.calls[0]).toEqual(['/tmp/proj', 0, 0, '', 'code']);

    await reopen(view);
    expect(rows().slice(-2)).toEqual(['Open in Cursor', 'Open in Visual Studio Code']);
  });

  it('lists every installed editor, the default first, and opens a picked one by id', async () => {
    addProject();
    // The preference makes Cursor the default, ahead of catalog order.
    await seedEditors([ed('code', 'Visual Studio Code'), ed('cursor', 'Cursor'), ed('zed', 'Zed')], 'cursor');
    const open = setBindingMock('OpenInEditor', async () => undefined);
    await renderOpen();
    expect(rows().slice(3)).toEqual(['Open in Cursor', 'Open in Visual Studio Code', 'Open in Zed']);
    await fireEvent.click(row('Open in Zed'));
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    expect(open.mock.calls[0]).toEqual(['/tmp/proj', 0, 0, '', 'zed']);
  });

  it('keeps a working editor row when none is installed', async () => {
    addProject();
    await seedEditors([]);
    const open = setBindingMock('OpenInEditor', async () => undefined);
    await renderOpen();
    // The click still reaches the backend, which surfaces the real "no
    // editor available" error.
    await fireEvent.click(row('Open in editor'));
    await vi.waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    expect(open.mock.calls[0]).toEqual(['/tmp/proj', 0, 0, '', '']);
  });

  it('has no editor row for a thread outside every project', async () => {
    const list = setBindingMock('ListAvailableEditors', async () => []);
    await renderOpen(makeThread({ projectId: undefined }));
    expect(queryRow(/^Open in/)).toBeNull();
    expect(list).not.toHaveBeenCalled();
  });

  it('has no editor row and makes no editor RPC away from the host', async () => {
    addProject();
    await pairViewOnly();
    const list = setBindingMock('ListAvailableEditors', async () => []);
    const settings = setBindingMock('GetEditorSettings', async () => ({ preference: '' }));
    await renderOpen();
    expect(queryRow(/^Open in/)).toBeNull();
    expect(list).not.toHaveBeenCalled();
    expect(settings).not.toHaveBeenCalled();
  });
});
