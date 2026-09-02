// The per-machine dev-server store: one box per attached backend fed by
// `devserver:list` keyed on the frame's origin, read on every hello the
// session holds `preview:open` for, and the two `access:admin` mutations
// that reconcile against the next frame rather than optimistically.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import {
  emitWailsEvent,
  resetWailsMocks,
  wailsListenerCount,
} from '../../test/mocks/wailsio-runtime';
import {
  grantBackendScopes,
  pairViewOnly,
  pairWithScopes,
  resetToLocalPage,
  revokeBackendScopes,
} from '../../test/helpers/scopes';
import { expectCleanTransitions } from '../../test/helpers/transitions';
import {
  REMOTE_BACKEND_UUID,
  resetStagedBackends,
  stageBackend,
} from '../../test/helpers/backends';
import { SCOPES } from '../transport/scopes';
import { HOME_BACKEND } from '../transport/backendKey';
import { TransportError } from '../transport/wsClient';
import { __resetScopesForTest } from '../transport/scopes';
import type { DevServer, DevServerList } from './bindings';
import {
  allowPreviewPort,
  allowedPreviewPorts,
  disallowPreviewPort,
  initDevServers,
  loadDevServers,
  machineDevServers,
  openPreview,
  previewFor,
  previewSignature,
  resetDevServersForTest,
} from './devServers.svelte';

function server(port: number, overrides: Partial<DevServer> = {}): DevServer {
  return {
    port,
    allowed: true,
    source: 'attributed',
    listening: true,
    ...overrides,
  };
}

function list(overrides: Partial<DevServerList> = {}): DevServerList {
  return { servers: [server(5173)], previewHost: 'desk.tail.ts.net', ...overrides };
}

const tick = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

const LAPTOP_HELLO = {
  protocolVersion: 1,
  capabilities: [],
  backendId: REMOTE_BACKEND_UUID,
  backendName: 'laptop',
  serverTimeMs: 0,
  clockSkewMs: 0,
  bundleId: '',
} as never;

describe('devServers store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWailsMocks();
    resetDevServersForTest();
    resetStagedBackends();
    resetToLocalPage();
  });

  afterEach(() => {
    resetDevServersForTest();
    resetStagedBackends();
    revokeBackendScopes('laptop');
    resetToLocalPage();
    __resetScopesForTest();
  });

  describe('the passive load', () => {
    it('reads the list on the owner’s own screen and keeps it per machine', async () => {
      const read = setBindingMock('GetDevServers', async () => list());
      await loadDevServers(HOME_BACKEND);
      expect(read).toHaveBeenCalledTimes(1);
      expect(machineDevServers(HOME_BACKEND).list?.previewHost).toBe('desk.tail.ts.net');
      expect(machineDevServers('laptop').list).toBeNull();
    });

    it('issues nothing from a view-only session, which holds no execute scope', async () => {
      const read = setBindingMock('GetDevServers', async () => list());
      await pairViewOnly();
      await loadDevServers(HOME_BACKEND);
      expect(read).not.toHaveBeenCalled();
      expect(machineDevServers(HOME_BACKEND).list).toBeNull();
      expect(machineDevServers(HOME_BACKEND).loadError).toBe('');
    });

    it('reads it for a device paired with full access', async () => {
      const read = setBindingMock('GetDevServers', async () => list());
      await pairWithScopes(SCOPES);
      await loadDevServers(HOME_BACKEND);
      expect(read).toHaveBeenCalledTimes(1);
    });

    it('is silent on a refusal and on an older backend, and loud on anything else', async () => {
      setBindingMock('GetDevServers', async () => {
        throw new TransportError('method_not_found', 'no such method');
      });
      await loadDevServers(HOME_BACKEND);
      expect(machineDevServers(HOME_BACKEND).loadError).toBe('');
      expect(machineDevServers(HOME_BACKEND).loading).toBe(false);

      setBindingMock('GetDevServers', async () => {
        throw new TransportError('scope_required', 'refused', undefined, 'preview:open');
      });
      await loadDevServers(HOME_BACKEND);
      expect(machineDevServers(HOME_BACKEND).loadError).toBe('');

      setBindingMock('GetDevServers', async () => {
        throw new Error('socket closed');
      });
      await loadDevServers(HOME_BACKEND);
      expect(machineDevServers(HOME_BACKEND).loadError).toBe('Socket closed.');
    });
  });

  describe('the channel', () => {
    it('keys a list frame by the backend it arrived from', () => {
      initDevServers();
      stageBackend();
      emitWailsEvent('devserver:list', list({ previewHost: 'desk.tail.ts.net' }));
      emitWailsEvent(
        'devserver:list',
        list({ servers: [server(3000)], previewHost: 'laptop.tail.ts.net' }),
        REMOTE_BACKEND_UUID,
      );
      expect(machineDevServers(HOME_BACKEND).list?.previewHost).toBe('desk.tail.ts.net');
      expect(previewFor(HOME_BACKEND, 5173).kind).toBe('open');
      expect(previewFor('laptop', 5173).kind).toBe('not-shared');
      expect(previewFor('laptop', 3000).kind).toBe('open');
    });

    it('replaces the list wholesale rather than merging rows', () => {
      initDevServers();
      emitWailsEvent('devserver:list', list({ servers: [server(5173), server(3000)] }));
      expect(allowedPreviewPorts(HOME_BACKEND)).toEqual([5173, 3000]);
      emitWailsEvent('devserver:list', list({ servers: [server(3000)] }));
      expect(allowedPreviewPorts(HOME_BACKEND)).toEqual([3000]);
    });

    it('subscribes once and tears down', () => {
      const stop = initDevServers();
      initDevServers();
      expect(wailsListenerCount('devserver:list')).toBe(1);
      stop();
      expect(wailsListenerCount('devserver:list')).toBe(0);
    });
  });

  describe('the answer a link reads', () => {
    it('is armed only once a machine has answered', () => {
      initDevServers();
      expect(previewSignature(HOME_BACKEND)).toBe('');
      emitWailsEvent('devserver:list', list());
      expect(previewSignature(HOME_BACKEND)).not.toBe('');
    });

    it('moves on a newly shared port and stands still on a frame that says the same thing', () => {
      initDevServers();
      emitWailsEvent('devserver:list', list());
      const first = previewSignature(HOME_BACKEND);
      emitWailsEvent('devserver:list', list({ servers: [server(5173, { pid: 42 })] }));
      expect(previewSignature(HOME_BACKEND)).toBe(first);
      emitWailsEvent('devserver:list', list({ servers: [server(5173), server(3000)] }));
      expect(previewSignature(HOME_BACKEND)).not.toBe(first);
    });

    it('says no-address before it says not-shared, because allowing a port there changes nothing', () => {
      initDevServers();
      emitWailsEvent('devserver:list', list({ previewHost: '' }));
      expect(previewFor(HOME_BACKEND, 5173).kind).toBe('no-address');
      expect(previewFor(HOME_BACKEND, 9999).kind).toBe('no-address');
    });

    it('treats a listener that is merely seen as not shared', () => {
      initDevServers();
      emitWailsEvent(
        'devserver:list',
        list({ servers: [server(5173, { allowed: false, source: 'seen' })] }),
      );
      expect(previewFor(HOME_BACKEND, 5173).kind).toBe('not-shared');
      expect(allowedPreviewPorts(HOME_BACKEND)).toEqual([]);
    });
  });

  describe('the hello edge', () => {
    it('reads every attached machine on init and again on each hello', async () => {
      const read = setBindingMock('GetDevServers', async () => list());
      const laptop = stageBackend({ hello: LAPTOP_HELLO });
      await grantBackendScopes('laptop', SCOPES);
      initDevServers();
      await tick();
      expect(read).toHaveBeenCalledTimes(1);
      laptop.setHello(null);
      laptop.setHello(LAPTOP_HELLO);
      await tick();
      expect(read).toHaveBeenCalledTimes(2);
    });

    it('reads nothing off a machine attached without the grant', async () => {
      const read = setBindingMock('GetDevServers', async () => list());
      stageBackend({ hello: LAPTOP_HELLO });
      initDevServers();
      await tick();
      expect(read).not.toHaveBeenCalled();
      expect(machineDevServers('laptop').list).toBeNull();
    });

    it('keeps a machine’s list through a dropped socket, and forgets it on detach', async () => {
      setBindingMock('GetDevServers', async () => list());
      const laptop = stageBackend({ hello: LAPTOP_HELLO });
      await grantBackendScopes('laptop', SCOPES);
      initDevServers();
      await tick();
      expect(previewFor('laptop', 5173).kind).toBe('open');
      laptop.setHello(null);
      expect(previewFor('laptop', 5173).kind).toBe('open');
      resetStagedBackends();
      expect(machineDevServers('laptop').list).toBeNull();
      expect(previewSignature('laptop')).toBe('');
    });
  });

  describe('allowing a port', () => {
    it('sends it to that machine and waits for the frame rather than applying it here', async () => {
      const allow = setBindingMock('AllowPreviewPort', async () => undefined);
      initDevServers();
      emitWailsEvent('devserver:list', list({ servers: [] }));
      await allowPreviewPort(HOME_BACKEND, 3000);
      expect(allow).toHaveBeenCalledWith(3000);
      expect(previewFor(HOME_BACKEND, 3000).kind).toBe('not-shared');
      emitWailsEvent('devserver:list', list({ servers: [server(3000)] }));
      expect(previewFor(HOME_BACKEND, 3000).kind).toBe('open');
    });

    it('issues nothing without access:admin, and nothing for a port off the range', async () => {
      setBindingMock('AllowPreviewPort', async () => undefined);
      await pairViewOnly();
      await allowPreviewPort(HOME_BACKEND, 3000);
      expect(getBindingMock('AllowPreviewPort')).not.toHaveBeenCalled();

      resetToLocalPage();
      await allowPreviewPort(HOME_BACKEND, 0);
      await allowPreviewPort(HOME_BACKEND, 65536);
      await allowPreviewPort(HOME_BACKEND, 1.5);
      expect(getBindingMock('AllowPreviewPort')).not.toHaveBeenCalled();
    });

    it('surfaces a refusal as user-facing state', async () => {
      setBindingMock('AllowPreviewPort', async () => {
        throw new Error('the port is already bound beyond loopback');
      });
      await allowPreviewPort(HOME_BACKEND, 3000);
      expect(machineDevServers(HOME_BACKEND).actionError).toBe(
        'The port is already bound beyond loopback.',
      );
    });

    it('stops sharing through the other half of the pair', async () => {
      const disallow = setBindingMock('DisallowPreviewPort', async () => undefined);
      await disallowPreviewPort(HOME_BACKEND, 5173);
      expect(disallow).toHaveBeenCalledWith(5173);
    });
  });

  describe('opening a preview', () => {
    it('mints the URL for the thread and opens it through the external-open path', async () => {
      const mint = setBindingMock('MintPreviewURL', async () => 'https://desk.tail.ts.net:5173/app');
      // The one external-open wrapper, which on a loopback page hands the
      // URL to the host binding rather than to `window.open`.
      const open = setBindingMock('OpenExternalURL', async () => undefined);
      await openPreview('thread-1', 5173, '/app');
      expect(mint).toHaveBeenCalledWith('thread-1', 5173, '/app');
      expect(open).toHaveBeenCalledWith('https://desk.tail.ts.net:5173/app');
    });

    it('surfaces a spent or refused mint rather than opening nothing', async () => {
      setBindingMock('MintPreviewURL', async () => {
        throw new Error('no preview listener for that port');
      });
      await openPreview('thread-1', 5173, '/');
      expect(machineDevServers(HOME_BACKEND).actionError).toBe(
        'No preview listener for that port.',
      );
    });
  });

  it('survives being started and stopped twice', () => {
    setBindingMock('GetDevServers', async () => list());
    expectCleanTransitions('devServers subscription', {
      on: () => initDevServers(),
      off: (handle) => handle?.(),
      whileOn: () => {
        expect(wailsListenerCount('devserver:list')).toBe(1);
      },
      onAgain: () => {
        // A second init shares the one subscription rather than adding a
        // second: two listeners would apply every frame twice.
        initDevServers();
        expect(wailsListenerCount('devserver:list')).toBe(1);
      },
      inFlight: () => {
        emitWailsEvent('devserver:list', list());
        expect(previewSignature(HOME_BACKEND)).not.toBe('');
      },
      // The list itself deliberately survives a teardown, exactly as it
      // survives a dropped socket, so it is not in the resting state.
      // What must not leak is the subscription.
      read: () => ({ listeners: wailsListenerCount('devserver:list') }),
    });
  });
});
