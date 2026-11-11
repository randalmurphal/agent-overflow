// The thread find host: it offers find only for an owner that serves the
// find contract, re-reads the open find when the transcript's text changes,
// and drops the hits and searches again across a same-thread reload.
import { afterEach, beforeEach, expect, it } from 'vitest';
import { render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import ThreadPaneFind from './ThreadPaneFind.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  getPaneFindProvider,
  hasPaneFindProvider,
  openPaneFind,
  resetPaneFindForTest,
  setPaneFindQuery,
} from '../../stores/paneFind.svelte';
import { resetPaneLayoutForTest } from '../../stores/paneLayout.svelte';
import { __setTransportHelloForTest } from '../../stores/transportStatus.svelte';
import type { ThreadPane } from '../../stores/thread.svelte';

function hello(capabilities: string[]) {
  __setTransportHelloForTest({
    protocolVersion: 1, capabilities, backendId: '', backendName: '',
    serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0,
  });
}

function wireHit(itemId: string) {
  return {
    ownershipEpoch: 0, threadId: 'thread-1', threadTitle: 'T', provider: 'claude', itemId,
    turnIndex: 1, itemIndex: 0, itemKind: 'assistant_text', itemRole: 'assistant', summary: 's', matchType: 'item',
  };
}

function mount(pane: ThreadPane) {
  return render(ThreadPaneFind, {
    props: { paneId: 'main', title: 'Thread', timeline: pane, scope: { threadId: 'thread-1' }, root: null },
  });
}

beforeEach(() => {
  resetBindingMocks();
  resetPaneFindForTest();
  resetPaneLayoutForTest();
});

afterEach(() => {
  resetPaneFindForTest();
  __setTransportHelloForTest(null);
});

it('offers find only while the owner serves the find contract', async () => {
  hello(['timeline.scopes.v1']);
  const pane = await buildPane(makeThread({ id: 'thread-1' }));
  mount(pane);
  flushSync();
  expect(hasPaneFindProvider('main')).toBe(false);
  hello(['timeline.scopes.v1', 'thread-find.v1']);
  flushSync();
  expect(hasPaneFindProvider('main')).toBe(true);
  hello(['timeline.scopes.v1']);
  flushSync();
  expect(hasPaneFindProvider('main')).toBe(false);
});

it('re-reads the open find when a row text changes without a structural change', async () => {
  hello(['timeline.scopes.v1', 'thread-find.v1']);
  const row = makeItem({ id: 'a', threadId: 'thread-1', turnIndex: 1, kind: 'assistant_text', role: 'assistant', summary: 'plain' });
  const pane = await buildPane(makeThread({ id: 'thread-1' }), [row]);
  const search = setBindingMock('SearchThreadItems', async () => []);
  mount(pane);
  openPaneFind('main');
  setPaneFindQuery('main', 'needle');
  await waitFor(() => expect(search).toHaveBeenCalledTimes(1));
  const structural = pane.timelineRevision;

  search.mockImplementation(async () => [wireHit('a')]);
  pane.upsertItem({ ...row, summary: 'plain with needle', updatedAt: row.updatedAt + 1 });
  expect(pane.timelineRevision).toBe(structural);
  await waitFor(() => expect(search).toHaveBeenCalledTimes(2), { timeout: 3000 });
  await waitFor(() => expect(getPaneFindProvider('main')?.result.count).toBe(1));
});

it('a same-thread reload drops the hits and searches again', async () => {
  hello(['timeline.scopes.v1', 'thread-find.v1']);
  const thread = makeThread({ id: 'thread-1' });
  const pane = await buildPane(thread);
  const search = setBindingMock('SearchThreadItems', async () => [wireHit('a')]);
  mount(pane);
  openPaneFind('main');
  setPaneFindQuery('main', 'needle');
  await waitFor(() => expect(getPaneFindProvider('main')?.result.count).toBe(1));
  let finish!: () => void;
  search.mockImplementation(() => new Promise((resolve) => { finish = () => resolve([]); }));
  await pane.switchThread(thread);
  flushSync();
  expect(getPaneFindProvider('main')?.result).toMatchObject({ count: 0, pending: true });
  await waitFor(() => expect(search).toHaveBeenCalledTimes(2));
  finish();
  await waitFor(() => expect(getPaneFindProvider('main')?.result.pending).toBe(false));
});
