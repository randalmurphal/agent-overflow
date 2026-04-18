import { describe, expect, it, beforeEach } from 'vitest';
import {
  clearThreadStatus,
  getAllThreadStatuses,
  getNonIdleThreadCount,
  getThreadStatus,
  resetForTest,
  setThreadStatus,
} from './threadStatuses.svelte';

describe('threadStatuses store', () => {
  beforeEach(() => {
    resetForTest();
  });

  describe('getThreadStatus', () => {
    it('returns idle for unknown threads', () => {
      expect(getThreadStatus('ghost')).toBe('idle');
    });

    it('returns the latest set value', () => {
      setThreadStatus('t1', 'running');
      expect(getThreadStatus('t1')).toBe('running');
      setThreadStatus('t1', 'pending-approval');
      expect(getThreadStatus('t1')).toBe('pending-approval');
    });
  });

  describe('setThreadStatus', () => {
    it('is independent per thread', () => {
      setThreadStatus('t1', 'running');
      setThreadStatus('t2', 'error');
      expect(getThreadStatus('t1')).toBe('running');
      expect(getThreadStatus('t2')).toBe('error');
    });

    it('setting idle drops the entry (does not keep a stale idle row)', () => {
      setThreadStatus('t1', 'running');
      expect(getAllThreadStatuses().has('t1')).toBe(true);
      setThreadStatus('t1', 'idle');
      expect(getAllThreadStatuses().has('t1')).toBe(false);
      // Still returns idle via the public getter.
      expect(getThreadStatus('t1')).toBe('idle');
    });

    it('setting idle for an already-absent thread is a no-op', () => {
      // Regression: must not throw or flip the map reference pointlessly.
      const before = getAllThreadStatuses();
      setThreadStatus('never-seen', 'idle');
      const after = getAllThreadStatuses();
      // Same-value set is a no-op; we just assert the entry still isn't there.
      expect(after.has('never-seen')).toBe(false);
      expect(before.size).toBe(after.size);
    });

    it('same-value re-set is idempotent', () => {
      setThreadStatus('t1', 'running');
      const snapshot = getAllThreadStatuses();
      setThreadStatus('t1', 'running');
      // Entry still present, same status.
      expect(getThreadStatus('t1')).toBe('running');
      expect(snapshot.size).toBe(1);
    });
  });

  describe('clearThreadStatus', () => {
    it('removes the entry', () => {
      setThreadStatus('t1', 'running');
      clearThreadStatus('t1');
      expect(getThreadStatus('t1')).toBe('idle');
      expect(getAllThreadStatuses().has('t1')).toBe(false);
    });

    it('is a no-op on an unknown id', () => {
      clearThreadStatus('ghost');
      expect(getAllThreadStatuses().size).toBe(0);
    });

    it('only clears the requested thread', () => {
      setThreadStatus('t1', 'running');
      setThreadStatus('t2', 'error');
      clearThreadStatus('t1');
      expect(getThreadStatus('t1')).toBe('idle');
      expect(getThreadStatus('t2')).toBe('error');
    });
  });

  describe('getNonIdleThreadCount', () => {
    it('starts at zero', () => {
      expect(getNonIdleThreadCount()).toBe(0);
    });

    it('counts every non-idle thread', () => {
      setThreadStatus('t1', 'running');
      setThreadStatus('t2', 'pending-approval');
      setThreadStatus('t3', 'error');
      expect(getNonIdleThreadCount()).toBe(3);
    });

    it('drops to zero when all threads clear', () => {
      setThreadStatus('t1', 'running');
      setThreadStatus('t2', 'pending-approval');
      clearThreadStatus('t1');
      setThreadStatus('t2', 'idle');
      expect(getNonIdleThreadCount()).toBe(0);
    });
  });

  describe('resetForTest', () => {
    it('wipes every entry', () => {
      setThreadStatus('t1', 'running');
      setThreadStatus('t2', 'error');
      resetForTest();
      expect(getNonIdleThreadCount()).toBe(0);
      expect(getThreadStatus('t1')).toBe('idle');
      expect(getThreadStatus('t2')).toBe('idle');
    });
  });
});
