// The provider CLI update store: one snapshot per attached computer, fed by
// `provider:updates` keyed on the frame's origin and by the two snapshot
// reads, the run request, the announcement toasts and the notes modal.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import {
  grantBackendScopes,
  pairViewOnly,
  pairWithScopes,
  resetToLocalPage,
  revokeBackendScopes,
} from '../../test/helpers/scopes';
import { REMOTE_BACKEND_UUID, resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { SCOPES } from '../transport/scopes';
import { takePinnedBackend } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { STEP_UP_REFUSAL } from '../transport/scopeRefusal';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { applyTransportGap } from './eventsTransportGap';
import { listenProviderUpdates } from '../../test/helpers/providerUpdates';
import { readFrontendValue } from './frontendStorage';
import {
  ANNOUNCED_STORAGE_KEY,
  checkProviderUpdates,
  closeProviderUpdateNotes,
  hasProviderUpdate,
  loadProviderUpdates,
  openProviderUpdateNotes,
  providerUpdate,
  providerUpdateNotes,
  providerUpdateFailure,
  providerUpdateRunning,
  providerUpdatesLoadError,
  resetProviderUpdatesForTest,
  retryProviderUpdateNotes,
  runProviderUpdate,
  type ProviderUpdateNotes,
  type ProviderUpdateStatus,
} from './providerUpdates.svelte';
import { methodNotFound } from '../../test/helpers/olderComputer';

function status(provider: string, overrides: Partial<ProviderUpdateStatus> = {}): ProviderUpdateStatus {
  return {
    provider,
    installedVersion: '1.0.0',
    latestVersion: '',
    available: false,
    checkedAt: 0,
    run: { phase: 'idle' },
    ...overrides,
  } as ProviderUpdateStatus;
}

const available = (provider: string, latest: string, overrides: Partial<ProviderUpdateStatus> = {}) =>
  status(provider, { latestVersion: latest, available: true, checkedAt: 1, ...overrides });

function notes(provider: string, versions: string[]): ProviderUpdateNotes {
  return {
    provider,
    fromVersion: '1.0.0',
    toVersion: versions[0] ?? '',
    releases: versions.map((version) => ({ version, name: `v${version}`, publishedAt: '', body: `notes ${version}`, url: '' })),
    omitted: 0,
    url: 'https://github.com/example/releases',
  } as ProviderUpdateNotes;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

const tick = () => new Promise<void>((resolve) => setTimeout(resolve, 0));
const messages = () => getToasts().map((t) => t.message);

const LAPTOP_HELLO = {
  protocolVersion: 1,
  capabilities: [],
  backendId: REMOTE_BACKEND_UUID,
  backendName: 'laptop',
  serverTimeMs: 0,
  clockSkewMs: 0,
  bundleId: '',
} as never;

describe('providerUpdates store', () => {
  beforeEach(() => {
    resetToLocalPage();
    localStorage.clear();
  });

  afterEach(() => {
    revokeBackendScopes('laptop');
    resetToLocalPage();
  });

  describe('hydration', () => {
    it('reads the snapshot on the owner’s screen and keeps it per computer', async () => {
      const read = setBindingMock('GetProviderUpdates', async () => [status('claude'), status('codex', { installedVersion: '0.1.0' })]);
      await loadProviderUpdates(HOME_BACKEND);
      expect(read).toHaveBeenCalledTimes(1);
      expect(providerUpdate('codex', HOME_BACKEND)?.installedVersion).toBe('0.1.0');
      expect(providerUpdate('claude', 'laptop')).toBeNull();
    });

    it('issues nothing from a view-only session, and reads with full access', async () => {
      const read = setBindingMock('GetProviderUpdates', async () => [status('claude')]);
      await pairViewOnly();
      await loadProviderUpdates(HOME_BACKEND);
      await checkProviderUpdates(HOME_BACKEND);
      expect(read).not.toHaveBeenCalled();
      expect(providerUpdate('claude', HOME_BACKEND)).toBeNull();

      await pairWithScopes(SCOPES);
      await loadProviderUpdates(HOME_BACKEND);
      expect(read).toHaveBeenCalledTimes(1);
    });

    it('reads a second computer only with the grant, pinned to it', async () => {
      const read = setBindingMock('GetProviderUpdates', async () => {
        expect(takePinnedBackend()).toBe('laptop');
        return [status('claude', { installedVersion: '2.0.0' })];
      });
      stageBackend({ hello: LAPTOP_HELLO });
      await loadProviderUpdates('laptop');
      expect(read).not.toHaveBeenCalled();

      await grantBackendScopes('laptop', SCOPES);
      await loadProviderUpdates('laptop');
      expect(read).toHaveBeenCalledTimes(1);
      expect(providerUpdate('claude', 'laptop')?.installedVersion).toBe('2.0.0');
      expect(providerUpdate('claude', HOME_BACKEND)).toBeNull();
    });

    it('keeps the snapshot through a dropped socket and forgets it on detach', async () => {
      setBindingMock('GetProviderUpdates', async () => [available('claude', '1.1.0')]);
      const laptop = stageBackend({ hello: LAPTOP_HELLO });
      await grantBackendScopes('laptop', SCOPES);
      await loadProviderUpdates('laptop');
      expect(hasProviderUpdate('claude', 'laptop')).toBe(true);
      laptop.setHello(null);
      laptop.setStatus('reconnecting');
      expect(hasProviderUpdate('claude', 'laptop')).toBe(true);
      resetStagedBackends();
      expect(providerUpdate('claude', 'laptop')).toBeNull();
    });

    it('is silent on passive failures and shows any other read failure', async () => {
      for (const err of [
        new TransportError('scope_required', 'no'),
        methodNotFound(),
        new DisconnectedError('socket closed'),
      ]) {
        setBindingMock('CheckProviderUpdates', async () => { throw err; });
        await checkProviderUpdates(HOME_BACKEND);
        expect(providerUpdatesLoadError(HOME_BACKEND), err.message).toBe('');
      }
      setBindingMock('CheckProviderUpdates', async () => { throw new Error('release host unreachable'); });
      await checkProviderUpdates(HOME_BACKEND);
      expect(providerUpdatesLoadError(HOME_BACKEND)).toBe('Release host unreachable.');
      expect(getToasts()).toEqual([]);
    });
  });

  describe('the channel', () => {
    it('replaces the snapshot of the computer the frame came from, and only that one', () => {
      listenProviderUpdates();
      stageBackend();
      emitWailsEvent('provider:updates', [status('claude', { installedVersion: '1.0.0' })]);
      emitWailsEvent('provider:updates', [status('claude', { installedVersion: '2.0.0' }), status('codex')], REMOTE_BACKEND_UUID);
      expect(providerUpdate('claude', HOME_BACKEND)?.installedVersion).toBe('1.0.0');
      expect(providerUpdate('codex', HOME_BACKEND)).toBeNull();
      expect(providerUpdate('claude', 'laptop')?.installedVersion).toBe('2.0.0');

      // Wholesale: a provider missing from the next frame is gone.
      emitWailsEvent('provider:updates', [status('codex')], REMOTE_BACKEND_UUID);
      expect(providerUpdate('claude', 'laptop')).toBeNull();
      expect(providerUpdate('claude', HOME_BACKEND)?.installedVersion).toBe('1.0.0');
    });

    it('drops a read that a frame overtook, and a read a newer read superseded', async () => {
      listenProviderUpdates();
      const first = deferred<ProviderUpdateStatus[]>();
      setBindingMock('GetProviderUpdates', () => first.promise);
      const reading = loadProviderUpdates(HOME_BACKEND);
      emitWailsEvent('provider:updates', [status('claude', { run: { phase: 'running' } })]);
      first.resolve([status('claude')]);
      await reading;
      expect(providerUpdate('claude', HOME_BACKEND)?.run.phase).toBe('running');

      const older = deferred<ProviderUpdateStatus[]>();
      const newer = deferred<ProviderUpdateStatus[]>();
      setBindingMock('GetProviderUpdates', () => older.promise);
      const a = loadProviderUpdates(HOME_BACKEND);
      setBindingMock('CheckProviderUpdates', () => newer.promise);
      const b = checkProviderUpdates(HOME_BACKEND);
      newer.resolve([status('claude', { installedVersion: '3.0.0' })]);
      await b;
      older.resolve([status('claude', { installedVersion: '0.0.1' })]);
      await a;
      expect(providerUpdate('claude', HOME_BACKEND)?.installedVersion).toBe('3.0.0');
    });

    it('drops a reply for a computer detached while it was in flight', async () => {
      stageBackend({ hello: LAPTOP_HELLO });
      await grantBackendScopes('laptop', SCOPES);
      listenProviderUpdates();
      const reply = deferred<ProviderUpdateStatus[]>();
      setBindingMock('GetProviderUpdates', () => reply.promise);
      const reading = loadProviderUpdates('laptop');
      resetStagedBackends();
      reply.resolve([available('claude', '9.0.0')]);
      await reading;
      expect(providerUpdate('claude', 'laptop')).toBeNull();
      expect(getToasts()).toEqual([]);
    });

    it('reads the computer again after a gap, and says what the lost frame would have', async () => {
      stageBackend();
      await grantBackendScopes('laptop', SCOPES);
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'running' } })], REMOTE_BACKEND_UUID);
      const read = setBindingMock('GetProviderUpdates', async () => {
        expect(takePinnedBackend()).toBe('laptop');
        return [status('claude', { installedVersion: '1.1.0', run: { phase: 'succeeded', toVersion: '1.1.0' } })];
      });
      applyTransportGap({ channel: 'provider:updates', seq: 0 }, { backendId: REMOTE_BACKEND_UUID });
      await tick();
      expect(read).toHaveBeenCalledTimes(1);
      expect(providerUpdateRunning('claude', 'laptop')).toBe(false);
      expect(messages()).toEqual(['Claude updated to 1.1.0 on Laptop']);
    });

    it('keeps the snapshot and the recovery read through a gap marker with no payload', async () => {
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'running' } })]);
      const reply = deferred<ProviderUpdateStatus[]>();
      setBindingMock('GetProviderUpdates', () => reply.promise);
      applyTransportGap({ channel: 'provider:updates', seq: 0 }, { backendId: '' });
      // The transport hands the marker to the channel right after the gap.
      emitWailsEvent('provider:updates', null);
      expect(providerUpdateRunning('claude', HOME_BACKEND)).toBe(true);
      reply.resolve([status('claude', { installedVersion: '1.1.0', run: { phase: 'succeeded', toVersion: '1.1.0' } })]);
      await tick();
      expect(providerUpdateRunning('claude', HOME_BACKEND)).toBe(false);
      expect(providerUpdate('claude', HOME_BACKEND)?.installedVersion).toBe('1.1.0');
    });

  });

  describe('hasProviderUpdate', () => {
    it('is true for an available, not running update with the grant', async () => {
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [status('claude', { latestVersion: '1.0.0' })]);
      expect(hasProviderUpdate('claude', HOME_BACKEND)).toBe(false);
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')]);
      expect(hasProviderUpdate('claude', HOME_BACKEND)).toBe(true);
      expect(hasProviderUpdate('codex', HOME_BACKEND)).toBe(false);
      emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'running' } })]);
      expect(hasProviderUpdate('claude', HOME_BACKEND)).toBe(false);
      emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'failed' } })]);
      expect(hasProviderUpdate('claude', HOME_BACKEND)).toBe(true);
      await pairViewOnly();
      expect(hasProviderUpdate('claude', HOME_BACKEND)).toBe(false);
    });
  });

  describe('announcements', () => {
    it('toasts once per provider version, persisted, across snapshots, computers and relaunch', async () => {
      stageBackend();
      await grantBackendScopes('laptop', SCOPES);
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0'), status('codex')]);
      expect(messages()).toEqual(['Claude 1.1.0 is available on This computer']);
      expect(readFrontendValue(ANNOUNCED_STORAGE_KEY)).toEqual(['claude@1.1.0']);

      emitWailsEvent('provider:updates', [available('claude', '1.1.0'), status('codex')]);
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')], REMOTE_BACKEND_UUID);
      expect(messages()).toHaveLength(1);

      // A relaunch: the store starts empty, the storage does not.
      resetProviderUpdatesForTest();
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')]);
      expect(messages()).toHaveLength(1);

      emitWailsEvent('provider:updates', [available('claude', '1.2.0'), available('codex', '0.2.0')]);
      expect(messages()).toEqual([
        'Claude 1.1.0 is available on This computer',
        'Claude 1.2.0 is available on This computer',
        'Codex 0.2.0 is available on This computer',
      ]);
    });

    it('does not toast a version whose run is in progress, nor after that run fails', () => {
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'running' } })]);
      expect(messages()).toEqual([]);
      emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'failed', error: 'exit status 1' } })]);
      expect(messages()).toEqual(['Claude did not update.']);
    });

    it('names the computer in every toast while more than one is attached', async () => {
      stageBackend();
      await grantBackendScopes('laptop', SCOPES);
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')], REMOTE_BACKEND_UUID);
      emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'running' } })], REMOTE_BACKEND_UUID);
      emitWailsEvent('provider:updates', [status('claude', { installedVersion: '1.1.0', run: { phase: 'succeeded', toVersion: '1.1.0' } })], REMOTE_BACKEND_UUID);
      emitWailsEvent('provider:updates', [available('codex', '0.3.0', { run: { phase: 'running' } })], REMOTE_BACKEND_UUID);
      emitWailsEvent('provider:updates', [available('codex', '0.3.0', { run: { phase: 'failed', error: 'npm exited 1' } })], REMOTE_BACKEND_UUID);
      expect(messages()).toEqual([
        'Claude 1.1.0 is available on Laptop',
        'Claude updated to 1.1.0 on Laptop',
        'Codex did not update on Laptop.',
      ]);
    });

    it('does not toast for a computer this session does not administer', () => {
      stageBackend();
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')], REMOTE_BACKEND_UUID);
      expect(messages()).toEqual([]);
      expect(readFrontendValue(ANNOUNCED_STORAGE_KEY)).toBeNull();
    });

    it("opens the notes of the computer that reported it from the toast's action", async () => {
      stageBackend();
      await grantBackendScopes('laptop', SCOPES);
      listenProviderUpdates();
      const load = setBindingMock('GetProviderUpdateNotes', async () => {
        expect(takePinnedBackend()).toBe('laptop');
        return notes('codex', ['0.2.0']);
      });
      emitWailsEvent('provider:updates', [available('codex', '0.2.0')], REMOTE_BACKEND_UUID);
      const toast = getToasts()[0];
      expect(toast.action?.label).toBe("What's new");
      toast.action!.run();
      expect(providerUpdateNotes()).toMatchObject({ provider: 'codex', backend: 'laptop', loading: true });
      await tick();
      expect(load).toHaveBeenCalledWith('codex');
      expect(providerUpdateNotes()?.notes?.releases[0].version).toBe('0.2.0');
    });

    it('says a watched run succeeded or failed, and nothing for one it never saw running', () => {
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [status('claude', { run: { phase: 'succeeded', toVersion: '1.1.0' } })]);
      expect(messages()).toEqual([]);

      emitWailsEvent('provider:updates', [available('claude', '1.2.0', { run: { phase: 'running' } })]);
      emitWailsEvent('provider:updates', [status('claude', { installedVersion: '1.2.0', run: { phase: 'succeeded', toVersion: '1.2.0' } })]);
      expect(messages()).toEqual(['Claude updated to 1.2.0']);
      expect(getToasts()[0].type).toBe('success');

      emitWailsEvent('provider:updates', [available('codex', '0.3.0', { run: { phase: 'running' } })]);
      emitWailsEvent('provider:updates', [available('codex', '0.3.0', { run: { phase: 'failed', error: 'npm exited 1' } })]);
      const failure = getToasts().at(-1)!;
      expect(failure.type).toBe('error');
      expect(failure.message).toBe('Codex did not update.');
      expect(failure.captured).toBeTruthy();
    });
  });

  describe('running an update', () => {
    it('pins the computer, is busy from the click, and lets frames drive the outcome', async () => {
      stageBackend();
      await grantBackendScopes('laptop', SCOPES);
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')], REMOTE_BACKEND_UUID);
      // The backend publishes the running frame before it answers the call.
      const run = setBindingMock('RunProviderUpdate', async () => {
        expect(takePinnedBackend()).toBe('laptop');
        emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'running' } })], REMOTE_BACKEND_UUID);
      });
      const pending = runProviderUpdate('claude', 'laptop');
      expect(providerUpdateRunning('claude', 'laptop')).toBe(true);
      expect(providerUpdateRunning('claude', HOME_BACKEND)).toBe(false);
      await pending;
      expect(run).toHaveBeenCalledWith('claude');
      expect(providerUpdateRunning('claude', 'laptop')).toBe(true);
      emitWailsEvent('provider:updates', [status('claude', { installedVersion: '1.1.0', run: { phase: 'succeeded', toVersion: '1.1.0' } })], REMOTE_BACKEND_UUID);
      expect(providerUpdateRunning('claude', 'laptop')).toBe(false);
    });

    it('writes nothing for a computer detached while the call was in flight', async () => {
      stageBackend();
      await grantBackendScopes('laptop', SCOPES);
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')], REMOTE_BACKEND_UUID);
      const reply = deferred<void>();
      setBindingMock('RunProviderUpdate', () => reply.promise);
      const pending = runProviderUpdate('claude', 'laptop');
      resetStagedBackends();
      reply.reject(new Error('connection closed'));
      await pending;
      expect(providerUpdateFailure('claude', 'laptop')).toBeNull();
      expect(providerUpdateRunning('claude', 'laptop')).toBe(false);
    });

    it('leaves a newer call busy when the call of a detached computer settles', async () => {
      stageBackend();
      await grantBackendScopes('laptop', SCOPES);
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')], REMOTE_BACKEND_UUID);
      const first = deferred<void>();
      const second = deferred<void>();
      const replies = [first, second];
      setBindingMock('RunProviderUpdate', () => replies.shift()!.promise);
      const stale = runProviderUpdate('claude', 'laptop');
      resetStagedBackends();
      stageBackend();
      await grantBackendScopes('laptop', SCOPES);
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')], REMOTE_BACKEND_UUID);
      const fresh = runProviderUpdate('claude', 'laptop');
      first.reject(new Error('connection closed'));
      await stale;
      expect(providerUpdateRunning('claude', 'laptop')).toBe(true);
      expect(providerUpdateFailure('claude', 'laptop')).toBeNull();
      second.resolve();
      await fresh;
      expect(providerUpdateRunning('claude', 'laptop')).toBe(false);
    });

    it('does not double-fire while a run is on', async () => {
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0', { run: { phase: 'running' } })]);
      const run = setBindingMock('RunProviderUpdate', async () => undefined);
      await runProviderUpdate('claude', HOME_BACKEND);
      expect(run).not.toHaveBeenCalled();
    });

    it('shows a step-up refusal beside the control and as a toast, until the next check', async () => {
      listenProviderUpdates();
      emitWailsEvent('provider:updates', [available('claude', '1.1.0')]);
      resetToastsForTest();
      setBindingMock('RunProviderUpdate', async () => { throw new TransportError('step_up_required', 'step-up required'); });
      await runProviderUpdate('claude', HOME_BACKEND);
      const shown = `${STEP_UP_REFUSAL.title} ${STEP_UP_REFUSAL.hint}`;
      expect(providerUpdateFailure('claude', HOME_BACKEND)).toEqual({ error: shown, output: '' });
      expect(providerUpdateRunning('claude', HOME_BACKEND)).toBe(false);
      expect(messages()).toEqual([shown]);
      expect(getToasts()[0].type).toBe('error');

      setBindingMock('CheckProviderUpdates', async () => [available('claude', '1.1.0')]);
      await checkProviderUpdates(HOME_BACKEND);
      expect(providerUpdateFailure('claude', HOME_BACKEND)).toBeNull();
    });
  });

  describe('release notes', () => {
    it('loads on open, pinned to the computer, and closing drops a late reply', async () => {
      const reply = deferred<ProviderUpdateNotes>();
      setBindingMock('GetProviderUpdateNotes', () => reply.promise);
      openProviderUpdateNotes('claude', HOME_BACKEND);
      expect(providerUpdateNotes()).toMatchObject({ provider: 'claude', loading: true, notes: null });
      closeProviderUpdateNotes();
      reply.resolve(notes('claude', ['1.1.0']));
      await tick();
      expect(providerUpdateNotes()).toBeNull();
    });

    it('drops the reply of a modal retargeted to another provider', async () => {
      const claude = deferred<ProviderUpdateNotes>();
      const codex = deferred<ProviderUpdateNotes>();
      setBindingMock('GetProviderUpdateNotes', (provider: string) => (provider === 'claude' ? claude.promise : codex.promise));
      openProviderUpdateNotes('claude', HOME_BACKEND);
      openProviderUpdateNotes('codex', HOME_BACKEND);
      codex.resolve(notes('codex', ['0.2.0']));
      await tick();
      claude.resolve(notes('claude', ['1.1.0']));
      await tick();
      expect(providerUpdateNotes()?.provider).toBe('codex');
      expect(providerUpdateNotes()?.notes?.releases[0].version).toBe('0.2.0');
    });

    it('shows a failure and loads again on retry', async () => {
      setBindingMock('GetProviderUpdateNotes', async () => { throw new Error('github rate limited'); });
      openProviderUpdateNotes('claude', HOME_BACKEND);
      await tick();
      expect(providerUpdateNotes()).toMatchObject({ loading: false, notes: null, error: 'Github rate limited.' });
      const load = setBindingMock('GetProviderUpdateNotes', async () => notes('claude', ['1.1.0']));
      retryProviderUpdateNotes();
      expect(providerUpdateNotes()?.loading).toBe(true);
      await tick();
      expect(load).toHaveBeenCalledTimes(1);
      expect(providerUpdateNotes()).toMatchObject({ loading: false, error: '' });
    });

    it('loads again on every open, so the range follows the latest check', async () => {
      let versions = ['1.1.0'];
      const load = setBindingMock('GetProviderUpdateNotes', async () => notes('claude', versions));
      openProviderUpdateNotes('claude', HOME_BACKEND);
      await tick();
      expect(providerUpdateNotes()?.notes?.toVersion).toBe('1.1.0');

      versions = ['1.2.0', '1.1.0'];
      openProviderUpdateNotes('claude', HOME_BACKEND);
      expect(providerUpdateNotes()).toMatchObject({ loading: true, notes: null });
      await tick();
      expect(load).toHaveBeenCalledTimes(2);
      expect(providerUpdateNotes()?.notes?.toVersion).toBe('1.2.0');
    });

    it('closes when its computer is detached', async () => {
      stageBackend();
      listenProviderUpdates();
      setBindingMock('GetProviderUpdateNotes', async () => notes('claude', ['1.1.0']));
      openProviderUpdateNotes('claude', 'laptop');
      await tick();
      expect(providerUpdateNotes()?.backend).toBe('laptop');
      resetStagedBackends();
      expect(providerUpdateNotes()).toBeNull();
    });
  });
});
