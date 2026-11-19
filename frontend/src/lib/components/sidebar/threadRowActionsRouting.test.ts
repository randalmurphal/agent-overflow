// The sidebar's archive and its "Archive again" retry through the real
// transport router with two computers attached. A worker the archive left
// open is a thread this client never listed, so only the computer the
// first archive went to can say where the retry belongs.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { Call } from '../../transport/runtime';
import { wsClient, type WSClient } from '../../transport/wsClient';
import { __attachBackendForTest, __resetBackendsForTest } from '../../transport/backends';
import { HOME_DESCRIPTOR } from '../../transport/manifestBackends';
import { __resetEntityIndexForTest, forgetThread, noteThread } from '../../transport/entityIndex';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { makeThread } from '../../../test/helpers/chat';
import { archiveThreadAction, type ThreadActionCtx } from './threadRowActions';

const REMOTE = 'remote-mac';
const ARCHIVE_THREAD = 3655608409;

function client(callByID: WSClient['callByID']): WSClient {
  return {
    callByID,
    callByName: vi.fn(),
    subscribe: vi.fn(() => () => undefined),
    installStepUpProver: vi.fn(), setWatchedThreads: vi.fn(), setPresence: vi.fn(), setLease: vi.fn(),
    getStatus: vi.fn(() => ({ status: 'connected', nextAttemptAt: null })),
    onReplay: vi.fn(() => () => undefined),
    onStatusChange: vi.fn(() => () => undefined), getHello: vi.fn(() => null),
    onHelloChange: vi.fn(() => () => undefined), close: vi.fn(),
  } as unknown as WSClient;
}

beforeEach(() => {
  __resetBackendsForTest();
  __resetEntityIndexForTest();
  for (const toast of getToasts()) removeToast(toast.id);
  setBindingMock('ArchiveThread', (id: unknown) => Call.ByID(ARCHIVE_THREAD, id));
});

afterEach(() => {
  vi.restoreAllMocks();
  __resetBackendsForTest();
  __resetEntityIndexForTest();
  __attachBackendForTest(HOME_DESCRIPTOR, wsClient);
});

it('archives again a worker the archive left open on the computer the archive went to', async () => {
  const home = vi.fn(async () => {
    throw new Error('The requested item no longer exists.');
  });
  let first = true;
  const remote = vi.fn(async (method: number, args: unknown[]) => {
    expect(method).toBe(ARCHIVE_THREAD);
    if (args[0] === 'owner' && first) {
      first = false;
      // The archive's frame takes the owner out of this client before the
      // answer arrives, so nothing read after the call can name its computer.
      forgetThread('owner');
      return { workersNotArchived: [{ threadId: 'worker', title: 'Parser review', error: 'thread is preparing a fork' }] };
    }
    return {};
  });
  __attachBackendForTest(HOME_DESCRIPTOR, client(home));
  __attachBackendForTest({ id: REMOTE, backendId: '99887766-5544-4333-8222-111111111111', name: 'Mac', wsUrl: '/ws/mac', bootstrapUrl: '/bootstrap/mac' }, client(remote));
  // Only the owner is known; the worker was never listed here.
  noteThread('owner', REMOTE);
  const ctx: ThreadActionCtx = {
    thread: makeThread({ id: 'owner', title: 'Owner' }),
    isActive: false,
    clearPane: vi.fn(),
    switchPane: vi.fn(async () => {}),
    reportError: vi.fn(),
  };

  await archiveThreadAction(ctx);
  expect(ctx.reportError).not.toHaveBeenCalled();
  const [toast] = getToasts();
  expect(toast.action?.label).toBe('Archive again');
  toast.action?.run();
  await vi.waitFor(() => expect(remote).toHaveBeenCalledWith(ARCHIVE_THREAD, ['worker']));
  expect(home).not.toHaveBeenCalled();
  expect(getToasts().filter((entry) => entry.type === 'error')).toEqual([]);
});
