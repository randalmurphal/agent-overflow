import { beforeEach, describe, expect, it } from 'vitest';
import { makeItem, makeThread } from '../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { createThreadDesignState } from './threadDesignState.svelte';

function designFence(payload: unknown): string {
  return [
    '```aoflow-design',
    JSON.stringify(payload),
    '```',
  ].join('\n');
}

describe('createThreadDesignState', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('projects assistant clarification and controls payloads onto design state', () => {
    const state = createThreadDesignState();
    const thread = makeThread({ id: 'design-1', mode: 'design' });

    state.applyAssistantPayloadsForItem(makeItem({
      id: 'assistant-1',
      threadId: 'design-1',
      kind: 'assistant_text',
      summary: [
        designFence({
          kind: 'clarification_request',
          requestId: 'clarify-1',
          questions: [{
            id: 'style',
            prompt: 'Pick a style',
            choices: [{ id: 'dense', label: 'Dense' }],
          }],
        }),
        designFence({
          kind: 'expose_controls',
          controls: [{ id: 'density', label: 'Density', min: 0, max: 1, value: 0.5 }],
        }),
      ].join('\n\n'),
    }), thread);

    expect(state.pendingClarification?.requestId).toBe('clarify-1');
    expect(state.pendingClarification?.threadId).toBe('design-1');
    expect(state.exposedControls.map((control) => control.id)).toEqual(['density']);
  });

  it('ignores duplicate payloads until reset gives the next thread a clean slate', () => {
    const state = createThreadDesignState();
    const firstThread = makeThread({ id: 'design-1', mode: 'design' });
    const secondThread = makeThread({ id: 'design-2', mode: 'design' });
    const firstPayload = designFence({
      kind: 'clarification_request',
      requestId: 'same-request',
      questions: [{
        id: 'first',
        prompt: 'First?',
        choices: [{ id: 'yes', label: 'Yes' }],
      }],
    });
    const secondPayload = designFence({
      kind: 'clarification_request',
      requestId: 'same-request',
      questions: [{
        id: 'second',
        prompt: 'Second?',
        choices: [{ id: 'yes', label: 'Yes' }],
      }],
    });

    state.applyAssistantPayloadsForItem(makeItem({
      threadId: firstThread.id,
      kind: 'assistant_text',
      summary: firstPayload,
    }), firstThread);
    state.applyAssistantPayloadsForItem(makeItem({
      threadId: firstThread.id,
      kind: 'assistant_text',
      summary: firstPayload,
    }), firstThread);
    expect(state.pendingClarification?.questions[0]?.id).toBe('first');

    state.reset();
    state.applyAssistantPayloadsForItem(makeItem({
      threadId: secondThread.id,
      kind: 'assistant_text',
      summary: secondPayload,
    }), secondThread);

    expect(state.pendingClarification?.threadId).toBe('design-2');
    expect(state.pendingClarification?.questions[0]?.id).toBe('second');
  });

  it('hydrates latest option paths and clears stale option state when no active set exists', async () => {
    const state = createThreadDesignState();
    const currentThread = makeThread({ id: 'design-1', mode: 'design' });
    setBindingMock('LatestDesignOptionSet', async () => ({
      setId: 'set-1',
      optionIds: ['one', 'two'],
    }));

    await state.applyDesignOptionsUpdate(() => currentThread, currentThread.id);

    expect(state.activeOptionSet).toEqual({
      setId: 'set-1',
      optionPaths: ['options/set-1/one', 'options/set-1/two'],
    });

    setBindingMock('LatestDesignOptionSet', async () => null);
    await state.applyDesignOptionsUpdate(() => currentThread, currentThread.id);

    expect(state.activeOptionSet).toBeNull();
  });

  it('ignores async option hydration when the current thread changes mid-request', async () => {
    const state = createThreadDesignState();
    let currentThread = makeThread({ id: 'design-1', mode: 'design' });
    let resolveLatest!: (value: unknown) => void;
    setBindingMock('LatestDesignOptionSet', () => new Promise((resolve) => {
      resolveLatest = resolve;
    }));

    const hydrating = state.applyDesignOptionsUpdate(() => currentThread, currentThread.id);
    currentThread = makeThread({ id: 'design-2', mode: 'design' });
    resolveLatest({ setId: 'set-1', optionIds: ['one'] });
    await hydrating;

    expect(state.activeOptionSet).toBeNull();
  });
});
