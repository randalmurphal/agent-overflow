import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';

import McpServersTrigger from './McpServersTrigger.svelte';
import { ThreadMCPServer } from '../../../stores/bindings';
import { resetPanesForTest } from '../../../stores/panes.svelte';
import { refreshMcpServers } from '../../../stores/mcpServers.svelte';
import {
  getBindingMock,
  resetBindingMocks,
  setBindingMock,
} from '../../../../test/mocks/bindings-app';
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
    refreshMcpServers('claude:/repo');
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
