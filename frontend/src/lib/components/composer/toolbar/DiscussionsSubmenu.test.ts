// Regression guard: selecting a discussion must refresh the thread so
// the UI flips into discussion mode. Before the fix, `startDiscussion`
// fire-and-forgot `StartDiscussion` and the backend does not emit a
// `thread:updated` event — which left ChatHeader / ModeCycleButton /
// DiscussionView showing the pre-discussion mode until reload.
//
// DiscussionsSubmenu is pure presentation: `definitions` and `error` are
// props owned by ModelProviderMenu (see ensureDiscussions there). These
// tests drive the component directly off those props instead of mocking
// ListDiscussionsForThread.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import DiscussionsSubmenu from './DiscussionsSubmenu.svelte';
import type { Thread } from '../../../types/models';
import type { DiscussionDefinition } from '../../../types/discussion';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import { buildPane as buildRegisteredPane, makeThread as makeBaseThread } from '../../../../test/helpers/chat';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return makeBaseThread({
    workspacePath: '/tmp',
    projectPath: '/tmp',
    ...overrides,
  });
}

async function buildPane(thread: Thread) {
  return buildRegisteredPane(thread);
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

const projectDef: DiscussionDefinition = {
  id: 'proj-def',
  name: 'Project Reviewers',
  scope: 'project',
  projectId: '/tmp',
  description: 'Project-scoped crew',
  participants: [],
  settings: {} as DiscussionDefinition['settings'],
  createdAt: 0,
  updatedAt: 0,
};

describe('<DiscussionsSubmenu>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('buckets project-scoped definitions above global ones', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat' }));

    const { findByRole, findByText } = render(DiscussionsSubmenu, {
      props: { pane, definitions: [architects, projectDef], error: null },
    });

    await findByText('Project');
    await findByText('Global');
    await findByRole('menuitem', { name: /Project Reviewers/i });
    await findByRole('menuitem', { name: /Architects/i });
  });

  it('a project-scoped definition for a different project is bucketed as neither section (filtered out)', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat', projectPath: '/other' }));

    const { queryByRole } = render(DiscussionsSubmenu, {
      props: { pane, definitions: [projectDef], error: null },
    });

    // projectDef.projectId is '/tmp' but the pane's project is '/other',
    // and its scope isn't 'global' either, so it renders in neither bucket.
    await waitFor(() => {
      expect(queryByRole('menuitem', { name: /Project Reviewers/i })).toBeNull();
    });
  });

  it('renders the error prop and skips the definition list entirely', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat' }));

    const { findByTestId, queryByRole } = render(DiscussionsSubmenu, {
      props: { pane, definitions: [architects], error: 'db offline' },
    });

    const errorEl = await findByTestId('discussions-submenu-error');
    expect(errorEl.textContent).toBe('db offline');
    // Definitions are not rendered while an error is present, even if
    // some were loaded previously (stale-while-revalidate keeps them in
    // the parent's state, but the submenu shows the error, not a stale list).
    expect(queryByRole('menuitem', { name: /Architects/i })).toBeNull();
  });

  it('refreshes the thread after StartDiscussion so the UI flips mode', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat' }));

    const start = setBindingMock('StartDiscussionByID', async () => {});
    const refreshed = makeThread({ mode: 'discussion', discussionId: architects.id });
    const getThread = setBindingMock('GetThread', async () => refreshed);

    const { findByRole } = render(DiscussionsSubmenu, {
      props: { pane, definitions: [architects], error: null },
    });

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

    const { findByRole } = render(DiscussionsSubmenu, {
      props: { pane, definitions: [architects], error: null },
    });
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
      props: { pane, definitions: [architects], error: null, onSelect: onSelectSpy },
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

    const { findByRole } = render(DiscussionsSubmenu, {
      props: { pane, definitions: [architects], error: null },
    });
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

  // Harmless defense: even though ModelProviderMenu now hides the
  // Discussions entry entirely for a draft/unstarted thread (empty id),
  // startDiscussion still guards on pane.threadId in case this component
  // is ever reached in that state some other way.
  it('toasts instead of starting when the pane has no threadId', async () => {
    const pane = await buildPane(makeThread({ mode: 'chat', id: '' }));
    const start = setBindingMock('StartDiscussionByID', async () => {});

    const { findByRole } = render(DiscussionsSubmenu, {
      props: { pane, definitions: [architects], error: null },
    });
    const row = await findByRole('menuitem', { name: /Architects/i });
    await fireEvent.click(row);

    await waitFor(() => {
      expect(start).not.toHaveBeenCalled();
    });
  });
});
