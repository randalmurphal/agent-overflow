import { describe, expect, it } from 'vitest';
import { createDiffSidebarSlot, DIFF_SIDEBAR_LRU_CAP } from './diffSidebarSlot.svelte';

const ui = {
  viewMode: 'stacked' as const,
  wordWrap: false,
  expandedFiles: ['src/foo.ts'],
  scrollTop: 100,
};

describe('createDiffSidebarSlot', () => {
  describe('open / close basics', () => {
    it('open sets the active payload and clears restore', () => {
      const slot = createDiffSidebarSlot();
      slot.open({ payloadId: 'p1', filePath: 'src/foo.ts' });
      expect(slot.activePayload).toEqual({ payloadId: 'p1', filePath: 'src/foo.ts' });
      expect(slot.restoreState).toBeNull();
    });

    it('close() preserves the snapshot map (mutex close)', () => {
      const slot = createDiffSidebarSlot();
      slot.open({ payloadId: 'p1' });
      slot.recordUI(ui);
      slot.snapshotForThread('thread-a');
      // Now slot is reset, but byThread still has thread-a.
      slot.open({ payloadId: 'p2' });
      slot.recordUI(ui);
      // Mutex close — no thread id; should NOT clear byThread.
      slot.close();
      slot.restoreForThread('thread-a');
      expect(slot.activePayload).toEqual({ payloadId: 'p1', filePath: undefined });
    });

    it('close(threadId) drops only that thread\'s snapshot from the LRU', () => {
      const slot = createDiffSidebarSlot();
      // Snapshot two threads.
      slot.open({ payloadId: 'pa' });
      slot.recordUI(ui);
      slot.snapshotForThread('thread-a');
      slot.open({ payloadId: 'pb' });
      slot.recordUI(ui);
      slot.snapshotForThread('thread-b');

      // Drop thread-a's snapshot via close(threadId) form.
      slot.close('thread-a');

      // Restoring thread-a finds nothing.
      slot.restoreForThread('thread-a');
      expect(slot.activePayload).toBeNull();

      // thread-b's snapshot is intact.
      slot.restoreForThread('thread-b');
      expect(slot.activePayload).toEqual({ payloadId: 'pb', filePath: undefined });
    });
  });

  describe('snapshot / restore', () => {
    it('snapshotForThread is a no-op when no payload is open', () => {
      const slot = createDiffSidebarSlot();
      slot.snapshotForThread('thread-a');
      expect(slot.snapshotCount).toBe(0);
      slot.restoreForThread('thread-a');
      expect(slot.activePayload).toBeNull();
    });

    it('snapshotForThread is a no-op when payload is open but no UI was recorded', () => {
      // Regression guard against silent data loss: opening then
      // immediately switching threads would otherwise drop the
      // payload pointer without preserving anything.
      const slot = createDiffSidebarSlot();
      slot.open({ payloadId: 'p1' });
      slot.snapshotForThread('thread-a');
      // No UI was recorded → no snapshot saved.
      expect(slot.snapshotCount).toBe(0);
    });

    it('restoreForThread on unknown thread does nothing', () => {
      const slot = createDiffSidebarSlot();
      slot.restoreForThread('never-seen');
      expect(slot.activePayload).toBeNull();
      expect(slot.restoreState).toBeNull();
    });

    it('restoreForThread re-arms activePayload + seeds restoreState', () => {
      const slot = createDiffSidebarSlot();
      slot.open({ payloadId: 'pa', filePath: 'src/a.ts' });
      slot.recordUI({ ...ui, scrollTop: 250 });
      slot.snapshotForThread('thread-a');

      slot.restoreForThread('thread-a');
      expect(slot.activePayload).toEqual({ payloadId: 'pa', filePath: 'src/a.ts' });
      expect(slot.restoreState).toEqual({
        viewMode: 'stacked',
        wordWrap: false,
        expandedFiles: ['src/foo.ts'],
        scrollTop: 250,
      });
    });

    it('consumeRestore is one-shot — second call returns null', () => {
      const slot = createDiffSidebarSlot();
      slot.open({ payloadId: 'pa' });
      slot.recordUI(ui);
      slot.snapshotForThread('thread-a');
      slot.restoreForThread('thread-a');

      const first = slot.consumeRestore();
      expect(first).not.toBeNull();
      expect(slot.consumeRestore()).toBeNull();
    });

    it('restoreForThread touches the LRU so a re-visited thread stays hot through eviction', () => {
      const slot = createDiffSidebarSlot();
      // Fill to cap.
      for (let i = 0; i < DIFF_SIDEBAR_LRU_CAP; i += 1) {
        slot.open({ payloadId: `p${i}` });
        slot.recordUI({ ...ui, scrollTop: i });
        slot.snapshotForThread(`t${i}`);
      }
      expect(slot.snapshotCount).toBe(DIFF_SIDEBAR_LRU_CAP);

      // Re-visit t0 (touches LRU).
      slot.restoreForThread('t0');
      slot.consumeRestore();

      // Push 5 new threads — t1..t4 should evict, t0 survives.
      for (let i = DIFF_SIDEBAR_LRU_CAP; i < DIFF_SIDEBAR_LRU_CAP + 5; i += 1) {
        slot.open({ payloadId: `p${i}` });
        slot.recordUI({ ...ui, scrollTop: i });
        slot.snapshotForThread(`t${i}`);
      }
      expect(slot.snapshotCount).toBe(DIFF_SIDEBAR_LRU_CAP);

      // t0 is still restorable.
      slot.restoreForThread('t0');
      expect(slot.activePayload?.payloadId).toBe('p0');

      // t1 was evicted.
      slot.close(); // reset before next restore
      slot.restoreForThread('t1');
      expect(slot.activePayload).toBeNull();
    });
  });

  describe('LRU bound', () => {
    it('caps at exactly DIFF_SIDEBAR_LRU_CAP entries', () => {
      const slot = createDiffSidebarSlot();
      for (let i = 0; i < DIFF_SIDEBAR_LRU_CAP + 5; i += 1) {
        slot.open({ payloadId: `p${i}` });
        slot.recordUI({ ...ui, scrollTop: i });
        slot.snapshotForThread(`t${i}`);
      }
      expect(slot.snapshotCount).toBe(DIFF_SIDEBAR_LRU_CAP);
    });

    it('exposes DIFF_SIDEBAR_LRU_CAP at 20 (memory budget pin)', () => {
      // Hard-coded number lives in two places (slot + memory plan
      // doc); pinning the value here ensures a future raise has to
      // update the test, surfacing the change in code review.
      expect(DIFF_SIDEBAR_LRU_CAP).toBe(20);
    });
  });

  describe('reset', () => {
    it('reset wipes everything', () => {
      const slot = createDiffSidebarSlot();
      slot.open({ payloadId: 'p1' });
      slot.recordUI(ui);
      slot.snapshotForThread('thread-a');
      expect(slot.snapshotCount).toBe(1);

      slot.reset();
      expect(slot.snapshotCount).toBe(0);
      expect(slot.activePayload).toBeNull();
      expect(slot.restoreState).toBeNull();
    });
  });
});
