import { beforeEach, describe, expect, it } from 'vitest';
import { getToasts, removeToast } from './toast.svelte';
import { getWorkflowQueueState, resetWorkflowsPane } from './workflowsPane.svelte';
import {
  applyWorkflowErrorEvent,
  applyWorkflowQueueStateEvent,
  resetWorkflowEventStateForTest,
} from './eventsWorkflow';

describe('workflow event fan-out', () => {
  beforeEach(() => {
    resetWorkflowsPane();
    resetWorkflowEventStateForTest();
    for (const toast of getToasts()) removeToast(toast.id);
  });

  it('patches the queue header snapshot', () => {
    applyWorkflowQueueStateEvent({ active: false, globalConcurrency: 3, startsRemaining: 2 });
    expect(getWorkflowQueueState()).toEqual({ active: false, globalConcurrency: 3, startsRemaining: 2 });
  });

  it('deduplicates user-facing errors by item and message', () => {
    applyWorkflowErrorEvent({ itemId: 'a', error: 'Provider stopped' });
    applyWorkflowErrorEvent({ itemId: 'a', error: 'Provider stopped' });
    applyWorkflowErrorEvent({ itemId: 'b', error: 'Provider stopped' });
    expect(getToasts().map((toast) => toast.message)).toEqual(['Provider stopped', 'Provider stopped']);
  });
});
