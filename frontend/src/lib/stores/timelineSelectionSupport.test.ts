import { afterEach, beforeEach, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { takePinnedBackend, withBackendTarget } from '../transport/backends';
import { noteThread, __resetEntityIndexForTest } from '../transport/entityIndex';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import type { TransportHello } from '../transport/wsClient';
import type { TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';
import { GetThreadUserMessageTicks, ListActivityRunMembers, ListItemsAfterCursor, ListItemsBeforeCursor, ListThreadSliceAround, SyncThreadWindow } from './bindings';

const hello: TransportHello = { protocolVersion: 1, capabilities: ['timeline.scopes.v1'], backendId: '', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 };
const cursor = { turnIndex: 0, itemIndex: 0, itemId: 'first' };
const readers = [
  ['SyncThreadWindow', (selection: TimelineSelection) => SyncThreadWindow('thread', { selection, anchorItemId: '', itemBudget: 20, haveEpoch: -1, haveRev: -1, inlinePreviews: false, runWindowRows: 8, maxBytes: 0 })],
  ['ListActivityRunMembers', (selection: TimelineSelection) => ListActivityRunMembers('thread', { selection, runFirstItemId: 'first', direction: 'after', shape: {} })],
  ['ListThreadSliceAround', (selection: TimelineSelection) => ListThreadSliceAround('thread', '', 20, {}, selection)],
  ['ListItemsBeforeCursor', (selection: TimelineSelection) => ListItemsBeforeCursor('thread', cursor, 20, {}, selection)],
  ['ListItemsAfterCursor', (selection: TimelineSelection) => ListItemsAfterCursor('thread', cursor, 20, {}, selection)],
  ['GetTimelineUserMessageTicks', (selection: TimelineSelection) => GetThreadUserMessageTicks('thread', selection)],
] as const;

beforeEach(() => { resetBindingMocks(); resetStagedBackends(); __resetEntityIndexForTest(); });
afterEach(() => { resetStagedBackends(); __resetEntityIndexForTest(); __setTransportHelloForTest(null); });

it.each(readers)('%s refuses unsupported scoped reads before issuing RPC and recovers on upgrade', async (name, read) => {
  const rpc = setBindingMock(name, async () => ({}));
  __setTransportHelloForTest({ ...hello, capabilities: [] });
  for (const selection of [{ scopeRootId: 'agent' }, { tools: true }]) {
    expect(() => read(selection)).toThrow('Update the computer hosting this thread');
  }
  expect(rpc).not.toHaveBeenCalled();
  __setTransportHelloForTest(hello);
  await read({ scopeRootId: 'agent' });
  expect(rpc).toHaveBeenCalledOnce();
  __setTransportHelloForTest({ ...hello, capabilities: [] });
  expect(() => read({ scopeRootId: 'agent' })).toThrow('Update the computer hosting this thread');
  expect(rpc).toHaveBeenCalledOnce();
});

it('negotiates against the conversation owner, independently of the home computer', async () => {
  __setTransportHelloForTest(hello);
  const remote = stageBackend({ id: 'remote', hello: { ...hello, capabilities: [] } });
  noteThread('thread', 'remote');
  const rpc = setBindingMock('ListThreadSliceAround', async () => ({}));
  expect(() => ListThreadSliceAround('thread', '', 20, {}, { scopeRootId: 'agent' })).toThrow('Update the computer');
  expect(rpc).not.toHaveBeenCalled();
  __setTransportHelloForTest({ ...hello, capabilities: [] });
  remote.setHello(hello);
  await ListThreadSliceAround('thread', '', 20, {}, { scopeRootId: 'agent' });
  expect(rpc).toHaveBeenCalledExactlyOnceWith('thread', '', 20, { selection: { scopeRootId: 'agent' } });
});

it('keeps legacy positional arity for main-thread reads on older hosts', async () => {
  __setTransportHelloForTest({ ...hello, capabilities: [] });
  const around = setBindingMock('ListThreadSliceAround', async () => ({}));
  const before = setBindingMock('ListItemsBeforeCursor', async () => ({}));
  const after = setBindingMock('ListItemsAfterCursor', async () => ({}));
  const ticks = setBindingMock('GetThreadUserMessageTicks', async () => []);
  const scopedTicks = setBindingMock('GetTimelineUserMessageTicks', async () => []);
  await ListThreadSliceAround('thread', '', 20, { runWindowRows: 8 });
  await ListItemsBeforeCursor('thread', cursor, 20, {});
  await ListItemsAfterCursor('thread', cursor, 20, {});
  await GetThreadUserMessageTicks('thread');
  expect(around).toHaveBeenCalledExactlyOnceWith('thread', '', 20, { runWindowRows: 8, selection: {} });
  expect(before).toHaveBeenCalledExactlyOnceWith('thread', cursor, 20, { selection: {} });
  expect(after).toHaveBeenCalledExactlyOnceWith('thread', cursor, 20, { selection: {} });
  expect(ticks).toHaveBeenCalledExactlyOnceWith('thread');
  expect(scopedTicks).not.toHaveBeenCalled();
});

it('checks an explicit RPC target without consuming the routing pin', async () => {
  __setTransportHelloForTest({ ...hello, capabilities: [] });
  const remote = stageBackend({ id: 'remote', hello });
  const rpc = setBindingMock('ListThreadSliceAround', async () => {
    expect(takePinnedBackend()).toBe('remote');
    return {};
  });
  await withBackendTarget('remote', () => ListThreadSliceAround('thread', '', 20, {}, { scopeRootId: 'agent' }));
  expect(rpc).toHaveBeenCalledOnce();
  remote.setHello({ ...hello, capabilities: [] });
  __setTransportHelloForTest(hello);
  expect(() => withBackendTarget('remote', () => ListThreadSliceAround('thread', '', 20, {}, { scopeRootId: 'agent' }))).toThrow('Update the computer');
  expect(rpc).toHaveBeenCalledOnce();
});
