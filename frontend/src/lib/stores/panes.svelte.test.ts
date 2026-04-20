import { describe, expect, it, beforeEach } from 'vitest';
import { getMainPane, getAllPanes } from './panes.svelte';

describe('panes store', () => {
  beforeEach(() => {
    // Module state is shared across tests; drain between cases.
    getAllPanes().clear();
  });

  describe('getMainPane()', () => {
    it('creates the main pane lazily on first call', () => {
      expect(getAllPanes().size).toBe(0);
      const pane = getMainPane();
      expect(pane).toBeDefined();
      expect(getAllPanes().size).toBe(1);
      expect(getAllPanes().get('main')).toBe(pane);
    });

    it('returns the same instance on subsequent calls', () => {
      const a = getMainPane();
      const b = getMainPane();
      expect(a).toBe(b);
    });

    it('exposes a usable pane contract', () => {
      const pane = getMainPane();
      expect(pane.thread).toBeNull();
      expect(pane.items).toEqual([]);
      expect(pane.pendingApprovals).toEqual([]);
      expect(typeof pane.upsertItem).toBe('function');
    });
  });
});
