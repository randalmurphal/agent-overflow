import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { WorkItem } from '../../types/workflow';
import WorkflowsFooter from './WorkflowsFooter.svelte';
import { hydrateWorkflowAttention, resetWorkflowRunsForTest } from '../../stores/workflowRuns.svelte';
import {
  isWorkflowsOverlayOpen,
  resetWorkflowsOverlayForTest,
} from '../../stores/workflowsOverlay.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

function item(id: string, state: string, over: Partial<WorkItem> = {}): WorkItem {
  return { id, projectId: 'p', state, createdAt: 1, ...over } as WorkItem;
}

const MERGED = '{"action":"merged","policy":"manual","at":1}';

async function hydrate(items: WorkItem[]): Promise<void> {
  setBindingMock('WorkflowListUnresolvedItems', async () => items);
  await hydrateWorkflowAttention();
}

describe('WorkflowsFooter', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetAppStorageForTest();
    resetWorkflowsOverlayForTest();
    resetWorkflowRunsForTest();
  });

  afterEach(() => {
    resetBindingMocks();
    resetWorkflowsOverlayForTest();
    resetWorkflowRunsForTest();
  });

  it('shows no count at all when nothing needs a human (§9)', async () => {
    await hydrate([item('a', 'running'), item('b', 'done')]);
    const view = render(WorkflowsFooter);
    expect(view.getByTestId('sidebar-workflows-button')).toHaveTextContent('Workflows');
    expect(view.queryByTestId('sidebar-workflows-attention')).not.toBeInTheDocument();
  });

  it('counts unresolved root runs a human must unblock, amber only', async () => {
    await hydrate([
      item('gate', 'needs-human', { reason: 'gate' }),
      item('failed', 'failed'),
      item('awaiting-disposition', 'done'),
      item('resolved', 'needs-human', { disposition: MERGED }),
      item('child', 'needs-human', { parentItemId: 'gate' }),
    ]);
    const view = render(WorkflowsFooter);
    const badge = view.getByTestId('sidebar-workflows-attention');
    expect(badge).toHaveTextContent('2');
    expect(badge.className).toContain('text-warning');
  });

  it('toggles the overlay and reflects its open state', async () => {
    await hydrate([]);
    const view = render(WorkflowsFooter);
    const button = view.getByTestId('sidebar-workflows-button');
    expect(button).toHaveAttribute('aria-pressed', 'false');

    await fireEvent.click(button);
    expect(isWorkflowsOverlayOpen()).toBe(true);
    expect(button).toHaveAttribute('aria-pressed', 'true');

    await fireEvent.click(button);
    expect(isWorkflowsOverlayOpen()).toBe(false);
  });

  it('stays quiet when the backend has no workflow engine', async () => {
    setBindingMock('WorkflowListUnresolvedItems', async () => { throw new Error('workflow store unavailable'); });
    await hydrateWorkflowAttention();
    const view = render(WorkflowsFooter);
    expect(view.queryByTestId('sidebar-workflows-attention')).not.toBeInTheDocument();
  });
});
