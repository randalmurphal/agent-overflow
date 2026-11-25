import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  acquireTrackerConnections,
  addTrackerConnection,
  applyTrackerConnectionsChanged,
  openTrackerTokenPage,
  removeTrackerConnection,
  setTrackerConnectionEnabled,
  trackerConnectionsOf,
  trackerReadGate,
  verifyTrackerConnection,
} from './trackerConnections.svelte';
import { setupEventListeners } from './events';
import { applyTransportGap } from './eventsTransportGap';
import { __setBackendStatusForTest } from './transportStatus.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { deferred } from '../../test/helpers/providerAccounts';
import { REMOTE_BACKEND_UUID, stageBackend } from '../../test/helpers/backends';
import { trackerConnection } from '../../test/helpers/trackerConnections';
import { SCOPES_AT_V0_0_19, answerAsV0019 } from '../../test/helpers/olderComputer';
import { pairWithScopes, resetToLocalPage } from '../../test/helpers/scopes';
import { __resetBackendsForTest, getPinnedBackend } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { setCarriedSessionScopes } from '../transport/scopes';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import type { TrackerConnection } from './bindings';

/** Answers TrackerConnections with each next list in turn, recording the computer each read went to. */
function answerReads(...lists: TrackerConnection[][]) {
  const targets: (BackendKey | null)[] = [];
  let i = 0;
  const mock = setBindingMock('TrackerConnections', async () => {
    targets.push(getPinnedBackend());
    const list = lists[Math.min(i, lists.length - 1)];
    i += 1;
    return list;
  });
  return { mock, targets };
}

const flush = async (): Promise<void> => {
  for (let i = 0; i < 6; i += 1) await Promise.resolve();
};

describe('a held connection list', () => {
  it('is read when a surface holds it, pinned to its computer, and dropped by the last release', async () => {
    const { mock, targets } = answerReads([trackerConnection()]);
    const release = acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(trackerConnectionsOf(HOME_BACKEND).loaded).toBe(true));
    expect(trackerConnectionsOf(HOME_BACKEND).list.map((c) => c.id)).toEqual(['jira-1']);
    expect(targets).toEqual([HOME_BACKEND]);

    // A second holder shares the list and reads nothing more.
    const second = acquireTrackerConnections(HOME_BACKEND);
    await flush();
    expect(mock).toHaveBeenCalledTimes(1);
    release();
    expect(trackerConnectionsOf(HOME_BACKEND).loaded).toBe(true);
    second();
    second();
    expect(trackerConnectionsOf(HOME_BACKEND)).toEqual({ list: [], loaded: false, error: null, answer: 'none' });

    // Nobody holds it: a nudge reads nothing.
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await flush();
    expect(mock).toHaveBeenCalledTimes(1);
  });

  it('answers a burst of nudges with one read that starts after the one in flight', async () => {
    const first = deferred<TrackerConnection[]>();
    const second = deferred<TrackerConnection[]>();
    const answers = [first, second];
    const read = setBindingMock('TrackerConnections', () => answers.shift()!.promise);
    acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(1));

    applyTrackerConnectionsChanged(HOME_BACKEND);
    applyTrackerConnectionsChanged(HOME_BACKEND);
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await flush();
    expect(read).toHaveBeenCalledTimes(1);

    first.resolve([trackerConnection({ verifiedName: 'old' })]);
    await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    expect(trackerConnectionsOf(HOME_BACKEND).list[0].verifiedName).toBe('old');
    second.resolve([trackerConnection({ verifiedName: 'User 1' })]);
    await vi.waitFor(() => expect(trackerConnectionsOf(HOME_BACKEND).list[0].verifiedName).toBe('User 1'));
    await flush();
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('settles an action only once a read that started after its reply shows the result', async () => {
    const inFlight = deferred<TrackerConnection[]>();
    const answers: Promise<TrackerConnection[]>[] = [
      Promise.resolve([trackerConnection({ verifiedAt: 0, verifiedName: '' })]),
      inFlight.promise,
      Promise.resolve([trackerConnection()]),
    ];
    const read = setBindingMock('TrackerConnections', () => answers.shift()!);
    acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(trackerConnectionsOf(HOME_BACKEND).loaded).toBe(true));

    // A nudge's read starts before the action replies and answers the old row.
    applyTrackerConnectionsChanged(HOME_BACKEND);
    await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    setBindingMock('VerifyTrackerConnection', async () => trackerConnection());
    let settled = false;
    const verify = verifyTrackerConnection(HOME_BACKEND, 'jira-1').then(() => {
      settled = true;
    });
    await flush();
    expect(settled).toBe(false);

    inFlight.resolve([trackerConnection({ verifiedAt: 0, verifiedName: '' })]);
    await verify;
    expect(read).toHaveBeenCalledTimes(3);
    expect(trackerConnectionsOf(HOME_BACKEND).list[0].verifiedName).toBe('User 1');
  });

  it('keeps the last list over a dropped connection and shows any other failure until a read lands', async () => {
    const answers: (() => Promise<TrackerConnection[]>)[] = [
      async () => [trackerConnection()],
      async () => { throw new DisconnectedError('socket closed'); },
      async () => { throw new TransportError('method_error', 'store: database is locked'); },
      async () => [trackerConnection({ label: 'Work' })],
    ];
    setBindingMock('TrackerConnections', () => answers.shift()!());
    acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(trackerConnectionsOf(HOME_BACKEND).loaded).toBe(true));

    await applyAndSettle();
    expect(trackerConnectionsOf(HOME_BACKEND).list).toHaveLength(1);
    expect(trackerConnectionsOf(HOME_BACKEND).error).toBeNull();

    await applyAndSettle();
    expect(trackerConnectionsOf(HOME_BACKEND).list).toHaveLength(1);
    expect(trackerConnectionsOf(HOME_BACKEND).error).toBeInstanceOf(TransportError);

    await applyAndSettle();
    expect(trackerConnectionsOf(HOME_BACKEND).error).toBeNull();
    expect(trackerConnectionsOf(HOME_BACKEND).list[0].label).toBe('Work');
  });

  it('shows a failed first read as a failure, never as an empty list, a dropped connection included', async () => {
    const answers: (() => Promise<TrackerConnection[]>)[] = [
      async () => { throw new DisconnectedError('socket closed'); },
      async () => [trackerConnection()],
    ];
    setBindingMock('TrackerConnections', () => answers.shift()!());
    acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(trackerConnectionsOf(HOME_BACKEND).error).toBeInstanceOf(DisconnectedError));
    expect(trackerConnectionsOf(HOME_BACKEND).loaded).toBe(false);
    await applyAndSettle();
    expect(trackerConnectionsOf(HOME_BACKEND)).toMatchObject({ loaded: true, error: null });
    expect(trackerConnectionsOf(HOME_BACKEND).list).toHaveLength(1);
  });

  it('reads as empty, and says so, on a computer whose release has no trackers', async () => {
    answerAsV0019();
    acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(trackerConnectionsOf(HOME_BACKEND).loaded).toBe(true));
    expect(trackerConnectionsOf(HOME_BACKEND)).toMatchObject({ list: [], error: null, answer: 'older' });
    expect(trackerReadGate(trackerConnectionsOf(HOME_BACKEND))).toBe('older');
  });

  // A phone's packaged bundle can be newer than its home computer: the
  // page's own computer is asked as any other, its grant regardless.
  it('says a home computer on v0.0.19 lacks trackers, with the grants that release gives', async () => {
    await pairWithScopes(SCOPES_AT_V0_0_19);
    try {
      answerAsV0019();
      acquireTrackerConnections(HOME_BACKEND);
      await vi.waitFor(() => expect(trackerReadGate(trackerConnectionsOf(HOME_BACKEND))).toBe('older'));
    } finally {
      resetToLocalPage();
    }
  });

  it('says a typed refusal is a refusal, and asks no more until the computer connects again', async () => {
    await pairWithScopes(['threads:read']);
    try {
      const read = setBindingMock('TrackerConnections', async () => {
        throw new TransportError('scope_required', 'tracker:read required', { scope: 'tracker:read' });
      });
      acquireTrackerConnections(HOME_BACKEND);
      await vi.waitFor(() => expect(trackerReadGate(trackerConnectionsOf(HOME_BACKEND))).toBe('refused'));
      expect(trackerConnectionsOf(HOME_BACKEND)).toMatchObject({ list: [], error: null });
      expect(read).toHaveBeenCalledTimes(1);
      window.dispatchEvent(new Event('focus'));
      await applyAndSettle();
      expect(read).toHaveBeenCalledTimes(1);
      __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null } as never);
      __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null } as never);
      await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    } finally {
      resetToLocalPage();
    }
  });
});

async function applyAndSettle(): Promise<void> {
  applyTrackerConnectionsChanged(HOME_BACKEND);
  await flush();
  await flush();
}

describe('what makes a held list read again', () => {
  let cancel: (() => void) | null = null;
  afterEach(() => {
    cancel?.();
    cancel = null;
  });

  it('tracker:connections_updated from the computer, and a gap on that channel', async () => {
    cancel = setupEventListeners();
    const { mock } = answerReads([]);
    acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(mock).toHaveBeenCalledTimes(1));

    emitWailsEvent('tracker:connections_updated', null);
    await vi.waitFor(() => expect(mock).toHaveBeenCalledTimes(2));

    applyTransportGap({ channel: 'tracker:connections_updated', seq: 0 } as never, { backendId: '' } as never);
    await vi.waitFor(() => expect(mock).toHaveBeenCalledTimes(3));
  });

  it('the window coming back, only while a list is held', async () => {
    const { mock } = answerReads([]);
    const release = acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(mock).toHaveBeenCalledTimes(1));

    window.dispatchEvent(new Event('focus'));
    await vi.waitFor(() => expect(mock).toHaveBeenCalledTimes(2));

    release();
    window.dispatchEvent(new Event('focus'));
    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    expect(mock).toHaveBeenCalledTimes(2);
  });

  it('the computer connecting again', async () => {
    const { mock } = answerReads([]);
    acquireTrackerConnections(HOME_BACKEND);
    await vi.waitFor(() => expect(mock).toHaveBeenCalledTimes(1));
    __setBackendStatusForTest(HOME_BACKEND, { status: 'reconnecting', nextAttemptAt: null } as never);
    await flush();
    expect(mock).toHaveBeenCalledTimes(1);
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null } as never);
    await vi.waitFor(() => expect(mock).toHaveBeenCalledTimes(2));
  });
});

describe('two computers', () => {
  beforeEach(() => {
    stageBackend({ id: 'laptop', backendId: REMOTE_BACKEND_UUID });
    setCarriedSessionScopes('laptop', ['tracker:read', 'tracker:operate']);
  });
  afterEach(() => {
    __resetBackendsForTest();
  });

  it('keeps each computer its own list and sends each call to the computer it names', async () => {
    const read = setBindingMock('TrackerConnections', async () =>
      getPinnedBackend() === 'laptop' ? [trackerConnection({ id: 'lin-1', kind: 'linear', site: 'example' })] : [trackerConnection()],
    );
    acquireTrackerConnections(HOME_BACKEND);
    acquireTrackerConnections('laptop');
    await vi.waitFor(() => expect(trackerConnectionsOf('laptop').loaded).toBe(true));
    await vi.waitFor(() => expect(trackerConnectionsOf(HOME_BACKEND).loaded).toBe(true));
    expect(trackerConnectionsOf(HOME_BACKEND).list.map((c) => c.id)).toEqual(['jira-1']);
    expect(trackerConnectionsOf('laptop').list.map((c) => c.id)).toEqual(['lin-1']);

    const targets: (BackendKey | null)[] = [];
    const record = (answer: unknown) => async () => {
      targets.push(getPinnedBackend());
      return answer;
    };
    setBindingMock('SetTrackerConnectionEnabled', record(trackerConnection({ id: 'lin-1', enabled: false })));
    setBindingMock('RemoveTrackerConnection', record(undefined));
    setBindingMock('OpenTrackerTokenPage', record(undefined));
    setBindingMock('AddTrackerConnection', record(trackerConnection({ id: 'tempo-1', kind: 'tempo' })));
    await setTrackerConnectionEnabled('laptop', 'lin-1', false);
    await removeTrackerConnection('laptop', 'lin-1');
    await openTrackerTokenPage('laptop', 'lin-1');
    await addTrackerConnection(HOME_BACKEND, { kind: 'tempo', site: '', parentId: 'jira-1' });
    expect(targets).toEqual(['laptop', 'laptop', 'laptop', HOME_BACKEND]);
    // Each action read its own computer again, and only that one.
    const readTargets = read.mock.calls.length;
    expect(readTargets).toBe(2 + 3);
  });

  // v0.0.19 has no tracker scopes, so its grant never holds tracker:read,
  // and it answers a method it lacks before it checks a grant.
  it('says a computer on v0.0.19 lacks trackers, with the grants that release gives', async () => {
    setCarriedSessionScopes('laptop', SCOPES_AT_V0_0_19);
    answerAsV0019();
    acquireTrackerConnections('laptop');
    await vi.waitFor(() => expect(trackerConnectionsOf('laptop').loaded).toBe(true));
    expect(trackerConnectionsOf('laptop')).toMatchObject({ answer: 'older', list: [], error: null });
  });

  // Its grant, cached from an earlier session, holds no tracker scope; the
  // failed ask says nothing about whether it may.
  it('shows an unreachable computer whose grant lacks tracker:read as a failure, never as a refusal', async () => {
    setCarriedSessionScopes('laptop', SCOPES_AT_V0_0_19);
    setBindingMock('TrackerConnections', async () => { throw new DisconnectedError('socket closed'); });
    acquireTrackerConnections('laptop');
    await vi.waitFor(() => expect(trackerConnectionsOf('laptop').error).toBeInstanceOf(DisconnectedError));
    expect(trackerConnectionsOf('laptop')).toMatchObject({ loaded: false, answer: 'none' });
    expect(trackerReadGate(trackerConnectionsOf('laptop'))).toBe('failed');
  });

  it('keeps a current computer’s refusal of tracker:read a refusal', async () => {
    setCarriedSessionScopes('laptop', ['threads:read']);
    setBindingMock('TrackerConnections', async () => {
      throw new TransportError('scope_required', 'tracker:read required', { scope: 'tracker:read' });
    });
    acquireTrackerConnections('laptop');
    await vi.waitFor(() => expect(trackerConnectionsOf('laptop').loaded).toBe(true));
    expect(trackerConnectionsOf('laptop')).toMatchObject({ answer: 'refused', list: [], error: null });
  });

  it('drops a computer’s list when it is detached', async () => {
    answerReads([trackerConnection()]);
    acquireTrackerConnections('laptop');
    await vi.waitFor(() => expect(trackerConnectionsOf('laptop').loaded).toBe(true));
    __resetBackendsForTest();
    expect(trackerConnectionsOf('laptop').loaded).toBe(false);
  });
});
