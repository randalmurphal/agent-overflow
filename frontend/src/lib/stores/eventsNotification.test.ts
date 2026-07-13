import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import type { Thread } from '../types/models';
import { getToasts, removeToast } from './toast.svelte';
import { getFocusedPaneOrNull, resetPanesForTest } from './panes.svelte';
import { resetPaneLayoutForTest } from './paneLayout.svelte';
import { getWorkflowCurrentLevel, resetWorkflowsPane } from './workflowsPane.svelte';
import {
  applyNotificationActivated,
  markNotificationHydrated,
  resetNotificationActivationForTest,
} from './eventsNotification';

function thread(id: string): Thread {
  return {
    id, title: id, provider: 'claude', workspacePath: '/tmp/p', projectPath: '/tmp/p',
    projectId: 'p', mode: 'workflow-triage', model: 'claude-sonnet-4-6',
    createdAt: 1, updatedAt: 1, archived: false,
  };
}

describe('workflow notification activation', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetPaneLayoutForTest();
    resetWorkflowsPane();
    resetNotificationActivationForTest();
    for (const toast of getToasts()) removeToast(toast.id);
    setBindingMock('WorkflowListItems', async () => []);
    setBindingMock('WorkflowListItemCosts', async () => ({}));
    setBindingMock('WorkflowListDefinitions', async () => ({
      baseBranch: 'main', predictedQueuePosition: 1, workflows: [],
    }));
  });

  afterEach(() => resetBindingMocks());

  it('opens a workflow item inside the sweep after hydration', async () => {
    setBindingMock('WorkflowListItems', async () => [{
      id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'needs-human',
      reason: 'gate', sortPosition: 0, createdAt: 1,
    }]);
    setBindingMock('WorkflowGetItem', async () => ({
      item: { id: 'run', projectId: 'p', workflowId: 'wf', goal: 'Run', state: 'needs-human' },
      phases: [], artifacts: [], usage: { inputTokens: 0, outputTokens: 0, totalTokens: 0, costUsd: 0 },
    }));
    applyNotificationActivated({ kind: 'workflow-item', workItemId: 'run' });
    await markNotificationHydrated();
    expect(getWorkflowCurrentLevel()).toMatchObject({ kind: 'run', itemId: 'run', sweep: true });
  });

  it('falls back to the overview with an error toast for a deleted run', async () => {
    setBindingMock('WorkflowGetItem', async () => { throw new Error('sql: no rows in result set'); });
    applyNotificationActivated({ kind: 'workflow-item', workItemId: 'gone' });
    await markNotificationHydrated();
    expect(getWorkflowCurrentLevel()).toEqual({ kind: 'overview' });
    expect(getToasts().some((toast) => toast.message.includes('no longer exists'))).toBe(true);
  });

  it('creates, reloads, and opens the project triage agent', async () => {
    const created = thread('triage');
    setBindingMock('WorkflowOpenTriageAgent', async () => created);
    setBindingMock('GetThread', async () => created);
    setBindingMock('ListRecentThreadItems', async () => ({ items: [], oldestTurnIndex: -1, hasMore: false }));
    setBindingMock('ListRecentTurns', async () => []);
    applyNotificationActivated({ kind: 'workflow-triage-agent', projectId: 'p' });
    await markNotificationHydrated();
    expect(getFocusedPaneOrNull()?.threadId).toBe('triage');
  });
});
