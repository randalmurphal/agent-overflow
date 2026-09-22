import { expect, it, vi } from 'vitest';
import { attachAsyncQuestions } from './asyncQuestions.svelte';
import { applyTransportGap } from './eventsTransportGap';
import { itemEventQueued, itemEventsSettled } from './itemEventSettlement';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';

it('reloads question state after a transport gap only after earlier replay settles', async () => {
  noteThread('question-gap', HOME_BACKEND);
  const list = setBindingMock('ListAsyncQuestions', vi.fn(async () => []));
  const held = attachAsyncQuestions('question-gap');
  try {
    await vi.waitFor(() => expect(held.current).toEqual([]));
    list.mockClear();
    itemEventQueued();
    applyTransportGap({ channel: 'provider:async_questions_changed', seq: 4 });
    expect(list).not.toHaveBeenCalled();
    itemEventsSettled(1);
    await vi.waitFor(() => expect(list).toHaveBeenCalledOnce());
  } finally { held.release(); }
});
