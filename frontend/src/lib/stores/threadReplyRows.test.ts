import { beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { applyThreadUpdated } from './eventsThreadRows';
import { resetPanesForTest } from './panes.svelte';
import { applyThreadReasoningEffort } from './threadModelControls';
import { getThreadById, prependThread } from './threads.svelte';

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
});

// The effort RPC's reply was read before a model patch that landed during
// the call (the provider reporting its effective model): the reply applies
// its effort and keeps the patched model.
describe('a reply after a patch frame', () => {
  it('keeps the patched model and applies the rest of the reply', async () => {
    const thread = makeThread({ id: 'thread-model-patch', model: 'model-before', reasoningEffort: 'low' });
    prependThread(thread);
    const pane = await buildPane(thread);
    setBindingMock('UpdateThreadReasoningEffort', async () => {
      applyThreadUpdated({ action: 'patch', id: thread.id, model: 'model-patched' });
      return { ...thread, reasoningEffort: 'high' };
    });
    expect(await applyThreadReasoningEffort(pane, 'high')).toEqual({ ok: true });
    for (const row of [pane.thread, getThreadById(thread.id)]) {
      expect(row?.model).toBe('model-patched');
      expect(row?.reasoningEffort).toBe('high');
    }
  });
});
