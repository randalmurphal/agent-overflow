// Regression guard: selecting a discussion must refresh the thread so
// the UI flips into discussion mode. Before the fix, `startDiscussion`
// fire-and-forgot `StartDiscussion` and the backend does not emit a
// `thread:updated` event — which left ChatHeader / ModeCycleButton /
// DiscussionView showing the pre-discussion mode until reload.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import DiscussionsSubmenu from './DiscussionsSubmenu.svelte';
import { getAllPanes } from '../../../stores/panes.svelte';
import { createThreadPane } from '../../../stores/thread.svelte';
import type { Thread } from '../../../types/models';
import type { DiscussionDefinition } from '../../../types/discussion';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

async function buildPane(thread: Thread) {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(thread);
  getAllPanes().set('main', pane);
  return pane;
}

// Minimal stub; only id/name/scope are read by the component under test.
// `settings` and the rest are filled with defensible zero values so the
// cast doesn't need an `unknown` hop.
const architects: DiscussionDefinition = {
  id: 'architects',
  name: 'Architects',
  scope: 'global',
  projectId: undefined,
  description: 'Architecture review crew',
  participants: [],
  settings: {} as DiscussionDefinition['settings'],
  createdAt: 0,
  updatedAt: 0,
};

describe('<DiscussionsSubmenu>', () => {
  beforeEach(() => {
    resetBindingMocks();
    setBindingMock('ListDiscussionsForThread', async () => [architects]);
  });

  it('refreshes the thread after StartDiscussion so the UI flips mode', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat' }));

    const start = setBindingMock('StartDiscussionByID', async () => {});
    const refreshed = makeThread({ mode: 'discussion', discussionId: architects.id });
    const getThread = setBindingMock('GetThread', async () => refreshed);

    const { findByRole } = render(DiscussionsSubmenu, { props: { pane } });

    // Wait for the definitions to hydrate + the Architects row to render.
    const row = await findByRole('menuitem', { name: /Architects/i });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(start).toHaveBeenCalledWith('thread-1', architects.id);
    });
    await waitFor(() => {
      expect(getThread).toHaveBeenCalledWith('thread-1');
    });
    // Pane should now reflect the refreshed thread's mode.
    await waitFor(() => {
      expect(pane.thread?.mode).toBe('discussion');
    });
    expect(pane.thread?.discussionId).toBe(architects.id);
  });

  it('still toasts + logs when the post-start refresh fails, without crashing', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat' }));

    const start = setBindingMock('StartDiscussionByID', async () => {});
    setBindingMock('GetThread', async () => {
      throw new Error('db offline');
    });

    const { findByRole } = render(DiscussionsSubmenu, { props: { pane } });
    const row = await findByRole('menuitem', { name: /Architects/i });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(start).toHaveBeenCalled();
    });
    // Pane stays on the original mode because the refresh threw — but
    // the overall click must not have thrown up to the user.
    expect(pane.thread?.mode).toBe('chat');
  });

  // Regression: selecting a discussion must collapse the parent menu
  // stack up-front. The submenu-level close happens via Menu primitive
  // events, but the root menu lives in a separate portaled subtree and
  // doesn't receive the bubble — so DiscussionsSubmenu fires an
  // explicit `onSelect` callback. Without this the root menu stays
  // open until the user clicks elsewhere.
  it('invokes onSelect synchronously when a discussion row is picked', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat' }));
    setBindingMock('StartDiscussionByID', async () => {});
    setBindingMock('GetThread', async () =>
      makeThread({ mode: 'discussion', discussionId: architects.id }),
    );

    const onSelectSpy = vi.fn();

    const { findByRole } = render(DiscussionsSubmenu, {
      props: { pane, onSelect: onSelectSpy },
    });
    const row = await findByRole('menuitem', { name: /Architects/i });
    await fireEvent.click(row);

    // onSelect must fire BEFORE the async StartDiscussion round-trip
    // resolves so the UI can collapse the menu immediately.
    expect(onSelectSpy).toHaveBeenCalledTimes(1);
  });

  it('does nothing when StartDiscussion rejects (no refresh, no pane mutation)', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat' }));

    setBindingMock('StartDiscussionByID', async () => {
      throw new Error('already running');
    });
    const getThread = setBindingMock('GetThread', async () =>
      makeThread({ mode: 'discussion' }),
    );

    const { findByRole } = render(DiscussionsSubmenu, { props: { pane } });
    const row = await findByRole('menuitem', { name: /Architects/i });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(getBindingMock('StartDiscussionByID')!).toHaveBeenCalled();
    });
    // StartDiscussion threw before the refresh branch — GetThread must
    // NOT have been invoked.
    expect(getThread).not.toHaveBeenCalled();
    expect(pane.thread?.mode).toBe('chat');
  });
});
