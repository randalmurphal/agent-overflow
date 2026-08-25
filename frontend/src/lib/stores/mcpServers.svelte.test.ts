import { afterEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import {
  attachMcpServers,
  isMcpServersLoading,
  MCP_CODEX_KEY,
  mcpRowsSourceThreadId,
  mcpServersKeys,
  mcpTargetFor,
  needsEphemeralRefresh,
  peekMcpServers,
  peekMcpServersError,
  permitMcpStatusFetch,
  reconnectMcpServer,
  refreshMcpServers,
  setMcpServerEnabled,
  type MCPTarget,
} from './mcpServers.svelte';
import { ThreadMCPServer } from './bindings';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import type { EntityAttachment } from './entityStore.svelte';
import { applyTransportGap } from './eventsTransportGap';
import { getToasts, removeToast } from './toast.svelte';

function row(over: Partial<ThreadMCPServer>): ThreadMCPServer {
  return new ThreadMCPServer({
    provider: 'claude',
    name: 'srv',
    status: 'unknown',
    disabled: false,
    source: 'config',
    ...over,
  });
}

async function flush(n = 6): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

const held: Array<EntityAttachment<ThreadMCPServer[]>> = [];

function target(provider: string, threadId: string, workspacePath: string): MCPTarget {
  const resolved = mcpTargetFor(provider, threadId, workspacePath);
  if (!resolved) throw new Error('expected a target');
  return resolved;
}

function attach(t: MCPTarget): EntityAttachment<ThreadMCPServer[]> {
  const handle = attachMcpServers(t.key, {
    provider: t.provider,
    threadId: t.threadId,
    workspacePath: t.workspacePath,
  });
  held.push(handle);
  return handle;
}

afterEach(() => {
  for (const handle of held.splice(0)) handle.release();
});

describe('needsEphemeralRefresh', () => {
  it('never chains on session-sourced listings', () => {
    const rows = [row({ source: 'session', status: 'connected' })];
    expect(needsEphemeralRefresh(rows)).toBe(false);
  });

  it('skips when membership is empty or fully disabled — config enumerates everything', () => {
    expect(needsEphemeralRefresh([])).toBe(false);
    expect(needsEphemeralRefresh([row({ disabled: true, status: 'disabled' })])).toBe(false);
  });

  it('chains only to resolve unknown or stale enabled rows', () => {
    expect(needsEphemeralRefresh([row({})])).toBe(true);
    expect(needsEphemeralRefresh([row({ status: 'connected', stale: true })])).toBe(true);
    expect(needsEphemeralRefresh([row({ status: 'connected' })])).toBe(false);
    // A known failure is an answer, not a gap — no spawn to re-ask.
    expect(needsEphemeralRefresh([row({ status: 'failed' })])).toBe(false);
  });

  it('one unresolved row among resolved ones still chains', () => {
    const rows = [row({ status: 'connected' }), row({ name: 'other' })];
    expect(needsEphemeralRefresh(rows)).toBe(true);
  });
});

describe('mcpTargetFor — entity keys', () => {
  it('keys Claude by WORKSPACE — membership is walked from the cwd out', () => {
    // Two worktrees of one project can legitimately carry different
    // `.mcp.json` files, so neither listing is a stale view of the other.
    const rootThread = mcpTargetFor('claude', 't1', '/repo');
    const worktreeThread = mcpTargetFor('claude', 't2', '/repo/.wt/a');
    expect(rootThread?.key).toBe('claude:/repo');
    expect(worktreeThread?.key).toBe('claude:/repo/.wt/a');
  });

  it('keys Codex app-globally — its enabled flag is global', () => {
    expect(mcpTargetFor('codex', 't1', '/a')?.key).toBe(MCP_CODEX_KEY);
    expect(mcpTargetFor('codex', 't2', '/b')?.key).toBe(MCP_CODEX_KEY);
  });

  it('has no Claude target without a workspace to walk from', () => {
    expect(mcpTargetFor('claude', 't1', '')).toBeNull();
    expect(mcpTargetFor('claude', 't1', '  ')).toBeNull();
  });

  it('has no target for a provider AO does not route MCP for', () => {
    expect(mcpTargetFor('claude-tui', 't1', '/repo')).toBeNull();
    expect(mcpTargetFor('', 't1', '/repo')).toBeNull();
  });

  it('lists from the thread when there is one and from the workspace otherwise', () => {
    expect(mcpTargetFor('claude', 't1', '/repo')?.threadId).toBe('t1');
    expect(mcpTargetFor('claude', '', '/repo')?.threadId).toBe('');
  });
});

describe('mcpServers store — one listing per entity', () => {
  it('shares ONE listing between two panes on the same workspace', async () => {
    const list = setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a' })]);
    const first = attach(target('claude', 't1', '/repo'));
    const second = attach(target('claude', 't2', '/repo'));
    await flush();

    expect(list).toHaveBeenCalledTimes(1);
    expect(mcpServersKeys()).toEqual(['claude:/repo']);
    expect(first.current).toBe(second.current);
  });

  it('lists two worktrees of one project INDEPENDENTLY', async () => {
    const list = setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a' })]);
    attach(target('claude', 't1', '/repo'));
    attach(target('claude', 't2', '/repo/.wt/a'));
    await flush();

    expect(list).toHaveBeenCalledTimes(2);
    expect(mcpServersKeys().sort()).toEqual(['claude:/repo', 'claude:/repo/.wt/a']);
  });

  it('shares ONE listing between Codex threads in different workspaces', async () => {
    const list = setBindingMock('ListThreadMcpServers', async () => [row({ provider: 'codex' })]);
    attach(target('codex', 't1', '/a'));
    attach(target('codex', 't2', '/b'));
    await flush();

    expect(list).toHaveBeenCalledTimes(1);
    expect(mcpServersKeys()).toEqual([MCP_CODEX_KEY]);
  });

  it('reports which thread produced the rows, so a shared key cannot lend a session', async () => {
    setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a', source: 'session' })]);
    const holder = attach(target('claude', 't1', '/repo'));
    await flush();

    expect(holder.current?.[0]?.source).toBe('session');
    expect(mcpRowsSourceThreadId('claude:/repo')).toBe('t1');
    // A pane on the same workspace whose own thread is t2 must not read this
    // as "my session answered".
    expect(mcpRowsSourceThreadId('claude:/repo')).not.toBe('t2');
  });

  it('reports an empty source thread for a workspace-sourced listing', async () => {
    setBindingMock('ListWorkspaceMcpServers', async () => [row({ name: 'a' })]);
    attach(target('claude', '', '/repo'));
    await flush();

    expect(mcpRowsSourceThreadId('claude:/repo')).toBe('');
  });

  it('lists a draft placeholder from its workspace, not from a thread', async () => {
    const workspace = setBindingMock('ListWorkspaceMcpServers', async () => [row({ name: 'a' })]);
    setBindingMock('ListThreadMcpServers', async () => {
      throw new Error('a placeholder has no thread row');
    });
    attach(target('claude', '', '/repo'));
    await flush();

    expect(workspace).toHaveBeenCalledWith('claude', '/repo');
  });

  it('re-sources on attach → release → re-attach', async () => {
    const list = setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a' })]);
    const first = attach(target('claude', 't1', '/repo'));
    await flush();
    expect(list).toHaveBeenCalledTimes(1);

    first.release();
    expect(mcpServersKeys()).toEqual([]);
    expect(peekMcpServers('claude:/repo')).toEqual([]);
    // The source-thread record leaves with the entry, or the next holder
    // would inherit a claim on a session that is no longer listed.
    expect(mcpRowsSourceThreadId('claude:/repo')).toBe('');

    attach(target('claude', 't1', '/repo'));
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('reports the in-flight listing so the menu can spin', async () => {
    let settle: (rows: ThreadMCPServer[]) => void = () => {};
    setBindingMock(
      'ListThreadMcpServers',
      () => new Promise<ThreadMCPServer[]>((resolve) => { settle = resolve; }),
    );
    attach(target('claude', 't1', '/repo'));
    await flush();
    expect(isMcpServersLoading('claude:/repo')).toBe(true);

    settle([row({ name: 'a' })]);
    await flush();
    expect(isMcpServersLoading('claude:/repo')).toBe(false);
  });
});

describe('mcpServers store — toggling', () => {
  // The backend invalidates its (provider, name) status entry on every
  // toggle path, which reaches the frontend as the `mcp:status` unknown
  // sentinel. That is the ONE re-list trigger — see the store header.
  function sentinel(name: string, provider = 'claude'): void {
    emitWailsEvent('mcp:status', { provider, name, status: 'unknown' });
  }

  it('re-lists ONCE per toggle — the sentinel owns the refresh, not the caller', async () => {
    let calls = 0;
    setBindingMock('ListThreadMcpServers', async () => {
      calls += 1;
      return [row({ name: 'a' })];
    });
    setBindingMock('SetThreadMcpServerEnabled', async () => {});
    const acting = target('claude', 't1', '/repo');
    attach(acting);
    await flush();
    expect(calls).toBe(1);

    await setMcpServerEnabled(acting, 'a', true);
    await flush();
    // Nothing yet: the toggle itself does not invalidate.
    expect(calls).toBe(1);

    sentinel('a');
    await flush();
    expect(calls).toBe(2);
  });

  it('heals every WORKSPACE carrying the server, not just the acting one', async () => {
    let enabled = false;
    setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a', disabled: !enabled })]);
    setBindingMock('SetThreadMcpServerEnabled', async () => {
      enabled = true;
    });
    const acting = target('claude', 't1', '/repo');
    const actingHandle = attach(acting);
    // A sibling worktree: its own key now, healed only by the fan-out.
    const worktree = attach(target('claude', 't2', '/repo/.wt/a'));
    await flush();
    expect(actingHandle.current?.[0]?.disabled).toBe(true);
    expect(worktree.current?.[0]?.disabled).toBe(true);

    await setMcpServerEnabled(acting, 'a', true);
    sentinel('a');
    await flush();

    expect(actingHandle.current?.[0]?.disabled).toBe(false);
    expect(worktree.current?.[0]?.disabled).toBe(false);
  });

  // A held key with no rows yet is LOADING, and its in-flight listing was
  // issued against pre-toggle config. Skipping it (membership is unknowable
  // with nothing to match the name against) let that pre-toggle answer land
  // with nothing due to correct it — the sibling workspace kept rendering
  // the state the toggle had already replaced.
  it('re-lists a workspace whose FIRST listing is still in flight when the sentinel lands', async () => {
    let enabled = false;
    const settlers: Array<() => void> = [];
    setBindingMock('ListThreadMcpServers', () => {
      const disabled = !enabled;
      return new Promise<ThreadMCPServer[]>((resolve) => {
        settlers.push(() => resolve([row({ name: 'a', disabled })]));
      });
    });
    const sibling = attach(target('claude', 't2', '/repo/.wt/a'));
    await flush();
    expect(settlers).toHaveLength(1);
    expect(sibling.current).toBeNull();

    // Another workspace toggled the server while this one was still loading.
    enabled = true;
    sentinel('a');
    await flush();
    expect(settlers).toHaveLength(2);

    // The pre-toggle answer lands late; the re-list is what the pane shows.
    settlers[0]();
    settlers[1]();
    await flush();
    expect(sibling.current?.[0]?.disabled).toBe(false);
  });

  it('re-lists ONCE per reconnect, through the same one path', async () => {
    let calls = 0;
    setBindingMock('ListThreadMcpServers', async () => {
      calls += 1;
      return [row({ name: 'a', source: 'session' })];
    });
    setBindingMock('ReconnectMcpServer', async () => {});
    const acting = target('claude', 't1', '/repo');
    attach(acting);
    await flush();
    expect(calls).toBe(1);

    await reconnectMcpServer(acting, 'a');
    await flush();
    expect(calls).toBe(1);

    sentinel('a');
    await flush();
    expect(calls).toBe(2);
  });

  it('routes a draft placeholder toggle to the workspace variant', async () => {
    setBindingMock('ListWorkspaceMcpServers', async () => [row({ name: 'a' })]);
    const toggle = setBindingMock('SetWorkspaceMcpServerEnabled', async () => {});
    const draft = target('claude', '', '/repo');
    attach(draft);
    await flush();

    await setMcpServerEnabled(draft, 'a', false);
    expect(toggle).toHaveBeenCalledWith('claude', '/repo', 'a', false);
  });
});

describe('mcpServers store — mcp:status routing', () => {
  it('treats status="unknown" as an invalidation, not a row to render', async () => {
    let calls = 0;
    setBindingMock('ListThreadMcpServers', async () => {
      calls += 1;
      return [row({ name: 'a', status: calls > 1 ? 'connected' : 'unknown' })];
    });
    attach(target('claude', 't1', '/repo'));
    await flush();
    expect(calls).toBe(1);

    emitWailsEvent('mcp:status', { provider: 'claude', name: 'a', status: 'unknown' });
    await flush();

    expect(calls).toBe(2);
    expect(peekMcpServers('claude:/repo')[0]?.status).toBe('connected');
  });

  it('ignores an invalidation for a server no held entity carries', async () => {
    let calls = 0;
    setBindingMock('ListThreadMcpServers', async () => {
      calls += 1;
      return [row({ name: 'a' })];
    });
    attach(target('claude', 't1', '/repo'));
    await flush();

    emitWailsEvent('mcp:status', { provider: 'claude', name: 'somewhere-else', status: 'unknown' });
    await flush();
    expect(calls).toBe(1);
  });

  it('patches a real status into every entity that shows the server, without re-listing', async () => {
    let calls = 0;
    setBindingMock('ListThreadMcpServers', async () => {
      calls += 1;
      return [row({ name: 'a', status: 'starting' })];
    });
    attach(target('claude', 't1', '/repo'));
    attach(target('claude', 't2', '/other'));
    await flush();
    expect(calls).toBe(2);

    emitWailsEvent('mcp:status', {
      provider: 'claude',
      name: 'a',
      status: 'connected',
      tools: ['t1'],
    });
    await flush();

    expect(calls).toBe(2);
    expect(peekMcpServers('claude:/repo')[0]?.status).toBe('connected');
    expect(peekMcpServers('claude:/other')[0]?.status).toBe('connected');
  });

  it('does not fold an ephemeral probe onto a session row', async () => {
    // A session row carries the thread's own lifecycle truth (the backend
    // merges retained startup state into it). The probe is app-global and
    // can be fired from any pane; it must not overwrite that merge.
    setBindingMock('ListThreadMcpServers', async () => [
      row({
        provider: 'codex',
        name: 'a',
        status: 'failed',
        error: 'invalid_grant: Invalid refresh token',
        source: 'session',
      }),
    ]);
    attach(target('codex', 't1', '/repo'));
    await flush();

    emitWailsEvent('mcp:status', {
      provider: 'codex',
      name: 'a',
      status: 'connected',
      source: 'ephemeral-fetch',
    });
    await flush();

    const patched = peekMcpServers(MCP_CODEX_KEY)[0];
    expect(patched?.status).toBe('failed');
    expect(patched?.error).toContain('invalid_grant');

    // A provider-sourced push is the thread speaking — that still lands.
    emitWailsEvent('mcp:status', {
      provider: 'codex',
      name: 'a',
      status: 'connected',
      source: 'notification',
    });
    await flush();
    expect(peekMcpServers(MCP_CODEX_KEY)[0]?.status).toBe('connected');
  });

  it('re-lists the entities carrying a server whose OAuth just completed', async () => {
    let calls = 0;
    setBindingMock('ListThreadMcpServers', async () => {
      calls += 1;
      return [row({ name: 'a', status: 'needs-auth' })];
    });
    attach(target('claude', 't1', '/repo'));
    await flush();

    emitWailsEvent('mcp:oauth-completed', {
      threadId: 't1',
      provider: 'claude',
      serverName: 'a',
      success: true,
    });
    await flush();
    expect(calls).toBe(2);
  });

  it('surfaces an asynchronous draft sign-in failure without a thread error row', async () => {
    const before = new Set(getToasts().map((toast) => toast.id));
    emitWailsEvent('mcp:oauth-completed', {
      threadId: '',
      provider: 'codex',
      serverName: 'atlassian',
      success: false,
      error: 'browser approval was denied',
    });
    await flush();

    const added = getToasts().filter((toast) => !before.has(toast.id));
    expect(added).toHaveLength(1);
    expect(added[0]).toMatchObject({
      type: 'error',
      message: 'Sign-in failed for atlassian: browser approval was denied',
    });
    for (const toast of added) removeToast(toast.id);
  });

  it('does not toast when an abandoned draft sign-in times out', async () => {
    const before = getToasts().length;
    emitWailsEvent('mcp:oauth-completed', {
      threadId: '',
      provider: 'claude',
      serverName: 'srv',
      success: false,
      timedOut: true,
      error: 'sign-in not confirmed',
    });
    await flush();
    expect(getToasts()).toHaveLength(before);
  });
});

describe('mcpServers store — provider status fetch', () => {
  it('never spawns a status fetch for a badge-priming listing', async () => {
    setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a', status: 'unknown' })]);
    const refresh = setBindingMock('RefreshMcpServerStatus', async () => []);
    attach(target('claude', 't1', '/repo'));
    await flush();

    expect(refresh).not.toHaveBeenCalled();
  });

  it('chains one while a menu holds the permit, and stops when it is released', async () => {
    setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a', status: 'unknown' })]);
    const refresh = setBindingMock('RefreshMcpServerStatus', async () => []);
    const held = target('claude', 't1', '/repo');
    attach(held);
    await flush();

    const release = permitMcpStatusFetch(held.key);
    refreshMcpServers(held.key);
    await flush();
    expect(refresh).toHaveBeenCalledTimes(1);
    expect(refresh).toHaveBeenCalledWith('claude', '/repo');

    release();
    refreshMcpServers(held.key);
    await flush();
    expect(refresh).toHaveBeenCalledTimes(1);
  });
});

describe('mcpServers store — failures', () => {
  it('surfaces a failed listing as state and recovers on the next refresh', async () => {
    let broken = true;
    setBindingMock('ListThreadMcpServers', async () => {
      if (broken) throw new Error('boom');
      return [row({ name: 'a' })];
    });
    const handle = attach(target('claude', 't1', '/repo'));
    await flush();

    expect(handle.error).toContain('boom');
    expect(peekMcpServersError('claude:/repo')).toContain('boom');

    broken = false;
    refreshMcpServers('claude:/repo');
    await flush();

    expect(handle.error).toBeNull();
    expect(peekMcpServers('claude:/repo')).toHaveLength(1);
  });
});

// `mcp:status` is edge-triggered — one frame per (provider, name) change,
// including the `unknown` sentinel that says "the listing moved" — so a frame
// dropped mid-connection (wsClient's forward-seq-skip detection) leaves every
// key carrying that server showing a superseded state. Recovery is blanket
// because the gap carries no server name, and it must not blank membership.
describe('mcpServers store — transport gap', () => {
  it('re-lists every live entity and keeps the rows while they reload', async () => {
    const list = setBindingMock('ListThreadMcpServers', async () => [
      row({ name: 'a', status: 'connected' }),
    ]);
    const first = attach(target('claude', 't1', '/repo'));
    const second = attach(target('claude', 't2', '/other'));
    await flush();
    expect(list).toHaveBeenCalledTimes(2);
    expect(mcpServersKeys().sort()).toEqual(['claude:/other', 'claude:/repo']);

    // Gate the re-list so the assertions below run while the fresh rows are
    // in flight — the window a blanking recovery would render as an empty
    // MCP menu on every open composer.
    let openGate = (): void => {};
    const gate = new Promise<void>((resolve) => {
      openGate = resolve;
    });
    const relist = setBindingMock('ListThreadMcpServers', async () => {
      await gate;
      return [row({ name: 'a', status: 'failed' })];
    });

    applyTransportGap({ channel: 'mcp:status', seq: 55 });
    await flush();

    expect(relist).toHaveBeenCalledTimes(2);
    expect(first.current?.[0]?.status).toBe('connected');
    expect(second.current?.[0]?.status).toBe('connected');

    openGate();
    await flush();
    expect(first.current?.[0]?.status).toBe('failed');
    expect(second.current?.[0]?.status).toBe('failed');
  });

  it('ignores a gap on a channel this store does not own', async () => {
    const list = setBindingMock('ListThreadMcpServers', async () => [row({ name: 'a' })]);
    attach(target('claude', 't1', '/repo'));
    await flush();
    expect(list).toHaveBeenCalledTimes(1);

    applyTransportGap({ channel: 'system:stats', seq: 6 });
    await flush();
    expect(list).toHaveBeenCalledTimes(1);
  });
});
