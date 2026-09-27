import { tick } from 'svelte';
import { render, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import BrowserPane from './BrowserPane.svelte';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { applyBrowserCompanionState, resetBrowserCompanionForTest } from '../../stores/browserCompanion.svelte';

const THREAD = 'thread-1';

function page(id: string, suspended: boolean) {
  return {
    id,
    url: `https://example.test/${id}`,
    title: `Page ${id}`,
    canGoBack: false,
    canGoForward: false,
    ...(suspended ? { suspended: true } : {}),
  };
}

function state(pages: ReturnType<typeof page>[]) {
  return { kind: 'state', threadId: THREAD, activePageId: pages[0]?.id ?? '', visible: true, pages };
}

describe('<BrowserPane> suspended pages', () => {
  beforeEach(() => {
    resetBrowserCompanionForTest();
    setBindingMock('BrowserCompanionPaneDetach', async () => undefined);
    setBindingMock('BrowserCompanionPaneRect', async () => undefined);
  });
  afterEach(() => resetBrowserCompanionForTest());

  it('restores a suspended active page by presenting it, once', async () => {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: state([page('a', true), page('b', false)]) }));
    let restore!: () => void;
    const act = setBindingMock(
      'BrowserCompanionDo',
      vi.fn(() => new Promise((resolve) => {
        restore = () => resolve(state([page('a', false), page('b', false)]));
      })),
    );
    render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });

    await waitFor(() => expect(act).toHaveBeenCalledTimes(1));
    expect(act.mock.calls[0][0]).toBe(THREAD);
    expect(act.mock.calls[0][1]).toMatchObject({ kind: 'activate', pageId: 'a' });
    // A state push while the restore runs asks nothing more.
    applyBrowserCompanionState(state([page('a', true), page('b', false)]) as never);
    await tick();
    restore();
    await tick();
    expect(act).toHaveBeenCalledTimes(1);
  });

  it('reports a refused restore in the banner and asks again only once the page was live', async () => {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: state([page('a', true)]) }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => {
      throw new Error('browser: page a cannot be restored: file is not allowed');
    }));
    const view = render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });

    expect((await view.findByRole('alert')).textContent).toContain('cannot be restored');
    applyBrowserCompanionState(state([page('a', true)]) as never);
    await tick();
    expect(act).toHaveBeenCalledTimes(1);
    // The page came back and was suspended again: presenting it restores it.
    applyBrowserCompanionState(state([page('a', false)]) as never);
    await tick();
    applyBrowserCompanionState(state([page('a', true)]) as never);
    await waitFor(() => expect(act).toHaveBeenCalledTimes(2));
  });

  it('asks nothing for a live active page', async () => {
    setBindingMock('BrowserCompanionPaneAttach', async () => ({ id: 'mount-1', state: state([page('a', false), page('b', true)]) }));
    const act = setBindingMock('BrowserCompanionDo', vi.fn(async () => state([page('a', false), page('b', true)])));
    const view = render(BrowserPane, { ctx: makeStubPanelContext({ threadId: THREAD }) });
    await view.findByText('Page a');
    await tick();
    expect(act).not.toHaveBeenCalled();
  });
});
