import { beforeEach, describe, expect, it } from 'vitest';
import { getToasts, removeToast } from './toast.svelte';
import { applyWorkflowErrorEvent, resetWorkflowEventStateForTest } from './eventsWorkflow';

describe('workflow event fan-out', () => {
  beforeEach(() => {
    resetWorkflowEventStateForTest();
    for (const toast of getToasts()) removeToast(toast.id);
  });

  it('deduplicates user-facing errors by item and message', () => {
    applyWorkflowErrorEvent({ itemId: 'a', error: 'Provider stopped' });
    applyWorkflowErrorEvent({ itemId: 'a', error: 'Provider stopped' });
    applyWorkflowErrorEvent({ itemId: 'b', error: 'Provider stopped' });
    expect(getToasts().map((toast) => toast.message)).toEqual(['Provider stopped', 'Provider stopped']);
  });

  it('ignores events without a usable error message', () => {
    applyWorkflowErrorEvent({ itemId: 'a', error: '   ' });
    applyWorkflowErrorEvent({ itemId: 'a' } as never);
    expect(getToasts()).toHaveLength(0);
  });

  it('caps error text at the toast budget', () => {
    applyWorkflowErrorEvent({ itemId: 'a', error: 'x'.repeat(400) });
    expect(getToasts()[0]?.message).toHaveLength(240);
  });
});
