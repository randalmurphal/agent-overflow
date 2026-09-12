import { afterEach, expect, it } from 'vitest';
import { stageBackend, resetStagedBackends } from '../../test/helpers/backends';
import { setBackendIdentityFromBootstrap } from '../transport/backendIdentity';
import { detachBackend } from '../transport/backends';
import { noteThread } from '../transport/entityIndex';

import { threadItemCache } from './threadItemCache';
import { beginThreadInterrupt, finishThreadInterrupt, isThreadInterruptPending, resetThreadInterruptStateForTest } from './threadInterruptState.svelte';

afterEach(() => { resetStagedBackends(); resetThreadInterruptStateForTest(); threadItemCache.clear(); });

it('invalidates only the moving conversation when its execution owner changes', () => {
  stageBackend({ id: 'gpu' });
  for (const id of ['moved', 'stays']) {
    noteThread(id, '', 0);
    beginThreadInterrupt(id);
    threadItemCache.set(id, { items: [], oldestLoadedTurnIndex: null, newestLoadedTurnIndex: null,
      hasMoreHistory: false, hasMoreNewer: false, latestSettledTurn: null,
      historyStamp: { epoch: 7, rev: 19, attested: true } });
  }
  noteThread('moved', 'gpu', 1);
  expect(threadItemCache.get('moved')).toBeNull();
  expect(threadItemCache.get('stays')?.historyStamp).toEqual({ epoch: 7, rev: 19, attested: true });
  expect(isThreadInterruptPending('moved')).toBe(false);
  expect(isThreadInterruptPending('stays')).toBe(true);
});

it('scopes history invalidation and ignores computer renames', () => {
  stageBackend({ id: 'gpu' });
  noteThread('remote', 'gpu');
  noteThread('local', '');
  setBackendIdentityFromBootstrap('mac-id', 'g1', 'Mac');
  setBackendIdentityFromBootstrap('gpu-id', 'g1', 'GPU', 'gpu');
  for (const id of ['local', 'remote']) {
    beginThreadInterrupt(id);
    threadItemCache.set(id, { items: [], oldestLoadedTurnIndex: null, newestLoadedTurnIndex: null,
      hasMoreHistory: false, hasMoreNewer: false, latestSettledTurn: null });
  }
  setBackendIdentityFromBootstrap('gpu-id', 'g1', 'Renamed GPU', 'gpu');
  expect(isThreadInterruptPending('remote')).toBe(true);
  setBackendIdentityFromBootstrap('mac-id', 'g2', 'Mac');
  expect(isThreadInterruptPending('local')).toBe(false);
  expect(threadItemCache.get('local')).toBeNull();
  expect(isThreadInterruptPending('remote')).toBe(true);
  expect(threadItemCache.get('remote')).not.toBeNull();
  detachBackend('gpu');
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
