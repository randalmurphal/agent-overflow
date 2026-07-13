import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { addProjectLocal, resetProjectsForTest } from './projects.svelte';
import { getPaneLayoutItems, resetPaneLayoutForTest } from './paneLayout.svelte';
import { resetPanesForTest } from './panes.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Project } from '../types/models';
import {
  applyWorkflowItemState,
  consumeWorkflowEscape,
  activateWorkflowsPane,
  beginWorkflowSweep,
  getWorkflowCosts,
  getWorkflowCurrentLevel,
  getWorkflowDefinitions,
  getWorkflowDetail,
  getWorkflowItems,
  getWorkflowProjectFilter,
  getWorkflowSweep,
  getWorkflowStack,
  loadWorkflowOverview,
  loadWorkflowCurrentLevel,
  openWorkflowIntake,
  openWorkflowsPane,
  parsePersistedWorkflowsPaneState,
  pushWorkflowLevel,
  recordWorkflowReceipt,
  resetWorkflowsPane,
  restoreWorkflowsPaneState,
  setWorkflowArmedAction,
  setWorkflowProjectFilter,
  setWorkflowsPanePersistenceHandler,
  stepWorkflowSweep,
  workflowAllClearSummary,
} from './workflowsPane.svelte';
import type { WorkItem } from '../types/workflow';

function project(id = 'p'): Project {
  return {
    id, name: `Project ${id}`, path: `/tmp/${id}`, sortPosition: 0,
    createdAt: 1, updatedAt: 1, archived: false,
  };
}

function installMocks(): void {
  setBindingMock('WorkflowListItems', async () => []);
  setBindingMock('WorkflowListItemCosts', async () => ({}));
  setBindingMock('WorkflowListDefinitions', async () => ({
    baseBranch: 'main', predictedQueuePosition: 1,
    workflows: [{ id: 'wf', name: 'Workflow', scope: 'shared', phaseCount: 0, phases: [], inputs: [], defaultStepMode: false, valid: true, allBindingsAvailable: true }],
  }));
  setBindingMock('WorkflowGetItem', async (itemId: string) => ({
    item: { id: itemId, projectId: 'p', workflowId: 'wf', state: 'failed', createdAt: 1 },
    phases: [], artifacts: [], usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
  }));
}

function parked(id: string, state: 'failed' | 'needs-human', endedAt: number): WorkItem {
  return {
    id, projectId: 'p', workflowId: 'wf', workflowScope: 'shared', goal: id,
    state, reason: state === 'failed' ? 'agent-error' : 'gate', sortPosition: endedAt,
    stepMode: false, source: 'manual', createdAt: endedAt, endedAt,
  } as WorkItem;
}

describe('workflows pane store', () => {
  beforeEach(() => {
    resetWorkflowsPane();
    resetPaneLayoutForTest();
    resetPanesForTest();
    resetProjectsForTest();
    addProjectLocal(project());
    installMocks();
  });
  afterEach(() => {
    setWorkflowsPanePersistenceHandler(null);
    resetBindingMocks();
    vi.useRealTimers();
  });

  it('reuses the singleton and seeds a full deep-link stack', () => {
    openWorkflowsPane({ kind: 'overview' });
    openWorkflowsPane({
      kind: 'run', projectId: 'p', workflowId: 'wf', itemId: 'run', workflowLabel: 'Workflow', label: 'Run',
    });
    expect(getPaneLayoutItems().filter((item) => item.kind === 'workflows')).toHaveLength(1);
    expect(getWorkflowStack().map((level) => level.kind)).toEqual(['overview', 'workflow', 'run']);
  });

  it('pops stack levels and consumes transient state first', () => {
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    setWorkflowArmedAction('discard:run');
    expect(consumeWorkflowEscape()).toBe(true);
    expect(getWorkflowCurrentLevel().kind).toBe('workflow');
    openWorkflowIntake();
    expect(consumeWorkflowEscape()).toBe(true);
    expect(getWorkflowCurrentLevel().kind).toBe('workflow');
    expect(consumeWorkflowEscape()).toBe(true);
    expect(getWorkflowCurrentLevel().kind).toBe('overview');
    expect(consumeWorkflowEscape()).toBe(false);
  });

  it('drops dead restored entries from the top', async () => {
    setBindingMock('WorkflowListDefinitions', async () => ({ baseBranch: 'main', predictedQueuePosition: 1, workflows: [] }));
    await restoreWorkflowsPaneState({
      projectFilter: null,
      stack: [
        { kind: 'overview' },
        { kind: 'workflow', projectId: 'p', workflowId: 'missing', label: 'Missing' },
      ],
    });
    expect(getWorkflowStack()).toEqual([{ kind: 'overview' }]);
  });

  it('truncates a restored workflow when its target disappeared', async () => {
    setBindingMock('WorkflowListDefinitions', async () => { throw new Error('rpc: no rows in result set'); });
    await restoreWorkflowsPaneState({
      projectFilter: null,
      stack: [
        { kind: 'overview' },
        { kind: 'workflow', projectId: 'p', workflowId: 'missing', label: 'Missing' },
      ],
    });
    expect(getWorkflowStack()).toEqual([{ kind: 'overview' }]);
  });

  it('rejects malformed persisted stacks', () => {
    expect(parsePersistedWorkflowsPaneState({ stack: [{ kind: 'run' }] })).toBeNull();
    expect(parsePersistedWorkflowsPaneState({
      stack: [{ kind: 'overview' }, { kind: 'run', projectId: 'p', workflowId: 'wf', workflowLabel: 'W', itemId: 'i', label: 'I', sweep: false }],
    })).toBeNull();
    expect(parsePersistedWorkflowsPaneState({
      stack: [
        { kind: 'overview' },
        { kind: 'workflow', projectId: 'p', workflowId: 'one', label: 'One' },
        { kind: 'run', projectId: 'p', workflowId: 'two', workflowLabel: 'Two', itemId: 'i', label: 'I', sweep: false },
      ],
    })).toBeNull();
    expect(parsePersistedWorkflowsPaneState({ stack: [{ kind: 'overview' }], projectFilter: 'p' })).toEqual({
      stack: [{ kind: 'overview' }], projectFilter: 'p',
    });
  });

  it('persists durable stack and filter mutations immediately', () => {
    const persist = vi.fn();
    setWorkflowsPanePersistenceHandler(persist);
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    expect(persist).toHaveBeenCalledTimes(1);
    expect(consumeWorkflowEscape()).toBe(true);
    expect(persist).toHaveBeenCalledTimes(2);
  });

  it('replays item events captured during an overview fetch', async () => {
    let release!: (items: WorkItem[]) => void;
    let calls = 0;
    setBindingMock('WorkflowListItems', () => {
      calls += 1;
      if (calls === 1) return new Promise<WorkItem[]>((resolve) => { release = resolve; });
      return Promise.resolve([parked('run', 'needs-human', 1)]).then((rows) => rows.map((row) => ({ ...row, state: 'running', reason: '' }) as WorkItem));
    });
    activateWorkflowsPane();
    const loading = loadWorkflowOverview();
    await vi.waitFor(() => expect(release).toBeTypeOf('function'));
    applyWorkflowItemState({ itemId: 'run', projectId: 'p', from: 'needs-human', to: 'running' });
    release([parked('run', 'needs-human', 1)]);
    await loading;
    expect(getWorkflowItems()[0]).toMatchObject({ id: 'run', state: 'running', reason: '' });
  });

  it('isolates a new refresh generation from an abandoned overview fetch', async () => {
    let release!: (items: WorkItem[]) => void;
    setBindingMock('WorkflowListItems', () => new Promise<WorkItem[]>((resolve) => { release = resolve; }));
    activateWorkflowsPane();
    const abandoned = loadWorkflowOverview();
    await vi.waitFor(() => expect(release).toBeTypeOf('function'));

    resetWorkflowsPane();
    setBindingMock('WorkflowListItems', async () => [parked('current', 'failed', 1)]);
    activateWorkflowsPane();
    await loadWorkflowOverview();
    expect(getWorkflowItems().map((item) => item.id)).toEqual(['current']);

    release([parked('stale', 'failed', 1)]);
    await abandoned;
    expect(getWorkflowItems().map((item) => item.id)).toEqual(['current']);
  });

  it('authoritatively refreshes disposition fields after an item event', async () => {
    let authoritative = parked('run', 'needs-human', 1);
    setBindingMock('WorkflowListItems', async () => [authoritative]);
    setBindingMock('WorkflowGetItem', async () => ({
      item: authoritative,
      phases: [], artifacts: [], usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    activateWorkflowsPane();
    await loadWorkflowOverview();
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    pushWorkflowLevel({
      kind: 'run', projectId: 'p', workflowId: 'wf', workflowLabel: 'Workflow',
      itemId: 'run', label: 'run', sweep: false,
    });
    await vi.waitFor(() => expect(getWorkflowDetail()?.item.id).toBe('run'));
    authoritative = {
      ...authoritative,
      state: 'done',
      reason: '',
      disposition: '{"action":"merged","policy":"manual","at":2}',
    } as WorkItem;

    applyWorkflowItemState({ itemId: 'run', projectId: 'p', from: 'needs-human', to: 'done' });
    await vi.waitFor(() => expect(getWorkflowItems()[0]?.disposition).toContain('merged'));
    await vi.waitFor(() => expect(getWorkflowDetail()?.item.disposition).toContain('merged'));
  });

  it('refreshes only the affected project items and costs after an item event', async () => {
    addProjectLocal(project('other'));
    let pItem = parked('p-run', 'needs-human', 1);
    const otherItem = { ...parked('other-run', 'failed', 2), projectId: 'other' } as WorkItem;
    setBindingMock('WorkflowListItems', async (projectId: string) => (
      projectId === 'p' ? [pItem] : [otherItem]
    ));
    setBindingMock('WorkflowListItemCosts', async (projectId: string) => (
      projectId === 'p' ? { 'p-run': pItem.state === 'running' ? 2 : 1 } : { 'other-run': 3 }
    ));

    activateWorkflowsPane();
    await loadWorkflowOverview();
    expect(getWorkflowDefinitions()).toHaveLength(2);
    getBindingMock('WorkflowListItems')!.mockClear();
    getBindingMock('WorkflowListItemCosts')!.mockClear();
    getBindingMock('WorkflowListDefinitions')!.mockClear();

    pItem = { ...pItem, state: 'running', reason: '' } as WorkItem;
    applyWorkflowItemState({
      itemId: 'p-run', projectId: 'p', from: 'needs-human', to: 'running',
    });

    await vi.waitFor(() => expect(getWorkflowCosts()['p-run']).toBe(2));
    expect(getBindingMock('WorkflowListItems')!.mock.calls).toEqual([['p']]);
    expect(getBindingMock('WorkflowListItemCosts')!.mock.calls).toEqual([['p']]);
    expect(getBindingMock('WorkflowListDefinitions')).not.toHaveBeenCalled();
    expect(getWorkflowItems().map((item) => item.id).sort()).toEqual(['other-run', 'p-run']);
    expect(getWorkflowDefinitions()).toHaveLength(2);
  });

  it('keeps the overview filter while loading a deep-linked workflow outside it', async () => {
    addProjectLocal(project('other'));
    setBindingMock('WorkflowListItems', async (projectId: string) => [
      { ...parked(`${projectId}-run`, 'failed', 1), projectId },
    ]);
    activateWorkflowsPane();
    setWorkflowProjectFilter('p');
    await vi.waitFor(() => expect(getWorkflowItems().map((item) => item.id)).toEqual(['p-run']));

    openWorkflowsPane({ kind: 'workflow', projectId: 'other', workflowId: 'wf', label: 'Other workflow' });
    await vi.waitFor(() => expect(getWorkflowItems().map((item) => item.id).sort()).toEqual(['other-run', 'p-run']));
    expect(getWorkflowProjectFilter()).toBe('p');
    expect(getWorkflowCurrentLevel()).toMatchObject({ kind: 'workflow', projectId: 'other' });
  });

  it('abandons stale run loading when the current level changes during an await', async () => {
    let release!: (items: WorkItem[]) => void;
    let calls = 0;
    setBindingMock('WorkflowListItems', () => {
      calls += 1;
      if (calls === 1) return new Promise<WorkItem[]>((resolve) => { release = resolve; });
      return Promise.resolve([]);
    });
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    pushWorkflowLevel({
      kind: 'run', projectId: 'p', workflowId: 'wf', workflowLabel: 'Workflow',
      itemId: 'run', label: 'run', sweep: false,
    });
    activateWorkflowsPane();
    const loading = loadWorkflowCurrentLevel();
    await vi.waitFor(() => expect(release).toBeTypeOf('function'));
    expect(consumeWorkflowEscape()).toBe(true);
    release([]);
    await loading;
    expect(getBindingMock('WorkflowGetItem')).not.toHaveBeenCalled();
    expect(getWorkflowCurrentLevel().kind).toBe('workflow');
  });

  it('auto-advances a sweep after 650ms and ends with the session summary', async () => {
    vi.useFakeTimers();
    setBindingMock('WorkflowListItems', async () => [parked('first', 'needs-human', 1), parked('second', 'failed', 2)]);
    activateWorkflowsPane();
    await loadWorkflowOverview();
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    pushWorkflowLevel({
      kind: 'run', projectId: 'p', workflowId: 'wf', workflowLabel: 'Workflow',
      itemId: 'first', label: 'first', sweep: true,
    });
    beginWorkflowSweep('first');
    recordWorkflowReceipt({ itemId: 'first', kind: 'approved', message: 'Approved', costUsd: 1.25 });
    await vi.advanceTimersByTimeAsync(649);
    expect(getWorkflowCurrentLevel()).toMatchObject({ kind: 'run', itemId: 'first' });
    await vi.advanceTimersByTimeAsync(1);
    expect(getWorkflowCurrentLevel()).toMatchObject({ kind: 'run', itemId: 'second' });
    recordWorkflowReceipt({ itemId: 'second', kind: 'handed-off', message: 'Handed off', costUsd: 2.5 });
    await vi.advanceTimersByTimeAsync(650);
    expect(getWorkflowCurrentLevel()).toEqual({ kind: 'all-clear' });
    expect(workflowAllClearSummary()).toEqual({
      count: 2, costUsd: 3.75, byKind: { approved: 1, 'handed-off': 1 },
    });
  });

  it('cancels pending auto-advance when the sweep is stepped manually', async () => {
    vi.useFakeTimers();
    setBindingMock('WorkflowListItems', async () => [parked('first', 'needs-human', 1), parked('second', 'failed', 2)]);
    activateWorkflowsPane();
    await loadWorkflowOverview();
    pushWorkflowLevel({ kind: 'workflow', projectId: 'p', workflowId: 'wf', label: 'Workflow' });
    pushWorkflowLevel({
      kind: 'run', projectId: 'p', workflowId: 'wf', workflowLabel: 'Workflow',
      itemId: 'first', label: 'first', sweep: true,
    });
    beginWorkflowSweep('first');
    recordWorkflowReceipt({ itemId: 'first', kind: 'approved', message: 'Approved', costUsd: 0 });
    expect(stepWorkflowSweep(1, true)).toBe(true);
    await vi.runAllTicks();
    const callsAfterManualStep = getBindingMock('WorkflowGetItem')?.mock.calls.length ?? 0;
    await vi.advanceTimersByTimeAsync(650);
    expect(getBindingMock('WorkflowGetItem')).toHaveBeenCalledTimes(callsAfterManualStep);
  });

  it('clamps a stale sweep index when the sweep set shrinks', async () => {
    let rows = [parked('first', 'needs-human', 1), parked('second', 'failed', 2)];
    setBindingMock('WorkflowListItems', async () => rows);
    activateWorkflowsPane();
    await loadWorkflowOverview();
    beginWorkflowSweep('second');
    rows = [parked('first', 'needs-human', 1)];
    applyWorkflowItemState({ itemId: 'first', projectId: 'p', from: 'needs-human', to: 'needs-human', reason: 'gate' });
    await vi.waitFor(() => expect(getWorkflowItems()).toHaveLength(1));
    expect(getWorkflowSweep().index).toBe(0);
  });

  it('lands on all-clear when a sweep entry loads with an empty set', async () => {
    const disposed = {
      ...parked('disposed', 'needs-human', 1),
      state: 'done',
      reason: '',
      disposition: '{"action":"discarded","policy":"manual","at":1}',
    } as WorkItem;
    setBindingMock('WorkflowListItems', async () => [disposed]);
    setBindingMock('WorkflowGetItem', async () => ({
      item: disposed,
      phases: [], artifacts: [], usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    openWorkflowsPane({
      kind: 'sweep-at-run', projectId: 'p', workflowId: 'wf', itemId: 'disposed',
      workflowLabel: 'Workflow', label: 'Disposed',
    });
    await vi.waitFor(() => expect(getWorkflowCurrentLevel()).toEqual({ kind: 'all-clear' }));
  });
});
