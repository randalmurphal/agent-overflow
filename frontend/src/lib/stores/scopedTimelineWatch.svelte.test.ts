// A scoped timeline reads a subagent's rows, which the backend sends only to
// a connection watching that scope. These pin that the surface names its
// scopes on the socket before its first history read, restates them when
// context resolution names another scope, and releases them on dispose.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import { installTimelineScopeCapability, installPaneMocks, makeItem, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { wsClient } from '../transport/wsClient';

let log: string[];

beforeEach(() => {
  installThreadPaneTestEnv();
  installTimelineScopeCapability();
  log = [];
  vi.spyOn(wsClient, 'setWatchedThreads').mockImplementation((_threads, scopes) => {
    log.push(`watch ${scopes.map(scope => `${scope.threadId}/${scope.scopeRootId}`).sort().join(',')}`);
  });
});

afterEach(() => { vi.restoreAllMocks(); });

function reads(): number { return log.filter(entry => entry.startsWith('read')).length; }
function lastWatchBefore(index: number): string | undefined {
  return log.slice(0, index).filter(entry => entry.startsWith('watch')).at(-1);
}

it('watches its scope before its first read, adds its launch row\'s scope once resolved, and releases both', async () => {
  const { createAgentScopeView } = await import('./agentScopeView.svelte');
  const pane = createThreadPane();
  const outer = makeItem({ id: 'outer', threadId: 't', itemIndex: 1, kind: 'tool_call', toolName: 'Agent', status: 'running' });
  const inner = makeItem({ id: 'inner', threadId: 't', parentId: 'outer', itemIndex: 2, kind: 'tool_call', toolName: 'Agent', status: 'running' });
  const child = makeItem({ id: 'inner-child', threadId: 't', parentId: 'inner', itemIndex: 3 });
  installPaneMocks([outer, inner, child]);
  await pane.switchThread(makeThread({ id: 't' }));
  const slice = getBindingMock('ListThreadSliceAround')!;
  setBindingMock('SyncThreadWindow', async (threadId: unknown, req: unknown) => {
    const scopeRootId = (req as { selection?: { scopeRootId?: string } }).selection?.scopeRootId;
    if (scopeRootId) log.push(`read ${scopeRootId}`);
    const page = await slice(threadId, '', 0, req);
    return { status: 'stale', epoch: 1, rev: 1, generation: 'test-generation', page };
  });
  log = [];

  const view = createAgentScopeView(pane, 'inner', { viewKey: 'agent', openAgentPane: () => {} });
  view.start();
  try {
    await vi.waitFor(() => expect(reads()).toBe(2));
    const first = log.indexOf('read inner');
    // The first read goes out behind a watch naming the agent's own scope.
    expect(lastWatchBefore(first)).toBe('watch t/inner');
    // Its launch row lives in the outer agent's scope: once resolution
    // says so, that scope is watched too, and the view re-reads behind it
    // for anything written there before the watch applied.
    const second = log.indexOf('read inner', first + 1);
    expect(lastWatchBefore(second)).toBe('watch t/inner,t/outer');
    expect(view.pane.getItemById('inner-child')).toBeDefined();

    // Resolution that names nothing new reads nothing more.
    await new Promise(resolve => setTimeout(resolve, 250));
    expect(reads()).toBe(2);
  } finally {
    view.dispose();
    pane.clear();
  }
  expect(log.filter(entry => entry.startsWith('watch')).at(-1)).toBe('watch ');
});

it('a top-level agent\'s view watches only its own scope and reads once', async () => {
  const { createAgentScopeView } = await import('./agentScopeView.svelte');
  const pane = createThreadPane();
  const launch = makeItem({ id: 'agent', threadId: 't', itemIndex: 1, kind: 'tool_call', toolName: 'Agent', status: 'running' });
  installPaneMocks([launch, makeItem({ id: 'agent-child', threadId: 't', parentId: 'agent', itemIndex: 2 })]);
  await pane.switchThread(makeThread({ id: 't' }));
  const slice = getBindingMock('ListThreadSliceAround')!;
  setBindingMock('SyncThreadWindow', async (threadId: unknown, req: unknown) => {
    const scopeRootId = (req as { selection?: { scopeRootId?: string } }).selection?.scopeRootId;
    if (scopeRootId) log.push(`read ${scopeRootId}`);
    const page = await slice(threadId, '', 0, req);
    return { status: 'stale', epoch: 1, rev: 1, generation: 'test-generation', page };
  });
  log = [];

  const view = createAgentScopeView(pane, 'agent', { viewKey: 'agent', openAgentPane: () => {} });
  view.start();
  try {
    await vi.waitFor(() => expect(view.pane.loading).toBe(false));
    await new Promise(resolve => setTimeout(resolve, 250));
    expect(reads()).toBe(1);
    expect(lastWatchBefore(log.indexOf('read agent'))).toBe('watch t/agent');
    expect(log.filter(entry => entry.startsWith('watch')).every(entry => entry === 'watch t/agent')).toBe(true);
  } finally {
    view.dispose();
    pane.clear();
  }
});
