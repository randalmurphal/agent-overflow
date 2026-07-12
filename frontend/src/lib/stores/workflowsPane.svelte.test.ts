import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { addProjectLocal, resetProjectsForTest } from './projects.svelte';
import { getPaneLayoutItems, resetPaneLayoutForTest } from './paneLayout.svelte';
import { resetPanesForTest } from './panes.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Project } from '../types/models';
import {
  consumeWorkflowEscape,
  activateWorkflowsPane,
  beginWorkflowSweep,
  getWorkflowCurrentLevel,
  getWorkflowStack,
  loadWorkflowOverview,
  openWorkflowIntake,
  openWorkflowsPane,
  parsePersistedWorkflowsPaneState,
  pushWorkflowLevel,
  recordWorkflowReceipt,
  resetWorkflowsPane,
  restoreWorkflowsPaneState,
  setWorkflowArmedAction,
  setWorkflowsPanePersistenceHandler,
  workflowAllClearSummary,
} from './workflowsPane.svelte';
import type { WorkItem } from '../types/workflow';

function project(): Project {
  return {
    id: 'p', name: 'Project', path: '/tmp/p', sortPosition: 0,
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
});
