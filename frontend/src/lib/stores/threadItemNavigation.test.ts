import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { navigateToThreadItem } from './threadItemNavigation';
import { createThreadPane } from './thread.svelte';
import { agentStateForPane, __resetAgentPaneStateForTest } from './agentPane.svelte';
import { installPaneMocks, makeItem, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { getToasts } from './toast.svelte';

beforeEach(resetBindingMocks);
afterEach(__resetAgentPaneStateForTest);

it('opens an unloaded nested search hit at the exact child, with ancestry and no main scroll request', async () => {
  const pane = createThreadPane({ paneId: 'search-main' });
  installPaneMocks([makeItem({ id: 'main-tail', threadId: 't', itemIndex: 100 })]);
  await pane.switchThread(makeThread({ id: 't' }));
  const source = [
    makeItem({ id: 'outer', threadId: 't', kind: 'tool_call', toolName: 'Agent' }),
    makeItem({ id: 'inner', threadId: 't', kind: 'tool_call', toolName: 'Agent', parentId: 'outer' }),
    makeItem({ id: 'match', threadId: 't', parentId: 'inner' }),
  ];
  setBindingMock('GetThreadItem', async (_thread: string, id: string) => source.find(item => item.id === id));
  const open = vi.spyOn(pane, 'openAgentPane').mockImplementation(() => {});
  const scroll = vi.spyOn(pane, 'requestScrollToItem');
  await navigateToThreadItem(pane, 'match');
  expect(open).toHaveBeenCalledWith('inner', expect.any(String));
  const state = agentStateForPane(pane.paneId, 't');
  expect(state.breadcrumb.map(entry => entry.itemId)).toEqual(['', 'outer', 'inner']);
  expect(state.itemRequest?.itemId).toBe('match');
  expect(scroll).not.toHaveBeenCalled();
  expect(pane.items.map(item => item.id)).toEqual(['main-tail']);
  pane.clear();
});

it('ignores a navigation response after switching threads', async () => {
  const pane = createThreadPane();
  installPaneMocks([]);
  await pane.switchThread(makeThread({ id: 'before' }));
  let resolve!: (item: unknown) => void;
  setBindingMock('GetThreadItem', () => new Promise(done => { resolve = done; }));
  const navigate = navigateToThreadItem(pane, 'late');
  await vi.waitFor(() => expect(resolve).toBeTypeOf('function'));
  await pane.switchThread(makeThread({ id: 'after' }));
  resolve(makeItem({ id: 'late', threadId: 'before' }));
  await navigate;
  expect(pane.scrollToItemRequest.itemId).toBe('');
  pane.clear();
});

it.each(['resolve', 'reject'] as const)('keeps the newer search selection when an older lookup %ss late', async outcome => {
  const pane = createThreadPane();
  installPaneMocks([makeItem({ id: 'newer', threadId: 't' })]);
  await pane.switchThread(makeThread({ id: 't' }));
  let resolve!: (item: unknown) => void;
  let reject!: (error: Error) => void;
  setBindingMock('GetThreadItem', () => new Promise((done, fail) => { resolve = done; reject = fail; }));
  const toastCount = getToasts().length;
  const first = navigateToThreadItem(pane, 'older');
  await navigateToThreadItem(pane, 'newer');
  if (outcome === 'resolve') resolve(makeItem({ id: 'older', threadId: 't' }));
  else reject(new Error('superseded lookup failed'));
  await first;
  expect(pane.scrollToItemRequest.itemId).toBe('newer');
  expect(getToasts()).toHaveLength(toastCount);
  pane.clear();
});
