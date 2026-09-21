import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import McpServersTrigger from './McpServersTrigger.svelte';
import { ThreadMCPServer } from '../../../stores/bindings';
import { resetPanesForTest } from '../../../stores/panes.svelte';
import { getToasts } from '../../../stores/toast.svelte';
import { refreshMcpServers } from '../../../stores/mcpServers.svelte';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../../test/mocks/wailsio-runtime';
import { buildPane, makeThread } from '../../../../test/helpers/chat';

function row(over: Partial<ThreadMCPServer> = {}): ThreadMCPServer {
  return new ThreadMCPServer({
    provider: 'claude',
    name: 'srv',
    status: 'connected',
    disabled: false,
    source: 'config',
    ...over,
  });
}

describe('<McpServersTrigger>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
  });

  it('primes the enabled-count badge without spawning a provider status check', async () => {
    setBindingMock('ListThreadMcpServers', async () => [
      row({ name: 'a' }),
      row({ name: 'b', disabled: true, status: 'disabled' }),
      row({ name: 'c', status: 'unknown' }),
    ]);
    setBindingMock('RefreshMcpServerStatus', async () => []);
    const pane = await buildPane(makeThread());

    const { getByTestId } = render(McpServersTrigger, { props: { pane } });

    await waitFor(() => {
      expect(getByTestId('composer-mcp-trigger')).toHaveAttribute('data-enabled-count', '2');
    });
    // The badge must never cost a `claude mcp list` spawn — only an open
    // menu permits that.
    expect(getBindingMock('RefreshMcpServerStatus')).not.toHaveBeenCalled();
  });

  it('two panes on one workspace share a single listing', async () => {
    const list = setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a' })]);
    const first_ = await buildPane(
      makeThread({ id: 'thread-a', workspacePath: '/repo' }),
      [],
      'main',
    );
    const second = await buildPane(
      makeThread({ id: 'thread-b', workspacePath: '/repo' }),
      [],
      'pane-1',
    );

    const view = render(McpServersTrigger, { props: { pane: first_ } });
    render(McpServersTrigger, { props: { pane: second } });

    await waitFor(() => {
      const badges = view.getAllByTestId('composer-mcp-trigger');
      expect(badges).toHaveLength(2);
      for (const badge of badges) expect(badge).toHaveAttribute('data-enabled-count', '1');
    });
    expect(list).toHaveBeenCalledTimes(1);
  });

  it('lists two worktrees of one project independently', async () => {
    // Claude walks `.mcp.json` from the cwd out, so a worktree's membership
    // is its own answer — sharing the root checkout's key would render one
    // worktree's servers in the other.
    const list = setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a' })]);
    const root = await buildPane(
      makeThread({ id: 'thread-root', workspacePath: '/repo' }),
      [],
      'main',
    );
    const worktree = await buildPane(
      makeThread({ id: 'thread-wt', workspacePath: '/repo/.wt/a' }),
      [],
      'pane-1',
    );

    render(McpServersTrigger, { props: { pane: root } });
    render(McpServersTrigger, { props: { pane: worktree } });

    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
    expect(list.mock.calls.map((c) => c[0]).sort()).toEqual(['thread-root', 'thread-wt']);
  });

  it('does NOT re-list when the pane switches threads inside one workspace', async () => {
    // The entity is the workspace, so a thread switch inside it is the same
    // entity. Re-attaching would drop the shared listing to refcount zero
    // and re-list for a change the entity never saw.
    const list = setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a' })]);
    const pane = await buildPane(makeThread({ id: 'thread-a', workspacePath: '/repo' }));

    const { getByTestId } = render(McpServersTrigger, { props: { pane } });
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));

    pane.replaceThread(makeThread({ id: 'thread-b', workspacePath: '/repo' }));
    await waitFor(() =>
      expect(getByTestId('composer-mcp-trigger')).toHaveAttribute('data-enabled-count', '1'),
    );
    expect(list).toHaveBeenCalledTimes(1);

    // …and the ctx followed the pane, so the next listing runs against the
    // thread it holds now rather than the one it attached with.
    refreshMcpServers(' claude:/repo');
    await waitFor(() => expect(list).toHaveBeenCalledTimes(2));
    expect(list).toHaveBeenLastCalledWith('thread-b');
  });

  it('opening the menu re-lists and permits the chained provider status fetch', async () => {
    const list = setBindingMock('ListThreadMcpServers', async () => [
      row({ name: 'a', status: 'unknown' }),
    ]);
    const refresh = setBindingMock('RefreshMcpServerStatus', async () => []);
    const pane = await buildPane(makeThread());

    const { getByTestId } = render(McpServersTrigger, { props: { pane } });
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));

    await fireEvent.click(getByTestId('composer-mcp-trigger'));

    await waitFor(() => {
      expect(refresh).toHaveBeenCalledTimes(1);
    });
  });

  it('offers Sign in again on a failed OAuth-credentialed row and shows the real error', async () => {
    // The incident shape end to end: a Codex server whose startup failed
    // with a revoked refresh token lists as failed + authStatus oAuth
    // (Codex deterministically omits failureReason for this case). The row
    // must show the provider's error and offer the sign-in, never
    // "Starting…".
    setBindingMock('ListThreadMcpServers', async () => [
      row({
        provider: 'codex',
        name: 'atlassian',
        status: 'failed',
        authStatus: 'oAuth',
        error: 'invalid_grant: Invalid refresh token',
        source: 'session',
      }),
    ]);
    const auth = setBindingMock('TriggerMcpAuth', async () => ({
      authUrl: 'https://example.test/oauth',
      provider: 'codex',
      requiresUserAction: true,
    }));
    const openURL = setBindingMock('OpenExternalURL', async () => {});
    const pane = await buildPane(makeThread({ provider: 'codex' }));

    const { getByTestId, findByRole, getByText } = render(McpServersTrigger, { props: { pane } });
    await fireEvent.click(getByTestId('composer-mcp-trigger'));

    // The accessible name carries the server; the visible text is the
    // short label.
    const signIn = await findByRole('button', { name: 'Sign in to atlassian again' });
    expect(signIn.textContent).toBe('Sign in again');
    getByText(/invalid_grant: Invalid refresh token/);
    await fireEvent.click(signIn);

    await waitFor(() => expect(auth).toHaveBeenCalledWith('thread-1', 'atlassian'));
    await waitFor(() => expect(openURL).toHaveBeenCalledWith('https://example.test/oauth'));
  });

  it('offers Sign in again on a failed OAuth-credentialed CONFIG row too', async () => {
    // The inactive-thread path: no live session, so the row comes from
    // config + the status cache — which records authStatus from the
    // ephemeral probe. The remedy must not depend on the thread being
    // live.
    setBindingMock('ListThreadMcpServers', async () => [
      row({
        provider: 'codex',
        name: 'atlassian',
        status: 'failed',
        authStatus: 'oAuth',
        error: 'invalid_grant: Invalid refresh token',
        source: 'config',
      }),
    ]);
    const auth = setBindingMock('TriggerMcpAuth', async () => ({
      authUrl: 'https://example.test/oauth',
      provider: 'codex',
      requiresUserAction: true,
    }));
    setBindingMock('OpenExternalURL', async () => {});
    const pane = await buildPane(makeThread({ provider: 'codex' }));

    const { getByTestId, findByRole } = render(McpServersTrigger, { props: { pane } });
    await fireEvent.click(getByTestId('composer-mcp-trigger'));

    const signIn = await findByRole('button', { name: 'Sign in to atlassian again' });
    await fireEvent.click(signIn);
    await waitFor(() => expect(auth).toHaveBeenCalledWith('thread-1', 'atlassian'));
  });

  it('signs in from a draft placeholder without materializing a thread', async () => {
    setBindingMock('ListWorkspaceMcpServers', async () => [
      row({
        provider: 'codex',
        name: 'atlassian',
        status: 'needs-auth',
        authStatus: 'oAuth',
        source: 'config',
      }),
    ]);
    const workspaceAuth = setBindingMock('TriggerWorkspaceMcpAuth', async () => ({
      authUrl: 'https://example.test/oauth',
      provider: 'codex',
      requiresUserAction: true,
    }));
    const threadAuth = setBindingMock('TriggerMcpAuth', async () => {
      throw new Error('draft sign-in must not require a persisted thread');
    });
    const createThread = setBindingMock('CreateThread', async () => {
      throw new Error('draft sign-in must not materialize the draft');
    });
    const openURL = setBindingMock('OpenExternalURL', async () => {});
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    pane.startDraftPlaceholder(
      {
        id: 'project-1',
        path: '/repo',
        name: 'Repository',
        sortPosition: 0,
        createdAt: 0,
        updatedAt: 0,
        archived: false,
      },
      'chat',
      { provider: 'codex', workspacePath: '/repo' },
    );

    const { getByTestId, findByRole } = render(McpServersTrigger, { props: { pane } });
    await fireEvent.click(getByTestId('composer-mcp-trigger'));
    await fireEvent.click(await findByRole('button', { name: 'Sign in to atlassian' }));

    await waitFor(() =>
      expect(workspaceAuth).toHaveBeenCalledWith('codex', '/repo', 'atlassian'),
    );
    expect(openURL).toHaveBeenCalledWith('https://example.test/oauth');
    expect(threadAuth).not.toHaveBeenCalled();
    expect(createThread).not.toHaveBeenCalled();
  });

  it('surfaces a failed listing in the menu instead of an empty state', async () => {
    setBindingMock('ListThreadMcpServers', async () => {
      throw new Error('mcp listing unavailable');
    });
    const pane = await buildPane(makeThread());

    const { getByTestId, findByTestId } = render(McpServersTrigger, { props: { pane } });
    await fireEvent.click(getByTestId('composer-mcp-trigger'));

    const error = await findByTestId('mcp-menu-error');
    expect(error.textContent ?? '').toMatch(/mcp listing unavailable/);
  });
});

it.each(['claude', 'codex'] as const)('shows %s off, blocked and failed independently of the switch', async (provider) => {
  const toggle = setBindingMock('SetThreadMcpServerEnabled', async () => {});
  setBindingMock('ListThreadMcpServers', async () => [
    row({ provider, name: 'off-server', source: 'session', disabled: true, status: 'disabled' }),
    row({ provider, name: 'blocked-server', source: 'session', status: 'disabled', toggleDisabledReason: 'Disabled by provider settings.' }),
    row({ provider, name: 'external-server', source: 'session', status: 'connected', toggleDisabledReason: 'Managed elsewhere.' }),
    row({ provider, name: 'failed-server', source: 'session', status: 'failed' }),
  ]);
  const pane = await buildPane(makeThread({ provider }));
  const view = render(McpServersTrigger, { props: { pane } });
  await fireEvent.click(view.getByTestId('composer-mcp-trigger'));
  const off = await view.findByRole('menuitem', { name: 'off-server Off' });
  await waitFor(() => expect(off).not.toHaveAttribute('aria-disabled', 'true'));
  expect(off.querySelector('[data-mcp-enabled]')).toHaveAttribute('data-mcp-enabled', 'false');
  expect(view.queryByRole('button', { name: 'Reconnect off-server' })).toBeNull();
  const blocked = view.getByRole('menuitem', { name: 'blocked-server Blocked' });
  expect(blocked).toHaveAttribute('aria-disabled', 'true');
  expect(blocked).toHaveAttribute('title', 'Disabled by provider settings.');
  expect(blocked.querySelector('[data-mcp-enabled]')).toHaveAttribute('data-mcp-enabled', 'true');
  expect(view.queryByRole('button', { name: 'Reconnect blocked-server' })).toBeNull();
  await fireEvent.click(blocked);
  await fireEvent.keyDown(blocked, { key: 'Enter' });
  await fireEvent.click(view.getByText('external-server'));
  expect(toggle).not.toHaveBeenCalled();
  const failed = view.getByText('failed-server').closest('[data-menuitem]')!;
  expect(failed.querySelector('[data-mcp-enabled]')).toHaveAttribute('data-mcp-enabled', 'true');
  expect(view.getByRole('button', { name: 'Reconnect failed-server' })).toBeVisible();
  await fireEvent.click(off);
  await waitFor(() => expect(toggle).toHaveBeenCalledWith('thread-1', 'off-server', true));
});

it('updates the switch and removes stale actions across repeated toggles', async () => {
  let enabled = false;
  setBindingMock('ListThreadMcpServers', async () => [row({
    provider: 'codex', name: 'srv', source: 'session', disabled: !enabled,
    status: enabled ? 'connected' : 'disabled', tools: enabled ? ['read'] : [],
  })]);
  const toggle = setBindingMock('SetThreadMcpServerEnabled', async (_thread, _name, next) => {
    enabled = next as boolean;
    emitWailsEvent('mcp:status', { provider: 'codex', name: 'srv', status: 'unknown' });
  });
  const pane = await buildPane(makeThread({ provider: 'codex' }));
  const view = render(McpServersTrigger, { props: { pane } });
  await fireEvent.click(view.getByTestId('composer-mcp-trigger'));
  for (const next of [true, false, true, false]) {
    const item = await view.findByRole('menuitem', { name: next ? 'srv Off' : /srv Connected/ });
    await waitFor(() => expect(item).not.toHaveAttribute('aria-disabled', 'true'));
    await fireEvent.click(item);
    await waitFor(() => expect(toggle).toHaveBeenLastCalledWith('thread-1', 'srv', next));
    await waitFor(() => expect(view.getByText('srv').closest('[data-menuitem]')!.querySelector('[data-mcp-enabled]'))
      .toHaveAttribute('data-mcp-enabled', String(next)));
    if (!next) expect(view.queryByRole('button', { name: 'Reconnect srv' })).toBeNull();
  }
});

it('keeps the saved off state and reports a rejected toggle', async () => {
  setBindingMock('ListThreadMcpServers', async () => [row({ provider: 'codex', name: 'srv', source: 'session', disabled: true, status: 'disabled' })]);
  setBindingMock('SetThreadMcpServerEnabled', async () => { throw new Error('config write failed'); });
  const pane = await buildPane(makeThread({ provider: 'codex' }));
  const view = render(McpServersTrigger, { props: { pane } });
  await fireEvent.click(view.getByTestId('composer-mcp-trigger'));
  const off = await view.findByRole('menuitem', { name: 'srv Off' });
  await waitFor(() => expect(off).not.toHaveAttribute('aria-disabled', 'true'));
  await fireEvent.click(off);
  await waitFor(() => expect(getToasts().some((toast) => toast.message.includes('config write failed'))).toBe(true));
  expect(off.querySelector('[data-mcp-enabled]')).toHaveAttribute('data-mcp-enabled', 'false');
  expect(view.queryByRole('button', { name: 'Reconnect srv' })).toBeNull();
});
