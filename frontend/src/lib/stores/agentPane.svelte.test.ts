import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  __resetAgentPaneStateForTest,
  agentPaneScopeTrailHolds,
  agentScopeForPane,
  agentStateForPane,
  disposeAgentStateForPane,
  openAgentCompanion,
  seedAgentStateForPane,
} from './agentPane.svelte';
import {
  closeCompanion,
  companionForSource,
  installCompanionPanes,
  resetCompanionPanesForTest,
} from './companionPanes.svelte';
import {
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
  type PaneLayoutItem,
} from './paneLayout.svelte';
import { createPane, destroyPane, resetPanesForTest } from './panes.svelte';

function threadItem(paneId: string): PaneLayoutItem {
  return { id: paneId, paneId, kind: 'thread', widthPx: 560 };
}

function labels(sourcePaneId: string, threadId = 'thread-1'): string[] {
  return agentStateForPane(sourcePaneId, threadId).breadcrumb.map((entry) => entry.label);
}

beforeEach(() => {
  resetPanesForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  __resetAgentPaneStateForTest();
  installCompanionPanes();
  setPaneLayoutItemsForTest([threadItem('main'), threadItem('right')]);
});

afterEach(() => {
  __resetAgentPaneStateForTest();
  resetCompanionPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
});

describe('agent pane scope', () => {
  it('opens one companion seeded at the launch row, under the thread root', () => {
    const state = openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');

    expect(state?.scopeItemId).toBe('launch-1');
    expect(state?.breadcrumb).toEqual([
      { itemId: '', label: 'main' },
      { itemId: 'launch-1', label: 'code-review' },
    ]);
    expect(companionForSource('main', 'agent')?.paneId).toBe('agent-main');
    expect(getPaneLayoutItems().map((item) => item.paneId)).toEqual(['main', 'agent-main', 'right']);
  });

  it('a second open swaps the scope in place instead of stacking a pane', () => {
    openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    const swapped = openAgentCompanion('main', 'thread-1', 'launch-2', 'plan-review');

    expect(swapped?.scopeItemId).toBe('launch-2');
    // Opening from OUTSIDE restarts the trail: launch-2 is not a child of
    // launch-1, so it must not inherit its ancestry.
    expect(swapped?.breadcrumb.map((entry) => entry.label)).toEqual(['main', 'plan-review']);
    expect(getPaneLayoutItems().filter((item) => item.kind === 'agent')).toHaveLength(1);
  });

  it('refuses to open without a launch row or a thread', () => {
    expect(openAgentCompanion('main', 'thread-1', '', 'code-review')).toBeNull();
    expect(openAgentCompanion('main', '', 'launch-1', 'code-review')).toBeNull();
    expect(companionForSource('main', 'agent')).toBeNull();
  });

  it('pushes a breadcrumb hop when a child is opened from inside, and pops back', () => {
    const state = openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    state?.pushScope('launch-2', 'Angle B');
    state?.pushScope('launch-3', 'Angle C');

    expect(state?.scopeItemId).toBe('launch-3');
    expect(labels('main')).toEqual(['main', 'code-review', 'Angle B', 'Angle C']);

    state?.popTo(1);
    expect(state?.scopeItemId).toBe('launch-1');
    expect(labels('main')).toEqual(['main', 'code-review']);

    // The root hop is "back to the whole thread": the empty scope, which
    // the pane body answers by closing.
    state?.popTo(0);
    expect(state?.scopeItemId).toBe('');
    expect(labels('main')).toEqual(['main']);
  });

  it('re-pushing a node already on the trail pops to it rather than duplicating it', () => {
    const state = openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    state?.pushScope('launch-2', 'Angle B');
    state?.pushScope('launch-1', 'code-review');

    expect(state?.scopeItemId).toBe('launch-1');
    expect(labels('main')).toEqual(['main', 'code-review']);

    // Re-pushing the CURRENT scope changes nothing.
    state?.pushScope('launch-1', 'code-review');
    expect(labels('main')).toEqual(['main', 'code-review']);
  });

  it('ignores out-of-range and no-op breadcrumb clicks', () => {
    const state = openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    state?.popTo(1);
    state?.popTo(-1);
    state?.popTo(7);

    expect(state?.scopeItemId).toBe('launch-1');
    expect(labels('main')).toEqual(['main', 'code-review']);
  });

  it('keeps one state per source pane and replaces it when the thread changes', () => {
    openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    openAgentCompanion('right', 'thread-2', 'launch-9', 'other');

    expect(agentStateForPane('main', 'thread-1').scopeItemId).toBe('launch-1');
    expect(agentStateForPane('right', 'thread-2').scopeItemId).toBe('launch-9');

    // Same pane, different thread: a fresh state, never the departed
    // thread's trail.
    const replaced = agentStateForPane('main', 'thread-3');
    expect(replaced.threadId).toBe('thread-3');
    expect(replaced.scopeItemId).toBe('');
    expect(agentStateForPane('main', 'thread-3')).toBe(replaced);
  });

  it('drops the scope when the source pane is destroyed', () => {
    createPane('main');
    openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    expect(agentScopeForPane('main', 'thread-1')?.scopeItemId).toBe('launch-1');

    destroyPane('main');

    expect(agentScopeForPane('main', 'thread-1')).toBeNull();
    expect(companionForSource('main', 'agent')).toBeNull();
  });

  it('answers no persistable scope for an unscoped, foreign-thread, or unknown pane', () => {
    expect(agentScopeForPane('main', 'thread-1')).toBeNull();

    openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    expect(agentScopeForPane('main', 'thread-2')).toBeNull();

    agentStateForPane('main', 'thread-1').reset();
    expect(agentScopeForPane('main', 'thread-1')).toBeNull();
  });

  it('disposes only the expected thread when one is named', () => {
    openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');

    disposeAgentStateForPane('main', 'thread-other');
    expect(agentScopeForPane('main', 'thread-1')?.scopeItemId).toBe('launch-1');

    disposeAgentStateForPane('main', 'thread-1');
    expect(agentScopeForPane('main', 'thread-1')).toBeNull();
  });

  it('seeds a persisted trail back onto a pane', () => {
    const state = seedAgentStateForPane('main', 'thread-1', {
      scopeItemId: 'launch-3',
      breadcrumb: [
        { itemId: '', label: 'main' },
        { itemId: 'launch-1', label: 'code-review' },
        { itemId: 'launch-3', label: 'Angle B' },
      ],
    });

    expect(state?.scopeItemId).toBe('launch-3');
    expect(labels('main')).toEqual(['main', 'code-review', 'Angle B']);
    expect(seedAgentStateForPane('main', 'thread-1', { scopeItemId: '', breadcrumb: [] })).toBeNull();
  });

  it('retains an anchor on the open trail for the subagent-memory eviction guard', () => {
    // The thread pane's eviction policy consults this alongside card
    // expansion: rows under an anchor the reader is scoped to (or one
    // hop up the trail from) must not fold out of pane memory.
    expect(agentPaneScopeTrailHolds('main', 'thread-1', 'launch-1')).toBe(false);

    const state = openAgentCompanion('main', 'thread-1', 'launch-1', 'code-review');
    state?.pushScope('launch-3', 'Angle B');
    expect(agentPaneScopeTrailHolds('main', 'thread-1', 'launch-1')).toBe(true);
    expect(agentPaneScopeTrailHolds('main', 'thread-1', 'launch-3')).toBe(true);
    expect(agentPaneScopeTrailHolds('main', 'thread-1', 'launch-9')).toBe(false);
    // Foreign thread and foreign pane hold nothing.
    expect(agentPaneScopeTrailHolds('main', 'thread-2', 'launch-1')).toBe(false);
    expect(agentPaneScopeTrailHolds('right', 'thread-1', 'launch-1')).toBe(false);

    // Scope state can outlive a generic companion close — a closed pane
    // retains nothing, so eviction resumes.
    const companion = companionForSource('main', 'agent');
    expect(companion).not.toBeNull();
    closeCompanion(companion!.paneId);
    expect(agentPaneScopeTrailHolds('main', 'thread-1', 'launch-1')).toBe(false);
  });
});
