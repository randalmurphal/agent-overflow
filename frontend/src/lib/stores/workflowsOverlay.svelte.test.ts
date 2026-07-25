import { beforeEach, describe, expect, it } from 'vitest';
import {
  closeWorkflowsOverlay,
  consumeWorkflowsOverlayEscape,
  getWorkflowArmedAction,
  getWorkflowProjectFilter,
  getWorkflowSweepIndex,
  getWorkflowsOverlayDialog,
  getWorkflowsOverlayRunId,
  getWorkflowsOverlayStack,
  getWorkflowsOverlayTop,
  isWorkflowSweepActive,
  isWorkflowsOverlayOpen,
  openWorkflowRunInOverlay,
  openWorkflowsOverlay,
  parsePersistedOverlayState,
  popWorkflowsOverlay,
  pruneWorkflowsOverlayStack,
  pushWorkflowAllClear,
  pushWorkflowRunDetail,
  resetWorkflowsOverlayForTest,
  setWorkflowArmedAction,
  setWorkflowProjectFilter,
  setWorkflowSweepCursor,
  setWorkflowsOverlayDialog,
  syncWorkflowsOverlayFromAppStorage,
  toggleWorkflowsOverlay,
} from './workflowsOverlay.svelte';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';

const STACK_KEY = 'workflows:overlay';

describe('workflows overlay navigation', () => {
  beforeEach(() => {
    resetAppStorageForTest();
    resetWorkflowsOverlayForTest();
  });

  it('opens and closes without touching the stack', () => {
    pushWorkflowRunDetail('run-1');
    expect(isWorkflowsOverlayOpen()).toBe(false);
    toggleWorkflowsOverlay();
    expect(isWorkflowsOverlayOpen()).toBe(true);
    toggleWorkflowsOverlay();
    expect(isWorkflowsOverlayOpen()).toBe(false);
    expect(getWorkflowsOverlayRunId()).toBe('run-1');
  });

  it('replaces the top when stepping between runs rather than deepening the stack', () => {
    pushWorkflowRunDetail('run-1');
    pushWorkflowRunDetail('run-2');
    pushWorkflowRunDetail('run-3');
    expect(getWorkflowsOverlayStack()).toEqual([{ level: 'home' }, { level: 'run', itemId: 'run-3' }]);
    expect(popWorkflowsOverlay()).toBe(true);
    expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    expect(popWorkflowsOverlay()).toBe(false);
  });

  it('ignores an empty run id so the stack can never hold a level with no target', () => {
    pushWorkflowRunDetail('');
    expect(getWorkflowsOverlayStack()).toEqual([{ level: 'home' }]);
  });

  it('lands all-clear on top of home and clears the sweep cursor', () => {
    pushWorkflowRunDetail('run-1', { sweep: true, sweepIndex: 2 });
    pushWorkflowAllClear();
    expect(getWorkflowsOverlayStack()).toEqual([{ level: 'home' }, { level: 'all-clear' }]);
    expect(isWorkflowSweepActive()).toBe(false);
    expect(getWorkflowSweepIndex()).toBe(-1);
  });

  it('opens a deep link straight into the sweep at that run', () => {
    openWorkflowRunInOverlay('run-9', 4);
    expect(isWorkflowsOverlayOpen()).toBe(true);
    expect(getWorkflowsOverlayRunId()).toBe('run-9');
    expect(isWorkflowSweepActive()).toBe(true);
    expect(getWorkflowSweepIndex()).toBe(4);
  });

  describe('escape precedence', () => {
    it('disarms a confirm before anything else', () => {
      openWorkflowsOverlay();
      pushWorkflowRunDetail('run-1');
      setWorkflowArmedAction('cancel:run-1');
      expect(consumeWorkflowsOverlayEscape()).toBe('disarmed');
      expect(getWorkflowArmedAction()).toBeNull();
      expect(getWorkflowsOverlayRunId()).toBe('run-1');
      expect(isWorkflowsOverlayOpen()).toBe(true);
    });

    it('closes a dialog before it pops', () => {
      openWorkflowsOverlay();
      pushWorkflowRunDetail('run-1');
      setWorkflowsOverlayDialog('discard');
      expect(consumeWorkflowsOverlayEscape()).toBe('dialog-closed');
      expect(getWorkflowsOverlayDialog()).toBeNull();
      expect(getWorkflowsOverlayRunId()).toBe('run-1');
    });

    it('pops to home, then closes the overlay', () => {
      openWorkflowsOverlay();
      pushWorkflowRunDetail('run-1');
      expect(consumeWorkflowsOverlayEscape()).toBe('popped');
      expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
      expect(consumeWorkflowsOverlayEscape()).toBe('closed');
      expect(isWorkflowsOverlayOpen()).toBe(false);
    });

    it('drops a stale armed confirm when the level changes under it', () => {
      openWorkflowsOverlay();
      pushWorkflowRunDetail('run-1');
      setWorkflowArmedAction('cancel:run-1');
      pushWorkflowRunDetail('run-2');
      expect(getWorkflowArmedAction()).toBeNull();
    });

    it('drops an armed confirm when a dialog takes over', () => {
      setWorkflowArmedAction('cancel:run-1');
      setWorkflowsOverlayDialog('intake');
      expect(getWorkflowArmedAction()).toBeNull();
    });

    it('clears transient state when the overlay closes', () => {
      openWorkflowsOverlay();
      setWorkflowsOverlayDialog('intake');
      setWorkflowArmedAction('discard:run-1');
      closeWorkflowsOverlay();
      expect(getWorkflowsOverlayDialog()).toBeNull();
      expect(getWorkflowArmedAction()).toBeNull();
    });
  });

  describe('project filter', () => {
    it('invalidates the sweep anchor because the filter narrows the set', () => {
      setWorkflowSweepCursor(true, 3);
      setWorkflowProjectFilter('project-a');
      expect(getWorkflowProjectFilter()).toBe('project-a');
      expect(getWorkflowSweepIndex()).toBe(-1);
    });

    it('is a no-op when the filter does not change', () => {
      setWorkflowProjectFilter('project-a');
      setWorkflowSweepCursor(true, 3);
      setWorkflowProjectFilter('project-a');
      expect(getWorkflowSweepIndex()).toBe(3);
    });
  });

  describe('pruning restored entries', () => {
    it('drops from the top down once a run is gone and resets the sweep', () => {
      pushWorkflowRunDetail('run-gone', { sweep: true, sweepIndex: 1 });
      pruneWorkflowsOverlayStack((itemId) => itemId !== 'run-gone');
      expect(getWorkflowsOverlayStack()).toEqual([{ level: 'home' }]);
      expect(isWorkflowSweepActive()).toBe(false);
      expect(getWorkflowSweepIndex()).toBe(-1);
    });

    it('keeps a stack whose runs all still exist', () => {
      pushWorkflowRunDetail('run-1', { sweep: true, sweepIndex: 2 });
      pruneWorkflowsOverlayStack(() => true);
      expect(getWorkflowsOverlayRunId()).toBe('run-1');
      expect(getWorkflowSweepIndex()).toBe(2);
    });

    it('leaves all-clear alone — it has no run to lose', () => {
      pushWorkflowAllClear();
      pruneWorkflowsOverlayStack(() => false);
      expect(getWorkflowsOverlayTop()).toEqual({ level: 'all-clear' });
    });
  });

  describe('persistence', () => {
    it('writes the stack, filter and cursor — but never `open`', () => {
      openWorkflowsOverlay();
      setWorkflowProjectFilter('project-a');
      pushWorkflowRunDetail('run-1', { sweep: true, sweepIndex: 2 });
      expect(JSON.parse(appStorageGet(STACK_KEY) ?? '{}')).toEqual({
        stack: [{ level: 'home' }, { level: 'run', itemId: 'run-1' }],
        projectFilter: 'project-a',
        sweepActive: true,
        sweepIndex: 2,
      });
    });

    it('adopts the durable copy once app storage hydrates', () => {
      appStorageSet(STACK_KEY, JSON.stringify({
        stack: [{ level: 'home' }, { level: 'run', itemId: 'restored' }],
        projectFilter: 'project-b',
        sweepActive: true,
        sweepIndex: 5,
      }));
      syncWorkflowsOverlayFromAppStorage();
      expect(getWorkflowsOverlayRunId()).toBe('restored');
      expect(getWorkflowProjectFilter()).toBe('project-b');
      expect(getWorkflowSweepIndex()).toBe(5);
      expect(isWorkflowsOverlayOpen()).toBe(false);
    });

    it('leaves the live state alone when nothing is stored', () => {
      pushWorkflowRunDetail('run-1');
      appStorageSet(STACK_KEY, JSON.stringify({ stack: [{ level: 'home' }] }));
      syncWorkflowsOverlayFromAppStorage();
      expect(getWorkflowsOverlayTop()).toEqual({ level: 'home' });
    });
  });

  describe('parsing a persisted stack', () => {
    const fresh = { stack: [{ level: 'home' }], projectFilter: '', sweepActive: false, sweepIndex: -1 };

    it.each([
      ['absent', null],
      ['not JSON', '{'],
      ['an array', '[]'],
      ['a scalar', '7'],
    ])('falls back to a fresh stack when the value is %s', (_label, raw) => {
      expect(parsePersistedOverlayState(raw)).toEqual(fresh);
    });

    it('repairs a stack that lost its home floor', () => {
      expect(parsePersistedOverlayState(JSON.stringify({
        stack: [{ level: 'run', itemId: 'run-1' }],
      })).stack).toEqual([{ level: 'home' }, { level: 'run', itemId: 'run-1' }]);
    });

    it('drops malformed entries and out-of-range cursors', () => {
      const parsed = parsePersistedOverlayState(JSON.stringify({
        stack: [{ level: 'home' }, { level: 'run' }, { level: 'nope' }, { level: 'all-clear' }],
        projectFilter: 7,
        sweepIndex: 1.5,
      }));
      expect(parsed.stack).toEqual([{ level: 'home' }, { level: 'all-clear' }]);
      expect(parsed.projectFilter).toBe('');
      expect(parsed.sweepIndex).toBe(-1);
    });
  });
});
