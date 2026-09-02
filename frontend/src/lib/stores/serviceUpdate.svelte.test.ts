// The per-machine update store: one box per attached backend, fed by the
// status and outcome channels keyed on the frame's origin, read on every
// hello the session holds `access:admin` for, and the request that the
// backend answers with frames rather than a return value.

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
import {
  REMOTE_BACKEND_UUID,
  resetStagedBackends,
  stageBackend,
} from '../../test/helpers/backends';
import { SCOPES } from '../transport/scopes';
import { HOME_BACKEND } from '../transport/backendKey';
import { TransportError } from '../transport/wsClient';
import { __resetScopesForTest } from '../transport/scopes';
import {
  canInstallSelectedRelease,
  hasPendingServiceUpdate,
  initServiceUpdates,
  isServiceUpdateInFlight,
  loadMachineUpdate,
  loadServiceReleases,
  machineUpdate,
  requestServiceUpdate,
  resetServiceUpdatesForTest,
  selectServiceRelease,
  supervisedMachines,
  type ReleaseSummary,
  type ServiceUpdateStatus,
} from './serviceUpdate.svelte';

function status(overrides: Partial<ServiceUpdateStatus> = {}): ServiceUpdateStatus {
  return {
    supervised: true,
    available: true,
    currentVersion: '1.2.0',
    phase: 'idle',
    ...overrides,
  } as ServiceUpdateStatus;
}

function release(tag: string, overrides: Partial<ReleaseSummary> = {}): ReleaseSummary {
  return {
    tag,
    version: tag.replace(/^v/, ''),
    name: '',
    publishedAt: '',
    prerelease: false,
    isLatest: false,
    isCurrent: false,
    isOlder: false,
    ...overrides,
  } as ReleaseSummary;
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

describe('serviceUpdate store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWailsMocks();
    resetServiceUpdatesForTest();
    resetStagedBackends();
    resetToLocalPage();
  });

  afterEach(() => {
    resetServiceUpdatesForTest();
    resetStagedBackends();
    revokeBackendScopes('laptop');
    resetToLocalPage();
    __resetScopesForTest();
  });

  describe('the passive load', () => {
    it('reads the status on the owner’s own screen and keeps it per machine', async () => {
      const read = setBindingMock('GetServiceUpdateStatus', async () => status());
      await loadMachineUpdate(HOME_BACKEND);
      expect(read).toHaveBeenCalledTimes(1);
      expect(machineUpdate(HOME_BACKEND).status?.currentVersion).toBe('1.2.0');
      expect(machineUpdate('laptop').status).toBeNull();
    });

    it('issues nothing from a view-only session', async () => {
      const read = setBindingMock('GetServiceUpdateStatus', async () => status());
      await pairViewOnly();
      await loadMachineUpdate(HOME_BACKEND);
      expect(read).not.toHaveBeenCalled();
      expect(machineUpdate(HOME_BACKEND).status).toBeNull();
      expect(machineUpdate(HOME_BACKEND).loadError).toBe('');
    });

    it('reads it for a device paired with full access, which holds the scope', async () => {
      const read = setBindingMock('GetServiceUpdateStatus', async () => status());
      await pairWithScopes(SCOPES);
      await loadMachineUpdate(HOME_BACKEND);
      expect(read).toHaveBeenCalledTimes(1);
    });

    it('is silent when an older backend has no such method, and loud on any other failure', async () => {
      setBindingMock('GetServiceUpdateStatus', async () => {
        throw new TransportError('method_not_found', 'no such method');
      });
      await loadMachineUpdate(HOME_BACKEND);
      expect(machineUpdate(HOME_BACKEND).loadError).toBe('');

      setBindingMock('GetServiceUpdateStatus', async () => {
        throw new Error('socket closed');
      });
      await loadMachineUpdate(HOME_BACKEND);
      expect(machineUpdate(HOME_BACKEND).loadError).toBe('Socket closed.');
    });
  });

  describe('the channels', () => {
    it('keys a status frame by the backend it arrived from', () => {
      initServiceUpdates();
      stageBackend();
      emitWailsEvent('service:update-status', status({ currentVersion: '1.2.0' }));
      emitWailsEvent(
        'service:update-status',
        status({ currentVersion: '1.1.0', phase: 'downloading', written: 10, total: 100 }),
        REMOTE_BACKEND_UUID,
      );
      expect(machineUpdate(HOME_BACKEND).status?.currentVersion).toBe('1.2.0');
      expect(machineUpdate(HOME_BACKEND).status?.phase).toBe('idle');
      expect(machineUpdate('laptop').status?.phase).toBe('downloading');
      expect(machineUpdate('laptop').status?.written).toBe(10);
    });

    it('replaces the status wholesale rather than merging fields', () => {
      initServiceUpdates();
      emitWailsEvent('service:update-status', status({ phase: 'downloading', written: 10, total: 100 }));
      emitWailsEvent('service:update-status', status({ phase: 'idle' }));
      expect(machineUpdate(HOME_BACKEND).status?.written).toBeUndefined();
    });

    it('keeps the outcome beside the status, keyed the same way', () => {
      initServiceUpdates();
      emitWailsEvent(
        'service:update-outcome',
        { updateId: 'u-1', outcome: 'rolled-back', version: '1.3.0', reason: 'health check failed' },
        REMOTE_BACKEND_UUID,
      );
      // An unknown origin keys to home, so the frame above lands on the
      // laptop only once the laptop is attached.
      expect(machineUpdate(HOME_BACKEND).outcome?.updateId).toBe('u-1');
    });

    it('subscribes once and tears down', () => {
      const stop = initServiceUpdates();
      initServiceUpdates();
      expect(wailsListenerCount('service:update-status')).toBe(1);
      expect(wailsListenerCount('service:update-outcome')).toBe(1);
      stop();
      expect(wailsListenerCount('service:update-status')).toBe(0);
      expect(wailsListenerCount('service:update-outcome')).toBe(0);
    });
  });

  describe('the hello edge', () => {
    it('reads every attached machine on init and again on each hello', async () => {
      const read = setBindingMock('GetServiceUpdateStatus', async () => status());
      // The laptop is attached with an admin grant; the load asks for it
      // before it fires, so a machine attached view-only is never read.
      const laptop = stageBackend({ hello: LAPTOP_HELLO });
      await grantBackendScopes('laptop', SCOPES);
      initServiceUpdates();
      await tick();
      // Home has said nothing in this fixture (null hello), so only the
      // laptop, which has, is read at init.
      expect(read).toHaveBeenCalledTimes(1);
      laptop.setHello(null);
      laptop.setHello(LAPTOP_HELLO);
      await tick();
      expect(read).toHaveBeenCalledTimes(2);
    });

    it('reads nothing off a machine attached without the grant', async () => {
      const read = setBindingMock('GetServiceUpdateStatus', async () => status());
      stageBackend({ hello: LAPTOP_HELLO });
      initServiceUpdates();
      await tick();
      expect(read).not.toHaveBeenCalled();
      expect(machineUpdate('laptop').status).toBeNull();
    });

    it('keeps a machine’s box through a dropped socket, and forgets it on detach', async () => {
      setBindingMock('GetServiceUpdateStatus', async () => status({ phase: 'requested' }));
      const laptop = stageBackend({ hello: LAPTOP_HELLO });
      await grantBackendScopes('laptop', SCOPES);
      initServiceUpdates();
      await tick();
      expect(machineUpdate('laptop').status?.phase).toBe('requested');
      laptop.setHello(null);
      expect(machineUpdate('laptop').status?.phase).toBe('requested');
      resetStagedBackends();
      expect(machineUpdate('laptop').status).toBeNull();
    });
  });

  describe('the derived answers', () => {
    it('lists the attached machines whose backend reports a supervisor', () => {
      initServiceUpdates();
      stageBackend();
      expect(supervisedMachines()).toEqual([]);
      emitWailsEvent('service:update-status', status({ supervised: false }));
      emitWailsEvent('service:update-status', status(), REMOTE_BACKEND_UUID);
      expect(supervisedMachines()).toEqual(['laptop']);
    });

    it('lights the badge only for a supervised machine with a newer release', () => {
      initServiceUpdates();
      emitWailsEvent('service:update-status', status({ supervised: false, latestVersion: '9.0.0' }));
      expect(hasPendingServiceUpdate()).toBe(false);
      emitWailsEvent('service:update-status', status({ latestVersion: '' }));
      expect(hasPendingServiceUpdate()).toBe(false);
      emitWailsEvent('service:update-status', status({ latestVersion: '1.3.0', latestTag: 'v1.3.0' }));
      expect(hasPendingServiceUpdate()).toBe(true);
    });

    it('names the in-flight phases', () => {
      for (const p of ['resolving', 'downloading', 'verifying', 'staging', 'requested']) {
        expect(isServiceUpdateInFlight(p)).toBe(true);
      }
      for (const p of ['idle', 'error', '']) expect(isServiceUpdateInFlight(p)).toBe(false);
    });
  });

  describe('requesting', () => {
    it('sends the tag to that machine, clears the last outcome, and lets the frames drive the phase', async () => {
      initServiceUpdates();
      emitWailsEvent('service:update-status', status({ latestTag: 'v1.3.0', latestVersion: '1.3.0' }));
      emitWailsEvent('service:update-outcome', { updateId: 'u-0', outcome: 'committed', version: '1.2.0' });
      const request = setBindingMock('RequestServiceUpdate', async () => undefined);
      const p = requestServiceUpdate(HOME_BACKEND, 'v1.3.0');
      expect(machineUpdate(HOME_BACKEND).requesting).toBe(true);
      expect(machineUpdate(HOME_BACKEND).outcome).toBeNull();
      await p;
      expect(request).toHaveBeenCalledWith('v1.3.0');
      // The return value says nothing; the first frame does.
      expect(machineUpdate(HOME_BACKEND).requesting).toBe(true);
      emitWailsEvent('service:update-status', status({ phase: 'resolving', targetTag: 'v1.3.0' }));
      expect(machineUpdate(HOME_BACKEND).requesting).toBe(false);
      expect(machineUpdate(HOME_BACKEND).status?.phase).toBe('resolving');
    });

    it('shows a refusal and does not double-fire while a flow is on', async () => {
      initServiceUpdates();
      emitWailsEvent('service:update-status', status());
      const request = setBindingMock('RequestServiceUpdate', async () => {
        throw new Error('a trial boot is already mid-update');
      });
      await requestServiceUpdate(HOME_BACKEND, 'v1.3.0');
      expect(machineUpdate(HOME_BACKEND).requesting).toBe(false);
      expect(machineUpdate(HOME_BACKEND).requestError).toBe('A trial boot is already mid-update.');

      emitWailsEvent('service:update-status', status({ phase: 'downloading' }));
      await requestServiceUpdate(HOME_BACKEND, 'v1.3.0');
      expect(request).toHaveBeenCalledTimes(1);
    });
  });

  describe('the release picker', () => {
    it('loads per machine, defaults to the latest stable, and keeps a listed pick', async () => {
      setBindingMock('ListServiceReleases', async () => [
        release('v1.3.0', { isLatest: true }),
        release('v1.2.0', { isCurrent: true }),
        release('v1.1.0', { isOlder: true }),
      ]);
      await loadServiceReleases(HOME_BACKEND);
      expect(machineUpdate(HOME_BACKEND).releasesLoaded).toBe(true);
      expect(machineUpdate(HOME_BACKEND).selectedTag).toBe('v1.3.0');
      expect(machineUpdate('laptop').releasesLoaded).toBe(false);

      selectServiceRelease(HOME_BACKEND, 'v1.1.0');
      await loadServiceReleases(HOME_BACKEND);
      expect(machineUpdate(HOME_BACKEND).selectedTag).toBe('v1.1.0');
      expect(getBindingMock('ListServiceReleases')).toHaveBeenCalledTimes(2);
    });

    it('allows Install for a listed non-current release while nothing is in flight', async () => {
      initServiceUpdates();
      emitWailsEvent('service:update-status', status());
      setBindingMock('ListServiceReleases', async () => [
        release('v1.2.0', { isCurrent: true, isLatest: true }),
        release('v1.1.0', { isOlder: true }),
      ]);
      await loadServiceReleases(HOME_BACKEND);
      expect(canInstallSelectedRelease(HOME_BACKEND)).toBe(false);
      selectServiceRelease(HOME_BACKEND, 'v1.1.0');
      expect(canInstallSelectedRelease(HOME_BACKEND)).toBe(true);
      emitWailsEvent('service:update-status', status({ phase: 'staging' }));
      expect(canInstallSelectedRelease(HOME_BACKEND)).toBe(false);
    });

    it('reports a failed list', async () => {
      setBindingMock('ListServiceReleases', async () => {
        throw new Error('release feed unreachable');
      });
      await loadServiceReleases(HOME_BACKEND);
      expect(machineUpdate(HOME_BACKEND).releasesError).toBe('Release feed unreachable.');
      expect(machineUpdate(HOME_BACKEND).releasesLoading).toBe(false);
    });
  });
});
