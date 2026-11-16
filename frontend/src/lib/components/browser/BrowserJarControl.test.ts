import { fireEvent, render, waitFor, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserJarControl from './BrowserJarControl.svelte';
import BrowserJarControlHarness from './BrowserJarControlHarness.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';

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

describe('<BrowserJarControl>', () => {
  let act: ReturnType<typeof vi.fn>;
  beforeEach(() => {
    resetBrowserCompanionForTest();
    act = setBindingMock('BrowserCompanionDo', vi.fn(async (_thread: string, action: { jar?: string }) => ({
      kind: 'state', threadId: THREAD, jar: action.jar, pages: [],
    })));
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

  it('asks before switching with tabs open, and switches only when confirmed', async () => {
    seed('worktree', [page('a'), page('b'), page('c', { suspended: true })]);
    const view = mount();
    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-thread'));
    const dialog = await view.findByRole('dialog');
    expect(dialog.textContent).toContain('Switch to This browser only?');
    expect(dialog.textContent).toContain('The 2 open tabs close and load their addresses again in the new cookie jar when you go back to them.');
    expect(dialog.textContent).toContain('form input, back and forward history, and sign-ins kept in the current jar');
    expect(act).not.toHaveBeenCalled();

    await fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await tick();
    expect(view.queryByRole('dialog')).toBeNull();
    expect(act).not.toHaveBeenCalled();

    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-thread'));
    await fireEvent.click(within(await view.findByRole('dialog')).getByRole('button', { name: 'Switch cookie jar' }));
    await waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(act.mock.calls[0][0]).toBe(THREAD);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'jar', jar: 'thread' });
  });

  it('switches at once with no open tab, and does nothing for the current jar', async () => {
    seed('worktree', [page('a', { suspended: true }), page('b', { error: 'failed' })]);
    const view = mount();
    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-worktree'));
    await tick();
    expect(act).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('browser-jar'));
    await fireEvent.click(await view.findByTestId('browser-jar-private'));
    await waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(view.queryByRole('dialog')).toBeNull();
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'jar', jar: 'private' });
  });

  it('renders nothing outside a browser pane', () => {
    seed('worktree', [page('a')]);
    const view = render(BrowserJarControl);
    expect(view.queryByTestId('browser-jar')).toBeNull();
  });
});
