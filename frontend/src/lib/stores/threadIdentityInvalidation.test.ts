import { afterEach, expect, it } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { detachBackend } from '../transport/backends';
import { noteThread } from '../transport/entityIndex';
import { getThreadHistoryStamp, recordAttestedStamp } from './threadHistoryStamps';
import { threadItemCache } from './threadItemCache';
import { beginThreadInterrupt, finishThreadInterrupt, isThreadInterruptPending, resetThreadInterruptStateForTest } from './threadInterruptState.svelte';

afterEach(() => { resetStagedBackends(); resetThreadInterruptStateForTest(); threadItemCache.clear(); });

it('invalidates only the moving conversation when its execution owner changes', () => {
  stageBackend({ id: 'gpu' });
  for (const id of ['moved', 'stays']) {
    noteThread(id, '', 0);
    recordAttestedStamp(id, 7, 19);
    beginThreadInterrupt(id);
  }
  noteThread('moved', 'gpu', 1);
  expect(getThreadHistoryStamp('moved')).toBeNull();
  expect(isThreadInterruptPending('moved')).toBe(false);
  expect(getThreadHistoryStamp('stays')).not.toBeNull();
  expect(isThreadInterruptPending('stays')).toBe(true);
});

it('scopes history invalidation and ignores computer renames', () => {
  stageBackend({ id: 'gpu' });
  noteThread('remote', 'gpu');
  noteThread('local', '');
  setBackendIdentityFromBootstrap('mac-id', 'g1', 'Mac');
  setBackendIdentityFromBootstrap('gpu-id', 'g1', 'GPU', 'gpu');
  for (const id of ['local', 'remote']) {
    recordAttestedStamp(id, 1, 1);
    beginThreadInterrupt(id);
    threadItemCache.set(id, { items: [], oldestLoadedTurnIndex: null, newestLoadedTurnIndex: null,
      hasMoreHistory: false, hasMoreNewer: false, latestSettledTurn: null });
  }
  setBackendIdentityFromBootstrap('gpu-id', 'g1', 'Renamed GPU', 'gpu');
  expect(getThreadHistoryStamp('remote')).not.toBeNull();
  expect(isThreadInterruptPending('remote')).toBe(true);
  setBackendIdentityFromBootstrap('mac-id', 'g2', 'Mac');
  expect(getThreadHistoryStamp('local')).toBeNull();
  expect(isThreadInterruptPending('local')).toBe(false);
  expect(threadItemCache.get('local')).toBeNull();
  expect(getThreadHistoryStamp('remote')).not.toBeNull();
  expect(isThreadInterruptPending('remote')).toBe(true);
  expect(threadItemCache.get('remote')).not.toBeNull();
  detachBackend('gpu');
  expect(getThreadHistoryStamp('remote')).toBeNull();
  expect(isThreadInterruptPending('remote')).toBe(false);
  expect(threadItemCache.get('remote')).toBeNull();
});

it('does not reuse an interrupt token after a computer history reset', () => {
  noteThread('local-reset', '');
  const old = beginThreadInterrupt('local-reset')!;
  setBackendIdentityFromBootstrap('mac-reset', 'new-history', 'Mac');
  const current = beginThreadInterrupt('local-reset')!;
  expect(current).not.toBe(old);
  finishThreadInterrupt('local-reset', old);
  expect(isThreadInterruptPending('local-reset')).toBe(true);
  finishThreadInterrupt('local-reset', current);
  expect(isThreadInterruptPending('local-reset')).toBe(false);
});
