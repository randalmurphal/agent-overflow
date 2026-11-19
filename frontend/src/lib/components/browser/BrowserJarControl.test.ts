import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserJarControl from './BrowserJarControl.svelte';
import BrowserJarControlHarness from './BrowserJarControlHarness.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import {
  applyBrowserCompanionState,
  attachBrowserCompanion,
  browserActionError,
  resetBrowserCompanionForTest,
} from '../../stores/browserCompanion.svelte';
import { TransportError } from '../../transport/wsClient';
import { setHostWindowFromBootstrap } from '../../transport/pageHost';

const THREAD = 'thread-1';

function page(id: string, extra: Record<string, unknown> = {}) {
  return { id, url: `https://example.test/${id}`, title: id, canGoBack: false, canGoForward: false, ...extra };
}

function seed(jar: string, pages: ReturnType<typeof page>[]) {
  applyBrowserCompanionState({ kind: 'state', threadId: THREAD, activePageId: pages[0]?.id ?? '', visible: true, jar, pages } as never);
}

function mount() {
  return render(BrowserJarControlHarness, { threadId: THREAD });
}

// The pane's banner is the attached pane's: attach one, as a mounted pane does.
async function attachPane(): Promise<() => void> {
  setBindingMock('BrowserCompanionPaneAttach', vi.fn(async () => ({
    id: 'mount-1', state: { kind: 'state', threadId: THREAD, jar: 'worktree', pages: [page('a')] },
  })));
  setBindingMock('BrowserCompanionPaneDetach', vi.fn(async () => {}));
  const pane = attachBrowserCompanion(THREAD);
  await vi.waitFor(() => expect(pane.current).not.toBeNull());
  return () => pane.release();
}

describe('<BrowserJarControl>', () => {
  // The backend's answer: an unconfirmed switch is refused while a tab is
  // live, as the Manager decides; `live` stands for its tabs.
  let live = true;
  let act: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    resetBrowserCompanionForTest();
    live = true;
    act = setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: { jar?: string; confirmed?: boolean }) => {
      if (live && !action.confirmed) {
        throw new TransportError('browser_jar_unconfirmed', 'Switching the cookie jar closes the open tabs and loses their page state');
      }
      return { kind: 'state', threadId: THREAD, jar: action.jar, pages: [] };
    }));
  });
  afterEach(() => resetBrowserCompanionForTest());

  it('shows the current jar and lists the four with what each shares', async () => {
    seed('project', [page('a')]);
    const view = mount();
    const button = view.getByTestId('browser-jar');
    expect(button.getAttribute('aria-label')).toBe('Cookie jar: Whole project');
    await fireEvent.click(button);
    const menu = await view.findByRole('menu', { name: 'Cookie jar' });
    const rows = within(menu).getAllByRole('menuitemcheckbox');
    expect(rows.map((row) => row.textContent?.replace('✓', '').replace(/\s+/g, ' ').trim())).toEqual([
      'This worktree Shared with every thread on this worktree',
      "Whole project Shared with this project's threads that choose it, in any worktree",
      "This browser only This thread's own, kept between sessions",
      'Private Not kept; discarded when its last tab closes',
    ]);
    expect(rows.map((row) => row.getAttribute('aria-checked'))).toEqual(['false', 'true', 'false', 'false']);
  });

  it('asks when the backend refuses an unconfirmed switch, and switches only when confirmed', async () => {
    const release = await attachPane();
    seed('worktree', [page('a'), page('b'), page('c', { suspended: true })]);
    const view = mount();
    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-thread'));
    const dialog = await view.findByRole('dialog');
    expect(act).toHaveBeenCalledTimes(1);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'jar', jar: 'thread' });
    expect(act.mock.calls[0][1].confirmed).toBeFalsy();
    expect(dialog.textContent).toContain('Switch to This browser only?');
    expect(dialog.textContent).toContain('The 2 open tabs close and load their addresses again in the new cookie jar when you go back to them.');
    expect(dialog.textContent).toContain('form input, back and forward history, and sign-ins kept in the current jar');
    // The refusal is the question, not a failure for the banner.
    expect(browserActionError(THREAD)).toBe('');

    await fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await tick();
    expect(view.queryByRole('dialog')).toBeNull();
    expect(act).toHaveBeenCalledTimes(1);

    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-thread'));
    await fireEvent.click(within(await view.findByRole('dialog')).getByRole('button', { name: 'Switch cookie jar' }));
    await waitFor(() => expect(act).toHaveBeenCalledTimes(3));
    expect(act.mock.calls[2][0]).toBe(THREAD);
    expect(act.mock.calls[2][1]).toMatchObject({ kind: 'jar', jar: 'thread', confirmed: true });
    release();
  });

  it('asks on the backend refusal even when the pane shows no open tab', async () => {
    const release = await attachPane();
    // A tab went live after the pane's last state: the backend's hold sees it.
    seed('worktree', [page('a', { suspended: true })]);
    const view = mount();
    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-private'));
    const dialog = await view.findByRole('dialog');
    expect(dialog.textContent).toContain('The open tabs close and load their addresses again in the new cookie jar when you go back to them.');
    await fireEvent.click(within(dialog).getByRole('button', { name: 'Switch cookie jar' }));
    await waitFor(() => expect(act).toHaveBeenCalledTimes(2));
    expect(act.mock.calls[1][1]).toMatchObject({ kind: 'jar', jar: 'private', confirmed: true });
    release();
  });

  // The confirmed switch is sent through the same checks as the first: not
  // once this window lost the thread's browser, nor from a pane that closed
  // while the question was up.
  it.each([
    ['this window no longer hosts the browser', () => setHostWindowFromBootstrap(false)],
    ['the pane that asked detached', (release: () => void) => release()],
  ])('sends no confirmed switch once %s', async (_name, lose) => {
    const release = await attachPane();
    seed('worktree', [page('a')]);
    const view = mount();
    try {
      await fireEvent.click(view.getByTestId('browser-jar'));
      await fireEvent.click(await view.findByTestId('browser-jar-thread'));
      const dialog = await view.findByRole('dialog');
      expect(act).toHaveBeenCalledTimes(1);
      lose(release);
      await fireEvent.click(within(dialog).getByRole('button', { name: 'Switch cookie jar' }));
      await tick();
      await Promise.resolve();
      expect(act).toHaveBeenCalledTimes(1);
    } finally {
      setHostWindowFromBootstrap(true);
      release();
    }
  });

  it('switches without asking when the backend has no live tab, and does nothing for the current jar', async () => {
    const release = await attachPane();
    live = false;
    seed('worktree', [page('a', { suspended: true }), page('b', { error: 'failed' })]);
    const view = mount();
    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-worktree'));
    await tick();
    expect(act).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-private'));
    await waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    await tick();
    expect(view.queryByRole('dialog')).toBeNull();
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'jar', jar: 'private' });
    expect(act.mock.calls[0][1].confirmed).toBeFalsy();
    release();
  });

  it('shows any other refusal in the banner and asks nothing', async () => {
    act.mockImplementation(async () => {
      throw new TransportError('internal', 'browser: save pages: disk full');
    });
    const release = await attachPane();
    seed('worktree', [page('a')]);
    const view = mount();
    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-thread'));
    await waitFor(() => expect(browserActionError(THREAD)).toContain('disk full'));
    expect(view.queryByRole('dialog')).toBeNull();
    release();
  });

  it('renders nothing outside a browser pane', () => {
    seed('worktree', [page('a')]);
    const view = render(BrowserJarControl);
    expect(view.queryByTestId('browser-jar')).toBeNull();
  });
});
